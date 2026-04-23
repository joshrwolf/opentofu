// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"bytes"
	"maps"
	"os"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type loweringContext struct {
	files map[string]*parsedFile
}

type parsedBlock struct {
	typ         string
	labels      []string
	rng         hcl.Range
	digest      digest.Digest
	digestReady bool
}

type parsedFile struct {
	src           []byte
	blocksByStart map[int]*parsedBlock
}

func LowerCatalog(cfg *configs.Config) (*catalog.Catalog, tfdiags.Diagnostics) {
	if cfg == nil {
		return nil, nil
	}

	ctx := &loweringContext{
		files: map[string]*parsedFile{},
	}
	root, diags := ctx.lowerPackage(cfg, nil, nil)
	return catalog.New(root), diags
}

func (ctx *loweringContext) lowerPackage(cfg *configs.Config, parent *catalog.Package, parentImport *catalog.Import) (*catalog.Package, tfdiags.Diagnostics) {
	module := modulePathFromConfig(cfg)
	imports, importDiags := lowerImports(module, cfg.Module)
	targets, targetDiags := ctx.lowerTargets(module, cfg.Module)
	outputs, outputDiags := lowerOutputs(module, cfg.Module)
	providers, providerDiags := ctx.lowerProviders(module, cfg.Module)
	name := "root"
	if len(cfg.Path) > 0 {
		name = cfg.Path[len(cfg.Path)-1]
	}

	var source catalog.SourceRef
	if cfg.Parent == nil {
		source = catalog.SourceRef{Filename: cfg.Module.SourceDir}
	} else {
		source = sourceRefFromHCL(cfg.CallRange)
	}

	pkg := &catalog.Package{
		Name:              name,
		Module:            module,
		Source:            source,
		ProviderTypes:     lowerProviderTypes(cfg.Module),
		Payload:           cfg,
		Parent:            parent,
		ParentImport:      parentImport,
		Children:          map[string]*catalog.Package{},
		Imports:           imports,
		Targets:           targets,
		Outputs:           outputs,
		Providers:         providers,
		RequiredProviders: lowerRequiredProviders(module, cfg.Module),
	}

	diags := tfdiags.Diagnostics{}
	diags = diags.Append(importDiags)
	diags = diags.Append(targetDiags)
	diags = diags.Append(outputDiags)
	diags = diags.Append(providerDiags)

	for _, name := range slices.Sorted(maps.Keys(cfg.Children)) {
		childCfg := cfg.Children[name]
		var importDecl *catalog.Import
		for i := range pkg.Imports {
			if pkg.Imports[i].Name == name {
				importDecl = &pkg.Imports[i]
				break
			}
		}
		childPkg, childDiags := ctx.lowerPackage(childCfg, pkg, importDecl)
		diags = diags.Append(childDiags)
		pkg.Children[name] = childPkg
	}

	return pkg, diags
}

func lowerImports(module catalog.ModulePath, mod *configs.Module) ([]catalog.Import, tfdiags.Diagnostics) {
	if mod == nil {
		return nil, nil
	}

	sortedNames := slices.Sorted(maps.Keys(mod.ModuleCalls))

	ret := make([]catalog.Import, 0, len(sortedNames))
	var diags tfdiags.Diagnostics
	for _, name := range sortedNames {
		call := mod.ModuleCalls[name]
		explicitDeps, depDiags := dependencyAddrs(module, call.DependsOn)
		diags = diags.Append(depDiags)
		ret = append(ret, catalog.Import{
			Name:         name,
			Module:       module,
			TargetModule: module.Child(name, catalog.NoKey()),
			Source:       sourceRefFromHCL(call.DeclRange),
			ExplicitDeps: explicitDeps,
			ProviderPass: lowerProviderPasses(call.Providers),
			Payload:      call,
		})
	}
	return ret, diags
}

