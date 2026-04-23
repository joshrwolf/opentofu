// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"context"
	"fmt"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func (s *Solver) TargetBinding(ctx context.Context, module catalog.ModulePath, target *catalog.TargetDecl) (buildprovider.Binding, tfdiags.Diagnostics) {
	modulePkg, ok := s.catalog.Package(module.Declaration())
	if !ok {
		return buildprovider.Binding{}, sourcelessError("Missing package for target " + catalog.ResourceAddr(module, target.Addr.Kind, target.Addr.Type, target.Addr.Name, target.Addr.Key).String() + ".")
	}

	resolvedModule, resolvedLocal, diags := s.resolveProvider(module, modulePkg, target.ProviderLocal)
	if diags.HasErrors() {
		return buildprovider.Binding{}, diags
	}
	return s.Binding(ctx, resolvedModule, resolvedLocal)
}

func (s *Solver) resolveProvider(module catalog.ModulePath, pkg *catalog.Package, local addrs.LocalProviderConfig) (catalog.ModulePath, addrs.LocalProviderConfig, tfdiags.Diagnostics) {
	currentModule := module
	current := pkg
	resolved := local

	for current != nil {
		if _, ok := s.catalog.Provider(current.Module, resolved); ok {
			return currentModule, resolved, nil
		}

		parentModule := currentModule.Parent()
		if current.ParentImport == nil || current.Parent == nil {
			if resolved.Alias != "" {
				return catalog.ModulePath{}, addrs.LocalProviderConfig{}, sourcelessError("Aliased provider configuration " + resolved.StringCompact() + " could not be resolved from module " + currentModule.String() + ". Add a matching provider block or pass it with module providers = {}.")
			}
			return currentModule, resolved, nil
		}

		if parentLocal, ok := remappedProvider(current.ParentImport.ProviderPass, resolved); ok {
			current = current.Parent
			currentModule = parentModule
			resolved = parentLocal
			continue
		}

		if resolved.Alias == "" {
			current = current.Parent
			currentModule = parentModule
			continue
		}

		return catalog.ModulePath{}, addrs.LocalProviderConfig{}, sourcelessError("Aliased provider configuration " + resolved.StringCompact() + " was not passed from module " + currentModule.String() + " to its parent. Add a providers = {} remap for this alias.")
	}

	return module, local, nil
}

func remappedProvider(passes []catalog.ProviderPass, local addrs.LocalProviderConfig) (addrs.LocalProviderConfig, bool) {
	for _, pass := range passes {
		if pass.InChild == local {
			return pass.InParent, true
		}
	}
	return addrs.LocalProviderConfig{}, false
}

func (s *Solver) computeBinding(ctx context.Context, module catalog.ModulePath, local addrs.LocalProviderConfig) (buildprovider.Binding, error) {
	if err := s.ensureModuleInstance(ctx, module); err != nil && module.Len() != 0 {
		return buildprovider.Binding{}, err
	}

	declModule := module.Declaration()
	pkg, ok := s.catalog.Package(declModule)
	if !ok {
		return buildprovider.Binding{}, fmt.Errorf("unknown package %s for provider binding", module)
	}

	if decl, ok := s.catalog.Provider(declModule, local); ok {
		if !decl.ConfigValid {
			return buildprovider.Binding{}, fmt.Errorf("Provider configuration %s in module %s has invalid build configuration and cannot be lowered into a binding.", local.StringCompact(), module)
		}
		if s.source == nil {
			return buildprovider.Binding{}, fmt.Errorf("build solver has no source evaluator for provider facts")
		}
		if s.providers == nil {
			return buildprovider.Binding{}, fmt.Errorf("build solver has no provider session for provider bindings")
		}

		schemaSet, schemaDiags := s.providers.Schema(ctx, decl.Provider)
		if schemaDiags.HasErrors() {
			return buildprovider.Binding{}, wrapDiags(schemaDiags)
		}
		if schemaSet == nil {
			return buildprovider.Binding{}, fmt.Errorf("provider %s returned nil schema for binding in module %s", decl.Provider, module)
		}
		providerResult, providerDiags := s.source.EvalProvider(ctx, EvalProviderRequest{
			Module: module,
			Local:  local,
			Schema: &schemaSet.Provider,
		})
		if providerDiags.HasErrors() {
			return buildprovider.Binding{}, wrapDiags(providerDiags)
		}
		if providerResult.Deferred {
			return buildprovider.Binding{}, fmt.Errorf("provider configuration %s in module %s depends on deferred values and cannot be lowered into a binding yet", local.StringCompact(), module)
		}
		if providerResult.Value == cty.NilVal {
			return buildprovider.Binding{}, fmt.Errorf("provider configuration %s in module %s did not produce a configuration value", local.StringCompact(), module)
		}
		request, requestDiags := buildprovider.BuildConfigRequest(decl.Provider, &schemaSet.Provider, providerResult.Value, diagSourceRange(decl.Source))
		if requestDiags.HasErrors() {
			return buildprovider.Binding{}, wrapDiags(requestDiags)
		}
		return buildprovider.NewBinding(module, local, decl.Provider, buildprovider.BindingKey(module, local, decl.Provider, request.Key), request), nil
	}

	if local.Alias != "" {
		return buildprovider.Binding{}, fmt.Errorf("missing aliased provider configuration %s in module %s", local.StringCompact(), module)
	}

	providerAddr := pkg.ProviderForLocal(local.LocalName)
	return buildprovider.NewBinding(module, local, providerAddr, buildprovider.EmptyConfigKey(module, local, providerAddr), nil), nil
}

func (s *Solver) ProviderFunctionSpec(addr catalog.Addr, src catalog.SourceRef, binding buildprovider.Binding, call ProviderFunctionCall) engine.Spec {
	if call.Request == nil {
		return engine.Spec{}
	}

	key := digest.FromStrings(
		"build-provider-function-action-v1",
		binding.ConfigKey.String(),
		call.Request.Key.String(),
	)

	spec := engine.Spec{
		Key:       key,
		Name:      addr.String() + "::" + call.Ref.Function.String(),
		Class:     string(catalog.RunnerKindProviderFunction),
		Cacheable: buildprovider.FunctionRequestCacheable(*call.Request),
		Volatile:  false,
		Runner: engine.RunnerSpec{
			Kind: string(catalog.RunnerKindProviderFunction),
			Payload: buildprovider.FunctionPayload{
				Binding:  binding,
				TypeName: call.Request.Function.Function,
				Request:  *call.Request,
			},
		},
		Source: engineSourceRefFromDiags(call.Ref.Range, src),
	}
	return spec
}