func (ctx *loweringContext) lowerTargets(module catalog.ModulePath, mod *configs.Module) ([]*catalog.TargetDecl, tfdiags.Diagnostics) {
	if mod == nil {
		return nil, nil
	}

	var ret []*catalog.TargetDecl
	var diags tfdiags.Diagnostics
	appendResources := func(kind catalog.TargetKind, resources map[string]*configs.Resource) {
		for _, name := range slices.Sorted(maps.Keys(resources)) {
			resource := resources[name]
			explicitDeps, depDiags := dependencyAddrs(module, resource.DependsOn)
			diags = diags.Append(depDiags)
			configKey, configDiags := ctx.targetConfigKey(resource)
			diags = diags.Append(configDiags)
			ret = append(ret, &catalog.TargetDecl{
				Addr:          catalog.ResourceAddr(module, kind, resource.Type, resource.Name, catalog.NoKey()),
				Driver:        "hcl",
				Source:        sourceRefFromHCL(resource.DeclRange),
				ConfigKey:     configKey,
				ConfigValid:   !configDiags.HasErrors(),
				ExplicitDeps:  explicitDeps,
				ProviderLocal: resource.ProviderConfigAddr(),
				Payload:       resource,
				Runner:        builtinRunTargetSpec(resource),
			})
		}
	}

	appendResources(catalog.TargetKindResource, mod.ManagedResources)
	appendResources(catalog.TargetKindData, mod.DataResources)

	for _, name := range slices.Sorted(maps.Keys(mod.Runs)) {
		run := mod.Runs[name]
		configKey, configDiags := ctx.runConfigKey(run)
		diags = diags.Append(configDiags)
		explicitDeps, depDiags := dependencyAddrs(module, run.DependsOn)
		diags = diags.Append(depDiags)
		ret = append(ret, &catalog.TargetDecl{
			Addr:         catalog.RunAddr(module, run.Name, catalog.NoKey()),
			Driver:       "hcl",
			Source:       sourceRefFromHCL(run.DeclRange),
			ConfigKey:    configKey,
			ConfigValid:  !configDiags.HasErrors(),
			ExplicitDeps: explicitDeps,
			Payload:      run,
			Runner: &catalog.RunnerSpec{
				Kind:     catalog.RunnerKindBuiltinRun,
				TypeName: "run",
			},
		})
	}
	return ret, diags
}

func lowerOutputs(module catalog.ModulePath, mod *configs.Module) ([]*catalog.OutputDecl, tfdiags.Diagnostics) {
	if mod == nil {
		return nil, nil
	}

	sortedNames := slices.Sorted(maps.Keys(mod.Outputs))

	ret := make([]*catalog.OutputDecl, 0, len(sortedNames))
	var diags tfdiags.Diagnostics
	for _, name := range sortedNames {
		output := mod.Outputs[name]
		explicitDeps, depDiags := dependencyAddrs(module, output.DependsOn)
		diags = diags.Append(depDiags)
		ret = append(ret, &catalog.OutputDecl{
			Addr:         catalog.OutputAddr(module, output.Name),
			Source:       sourceRefFromHCL(output.DeclRange),
			ExplicitDeps: explicitDeps,
			Payload:      output,
		})
	}
	return ret, diags
}

func (ctx *loweringContext) lowerProviders(module catalog.ModulePath, mod *configs.Module) ([]*catalog.ProviderDecl, tfdiags.Diagnostics) {
	if mod == nil {
		return nil, nil
	}

	sortedKeys := slices.Sorted(maps.Keys(mod.ProviderConfigs))

	ret := make([]*catalog.ProviderDecl, 0, len(sortedKeys))
	var diags tfdiags.Diagnostics
	for _, key := range sortedKeys {
		providerConfig := mod.ProviderConfigs[key]
		local := providerConfig.Addr()
		providerAddr := mod.ProviderForLocalConfig(local)
		configKey, keyDiags := ctx.providerConfigKey(module, local, providerAddr, providerConfig)
		diags = diags.Append(keyDiags)
		ret = append(ret, &catalog.ProviderDecl{
			Module:      module,
			Local:       local,
			Provider:    providerAddr,
			Source:      sourceRefFromHCL(providerConfig.DeclRange),
			ConfigKey:   configKey,
			ConfigValid: !keyDiags.HasErrors(),
			Payload:     providerConfig,
		})
	}
	return ret, diags
}

func lowerRequiredProviders(module catalog.ModulePath, mod *configs.Module) []*catalog.RequiredProviderDecl {
	if mod == nil || mod.ProviderRequirements == nil {
		return nil
	}

	sortedNames := slices.Sorted(maps.Keys(mod.ProviderRequirements.RequiredProviders))

	ret := make([]*catalog.RequiredProviderDecl, 0, len(sortedNames))
	for _, name := range sortedNames {
		required := mod.ProviderRequirements.RequiredProviders[name]
		ret = append(ret, &catalog.RequiredProviderDecl{
			Module:    module,
			LocalName: name,
			Provider:  required.Type,
			Aliases:   slices.Clone(required.Aliases),
			Source:    sourceRefFromHCL(required.DeclRange),
			Payload:   required,
		})
	}
	return ret
}

func lowerProviderPasses(passes []configs.PassedProviderConfig) []catalog.ProviderPass {
	if len(passes) == 0 {
		return nil
	}

	ret := make([]catalog.ProviderPass, 0, len(passes))
	for _, pass := range passes {
		ret = append(ret, catalog.ProviderPass{
			InChild:  addrs.LocalProviderConfig{LocalName: pass.InChild.Name, Alias: pass.InChild.Alias},
			InParent: addrs.LocalProviderConfig{LocalName: pass.InParent.Name, Alias: pass.InParent.Alias},
		})
	}
	return ret
}

func dependencyAddrs(module catalog.ModulePath, deps []hcl.Traversal) ([]catalog.Addr, tfdiags.Diagnostics) {
	if len(deps) == 0 {
		return nil, nil
	}

	ret := make([]catalog.Addr, 0, len(deps))
	var diags tfdiags.Diagnostics
	for _, traversal := range deps {
		ref, refDiags := addrs.ParseRef(traversal)
		diags = diags.Append(refDiags)
		if refDiags.HasErrors() || ref == nil {
			continue
		}
		if len(ref.Remaining) != 0 {
			diags = diags.Append(unsupportedDependencyDiagnostic(ref, "Build-mode depends_on references must target a module, resource, data source, or output directly, without trailing attribute traversals."))
			continue
		}

		addr, ok := catalogAddrForReference(module, ref.Subject)
		if !ok {
			diags = diags.Append(unsupportedDependencyDiagnostic(ref, "Build-mode depends_on supports only module, resource, data source, and output references."))
			continue
		}

		ret = append(ret, addr)
	}
	return ret, diags
}

func catalogAddrForReference(module catalog.ModulePath, subject any) (catalog.Addr, bool) {
	switch subject := subject.(type) {
	case addrs.Resource:
		kind, ok := targetKindForMode(subject.Mode)
		if !ok {
			return catalog.Addr{}, false
		}
		return catalog.ResourceAddr(module, kind, subject.Type, subject.Name, catalog.NoKey()), true
	case addrs.ResourceInstance:
		kind, ok := targetKindForMode(subject.Resource.Mode)
		if !ok {
			return catalog.Addr{}, false
		}
		return catalog.ResourceAddr(module, kind, subject.Resource.Type, subject.Resource.Name, keyFromAddrs(subject.Key)), true
	case addrs.ModuleCall:
		return catalog.ModuleAddr(module.Child(subject.Name, catalog.NoKey())), true
	case addrs.ModuleCallInstance:
		return catalog.ModuleAddr(module.Child(subject.Call.Name, keyFromAddrs(subject.Key))), true
	case addrs.ModuleCallOutput:
		return catalog.OutputAddr(module.Child(subject.Call.Name, catalog.NoKey()), subject.Name), true
	case addrs.ModuleCallInstanceOutput:
		return catalog.OutputAddr(module.Child(subject.Call.Call.Name, keyFromAddrs(subject.Call.Key)), subject.Name), true
	case addrs.Run:
		return catalog.RunAddr(module, subject.Name, catalog.NoKey()), true
	case addrs.OutputValue:
		return catalog.OutputAddr(module, subject.Name), true
	default:
		return catalog.Addr{}, false
	}
}

func targetKindForMode(mode addrs.ResourceMode) (catalog.TargetKind, bool) {
	switch mode {
	case addrs.ManagedResourceMode:
		return catalog.TargetKindResource, true
	case addrs.DataResourceMode:
		return catalog.TargetKindData, true
	default:
		return "", false
	}
}

func modulePathFromConfig(cfg *configs.Config) catalog.ModulePath {
	path := catalog.RootModule()
	if cfg == nil {
		return path
	}
	for _, step := range cfg.Path {
		path = path.Child(step, catalog.NoKey())
	}
	return path
}

func keyFromAddrs(key addrs.InstanceKey) catalog.Key {
	switch key := key.(type) {
	case addrs.IntKey:
		return catalog.IntKey(int(key))
	case addrs.StringKey:
		return catalog.StringKey(string(key))
	default:
		return catalog.NoKey()
	}
}

func lowerProviderTypes(mod *configs.Module) map[string]addrs.Provider {
	if mod == nil || mod.ProviderRequirements == nil {
		return nil
	}

	ret := make(map[string]addrs.Provider, len(mod.ProviderRequirements.RequiredProviders))
	for name, required := range mod.ProviderRequirements.RequiredProviders {
		ret[name] = required.Type
	}
	return ret
}

func (ctx *loweringContext) providerConfigKey(module catalog.ModulePath, local addrs.LocalProviderConfig, provider addrs.Provider, providerConfig *configs.Provider) (digest.Digest, tfdiags.Diagnostics) {
	configDigest, diags := ctx.blockDigest(providerConfig.DeclRange, "provider", []string{providerConfig.Name})
	return buildprovider.BindingKey(module, local, provider, configDigest), diags
}

func (ctx *loweringContext) targetConfigKey(resource *configs.Resource) (digest.Digest, tfdiags.Diagnostics) {
	if resource == nil {
		return digest.Digest{}, nil
	}

	blockType := "resource"
	if resource.Mode == addrs.DataResourceMode {
		blockType = "data"
	}

	return ctx.blockDigest(resource.DeclRange, blockType, []string{resource.Type, resource.Name})
}

func (ctx *loweringContext) runConfigKey(run *configs.Run) (digest.Digest, tfdiags.Diagnostics) {
	if run == nil {
		return digest.Digest{}, nil
	}
	return ctx.blockDigest(run.DeclRange, "run", []string{run.Name})
}

func (ctx *loweringContext) blockDigest(declRange hcl.Range, blockType string, labels []string) (digest.Digest, tfdiags.Diagnostics) {
	file, diags := ctx.parsedFile(declRange.Filename)
	if diags.HasErrors() {
		return digest.Digest{}, diags
	}

	block := file.blocksByStart[declRange.Start.Byte]
	if block == nil || block.typ != blockType || !slices.Equal(block.labels, labels) {
		return digest.Digest{}, tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to locate build declaration",
			Detail:   "OpenTofu could not canonicalize this declaration for build mode.",
			Subject:  declRange.Ptr(),
		})
	}
	if block.digestReady {
		return block.digest, nil
	}

	blockSrc := file.src[block.rng.Start.Byte:block.rng.End.Byte]
	blockDigest, digestDiags := syntacticConfigDigest(blockSrc, declRange.Filename)
	if digestDiags.HasErrors() {
		return digest.Digest{}, digestDiags
	}

	block.digest = blockDigest
	block.digestReady = true
	return blockDigest, digestDiags
}

func (ctx *loweringContext) parsedFile(filename string) (*parsedFile, tfdiags.Diagnostics) {
	if existing := ctx.files[filename]; existing != nil {
		return existing, nil
	}

	src, err := os.ReadFile(filename)
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to read build source",
			Detail:   err.Error(),
			Subject:  &hcl.Range{Filename: filename},
		})
	}

	file, parseDiags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
	diags := tfdiags.Diagnostics{}.Append(parseDiags)
	if parseDiags.HasErrors() {
		return nil, diags
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok || body == nil {
		return nil, diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Failed to inspect build source",
			Detail:   "OpenTofu could not analyze this source file for build mode.",
			Subject:  &hcl.Range{Filename: filename},
		})
	}

	parsed := &parsedFile{
		src:           src,
		blocksByStart: make(map[int]*parsedBlock, len(body.Blocks)),
	}
	for _, block := range body.Blocks {
		parsed.blocksByStart[block.DefRange().Start.Byte] = &parsedBlock{
			typ:    block.Type,
			labels: slices.Clone(block.Labels),
			rng:    block.Range(),
		}
	}
	ctx.files[filename] = parsed
	return parsed, diags
}

// syntacticConfigDigest produces the v1 config-key digest for a declaration.
// It is intentionally syntactic rather than semantic: it strips layout-only
// tokens and comments, but preserves all other token spellings. Catalog and
// solve depend only on the resulting digest value so this boundary can be
// replaced later with a semantic canonicalizer without reshaping their APIs.
func syntacticConfigDigest(src []byte, filename string) (digest.Digest, tfdiags.Diagnostics) {
	tokens, diags := hclsyntax.LexConfig(src, filename, hcl.InitialPos)
	tfDiags := tfdiags.Diagnostics{}.Append(diags)
	if diags.HasErrors() {
		return digest.Digest{}, tfDiags
	}

	var buf bytes.Buffer
	for _, token := range tokens {
		switch token.Type {
		case hclsyntax.TokenComment, hclsyntax.TokenNewline, hclsyntax.TokenTabs, hclsyntax.TokenEOF:
			continue
		}
		buf.WriteRune(rune(token.Type))
		buf.WriteByte(0)
		buf.Write(token.Bytes)
		buf.WriteByte(0)
	}

	return digest.FromBytes(buf.Bytes()), tfDiags
}

func unsupportedDependencyDiagnostic(ref *addrs.Reference, detail string) tfdiags.Diagnostics {
	return tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "Unsupported build depends_on reference",
		Detail:   detail,
		Subject:  ref.SourceRange.ToHCL().Ptr(),
	})
}

func sourceRefFromHCL(rng hcl.Range) catalog.SourceRef {
	return catalog.SourceRef{
		Filename: rng.Filename,
		Start: catalog.SourcePos{
			Line:   rng.Start.Line,
			Column: rng.Start.Column,
			Byte:   rng.Start.Byte,
		},
		End: catalog.SourcePos{
			Line:   rng.End.Line,
			Column: rng.End.Column,
			Byte:   rng.End.Byte,
		},
	}
}

