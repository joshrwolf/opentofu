// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"cmp"
	"context"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/lang/blocktoattr"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func (l *Loaded) EvalTarget(ctx context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
	if l == nil || l.Catalog == nil || l.Config == nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"HCL build source is not initialized.",
		))
	}
	if !req.Addr.Actionable() {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"EvalTarget requires a resource, data, or run target address.",
		))
	}

	declAddr := req.Addr
	declAddr.Module = declAddr.Module.Declaration()
	declAddr.Key = catalog.NoKey()
	targetDecl, ok := l.Catalog.Target(declAddr)
	if !ok {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Unknown target "+declAddr.String()+".",
		))
	}

	var declRange hcl.Range
	switch payload := targetDecl.Payload.(type) {
	case *configs.Resource:
		if payload == nil {
			return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Target "+declAddr.String()+" is not backed by an HCL resource declaration.",
			))
		}
		declRange = payload.DeclRange
	case *configs.Run:
		if payload == nil {
			return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Target "+declAddr.String()+" is not backed by an HCL run declaration.",
			))
		}
		declRange = payload.DeclRange
	default:
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Target "+declAddr.String()+" is not backed by a supported HCL build declaration.",
		))
	}

	file, syntaxBlock := l.syntaxBlockForRange(declRange)
	if file == nil || syntaxBlock == nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Target "+declAddr.String()+" does not have an HCL syntax block available for build analysis.",
		))
	}

	moduleCfg := configForModulePath(l.Config, req.Addr.Module)
	if moduleCfg == nil || moduleCfg.Module == nil || moduleCfg.Module.StaticEvaluator == nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Module "+req.Addr.Module.String()+" does not have an HCL static evaluator.",
		))
	}

	var requestLowerer *providerRequestLowerer
	scope, scopeErr := l.moduleScope(ctx, req.Addr.Module)
	if scopeErr != nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Failed to prepare scope for "+req.Addr.Module.String()+": "+scopeErr.Error(),
		))
	}
	moduleOpts := scope.Options()
	if req.Runtime != nil {
		moduleOpts.Runtime = runtimeLookupForModule(req.Addr.Module, req.Runtime)
	}
	ident := configs.StaticIdentifier{
		Module:    moduleCfg.Path,
		Subject:   req.Addr.String(),
		DeclRange: declRange,
	}

	switch payload := targetDecl.Payload.(type) {
	case *configs.Resource:
		resource := payload
		evalOptions, optionDiags := l.targetEvalOptions(ctx, req.Addr, resource.DeclRange, resource.Count, resource.ForEach, resource.Enabled, moduleOpts)
		if optionDiags.HasErrors() {
			return EvalResult{}, optionDiags
		}
		evalOptions.Runtime = moduleOpts.Runtime
		requestLowerer = newProviderRequestLowerer(ctx, moduleCfg.Module.StaticEvaluator, ident, evalOptions)

		analysis := newTargetAnalysis(req.Addr.Module, req.Addr.Key, moduleCfg.Module, l.Sources(), requestLowerer)
		analysis.analyzeBody(file, syntaxBlock.Body, true)
		analysis.analyzeExpr(resource.Count)
		analysis.analyzeExpr(resource.ForEach)
		analysis.analyzeExpr(resource.Enabled)
		for _, rule := range resource.Preconditions {
			if rule == nil {
				continue
			}
			analysis.analyzeExpr(rule.Condition)
			analysis.analyzeExpr(rule.ErrorMessage)
		}
		if isNullResourceCompatibilityTarget(resource) {
			analyzeNullResourceCompatibilityConfig(analysis, resource)
		}

		result := analysis.result()
		result.Digest = targetDecl.ConfigKey
		if isBuiltinRunCompatibilityTarget(targetDecl) {
			payload, payloadDeferred, payloadDiags := l.lowerBuiltinRunPayload(ctx, req.Addr, targetDecl, resource, req.Runtime, &moduleOpts)
			analysis.diags = analysis.diags.Append(payloadDiags)
			if payloadDiags.HasErrors() {
				return result, analysis.diags
			}
			result.Deferred = payloadDeferred
			if !payloadDeferred {
				result.Payload = &payload
			}
			return result, analysis.diags
		}
		if req.Schema != nil {
			configValue, configDeferred, configDiags := l.lowerTargetConfig(ctx, req.Addr, resource, req.Schema, req.Runtime, &moduleOpts)
			analysis.diags = analysis.diags.Append(configDiags)
			if configDiags.HasErrors() {
				return result, analysis.diags
			}
			result.Deferred = configDeferred
			if configValue != cty.NilVal {
				result.Value = configValue
				result.Known = true
			}
		}
		return result, analysis.diags
	case *configs.Run:
		evalOptions, optionDiags := l.targetEvalOptions(ctx, req.Addr, payload.DeclRange, payload.Count, payload.ForEach, payload.Enabled, moduleOpts)
		if optionDiags.HasErrors() {
			return EvalResult{}, optionDiags
		}
		evalOptions.Runtime = moduleOpts.Runtime
		requestLowerer = newProviderRequestLowerer(ctx, moduleCfg.Module.StaticEvaluator, ident, evalOptions)

		analysis := newTargetAnalysis(req.Addr.Module, req.Addr.Key, moduleCfg.Module, l.Sources(), requestLowerer)
		for _, attr := range bodyAttributes(syntaxBlock.Body, "depends_on") {
			analysis.analyzeExpr(attr.Expr)
		}
		analysis.analyzeExpr(payload.Count)
		analysis.analyzeExpr(payload.ForEach)
		analysis.analyzeExpr(payload.Enabled)

		result := analysis.result()
		result.Digest = targetDecl.ConfigKey
		querySafety := buildprovider.DefaultQuerySafetyRegistry()
		for _, call := range result.ProviderFunctionCalls {
			if querySafety.IsQuerySafe(call.Ref.Function) {
				continue
			}
			result.Deferred = true
			return result, analysis.diags
		}
		runPayload, payloadDeferred, payloadDiags := l.lowerNativeRunPayload(ctx, req.Addr, payload, req.Runtime, &moduleOpts)
		analysis.diags = analysis.diags.Append(payloadDiags)
		if payloadDiags.HasErrors() {
			return result, analysis.diags
		}
		result.Deferred = payloadDeferred
		if !payloadDeferred {
			result.Payload = &runPayload
		}
		return result, analysis.diags
	default:
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Target "+declAddr.String()+" is not backed by a supported HCL build declaration.",
		))
	}
}

func (l *Loaded) lowerTargetConfig(ctx context.Context, addr catalog.Addr, resource *configs.Resource, schema *providers.Schema, runtime RuntimeResolver, moduleOptions *configs.StaticEvalOptions) (value cty.Value, deferred bool, diags tfdiags.Diagnostics) {
	if resource == nil || schema == nil || schema.Block == nil {
		return cty.NilVal, false, nil
	}

	dec, decDiags := l.newTargetDecoder(ctx, addr, resource.DeclRange, resource.Count, resource.ForEach, resource.Enabled, runtime, moduleOptions)
	if decDiags.HasErrors() {
		return cty.NilVal, false, decDiags
	}

	configBody := blocktoattr.FixUpBlockAttrs(resource.Config, schema.Block)
	value, deferred, diags = dec.decode(ctx, configBody, schema.Block, addr.String(), resource.DeclRange)
	if deferred || diags.HasErrors() {
		return cty.NilVal, deferred, diags
	}
	if !value.IsWhollyKnown() {
		return cty.NilVal, true, nil
	}
	return value, false, nil
}

type targetAnalysis struct {
	module      catalog.ModulePath
	instanceKey catalog.Key
	moduleCfg   *configs.Module
	sources     map[string]*hcl.File

	refs              []catalog.Addr
	refSeen           map[catalog.AddrKey]struct{}
	localSeen         map[string]struct{}
	providerFunctions []ProviderFunctionRef
	providerSeen      map[ProviderFunctionRef]struct{}
	providerCalls     []ProviderFunctionCall
	providerCallSeen  map[digest.Digest]struct{}
	requestLowerer    *providerRequestLowerer
	diags             tfdiags.Diagnostics
}

type providerRequestLowerer struct {
	ctx            context.Context
	evaluator      *configs.StaticEvaluator
	ident          configs.StaticIdentifier
	options        configs.StaticEvalOptions
	queryFunctions *buildprovider.QueryFunctionResolver
	querySafety    *buildprovider.QuerySafetyRegistry
}

func newProviderRequestLowerer(ctx context.Context, evaluator *configs.StaticEvaluator, ident configs.StaticIdentifier, options configs.StaticEvalOptions) *providerRequestLowerer {
	if evaluator == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &providerRequestLowerer{
		ctx:         ctx,
		evaluator:   evaluator,
		ident:       ident,
		options:     options,
		querySafety: buildprovider.DefaultQuerySafetyRegistry(),
	}
}

func newTargetAnalysis(module catalog.ModulePath, instanceKey catalog.Key, moduleCfg *configs.Module, sources map[string]*hcl.File, requestLowerer *providerRequestLowerer) *targetAnalysis {
	return &targetAnalysis{
		module:           module,
		instanceKey:      instanceKey,
		moduleCfg:        moduleCfg,
		sources:          sources,
		refSeen:          make(map[catalog.AddrKey]struct{}),
		localSeen:        make(map[string]struct{}),
		providerSeen:     make(map[ProviderFunctionRef]struct{}),
		providerCallSeen: make(map[digest.Digest]struct{}),
		requestLowerer:   requestLowerer,
	}
}

func (a *targetAnalysis) result() EvalResult {
	return EvalResult{
		Refs:                  a.refs,
		ProviderFunctionCalls: a.providerCalls,
	}
}

func (a *targetAnalysis) analyzeBody(file *hcl.File, body *hclsyntax.Body, topLevel bool) {
	if file == nil || body == nil {
		return
	}

	attrs := make([]*hclsyntax.Attribute, 0, len(body.Attributes))
	for _, attr := range body.Attributes {
		attrs = append(attrs, attr)
	}
	slices.SortFunc(attrs, func(a, b *hclsyntax.Attribute) int {
		return cmp.Compare(a.Range().Start.Byte, b.Range().Start.Byte)
	})
	for _, attr := range attrs {
		if topLevel && skipTopLevelTargetAttr(attr.Name) {
			continue
		}
		a.analyzeExpr(attr.Expr)
	}

	for _, block := range body.Blocks {
		if topLevel && skipTopLevelTargetBlock(block.Type) {
			continue
		}
		a.analyzeBody(file, block.Body, false)
	}
}

func (a *targetAnalysis) analyzeExpr(expr hcl.Expression) {
	if expr == nil {
		return
	}

	refs, refDiags := lang.ReferencesInExpr(addrs.ParseRef, expr)
	a.diags = a.diags.Append(refDiags)
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		if fn, ok := ref.Subject.(addrs.ProviderFunction); ok {
			a.appendProviderFunction(ProviderFunctionRef{
				Function: fn,
				Range:    ref.SourceRange,
			})
			continue
		}
		if local, ok := ref.Subject.(addrs.LocalValue); ok {
			a.analyzeLocal(local.Name)
			continue
		}

		addr, ok := catalogAddrForReference(a.module, ref.Subject)
		if !ok {
			continue
		}
		if _, ok := a.refSeen[addr.Identity()]; ok {
			continue
		}
		a.refSeen[addr.Identity()] = struct{}{}
		a.refs = append(a.refs, addr)
	}

	node, ok := expr.(hclsyntax.Node)
	if !ok {
		return
	}

	a.diags = a.diags.Append(hclsyntax.Walk(node, targetFunctionWalker{
		onFunction: func(call *hclsyntax.FunctionCallExpr) {
			fn, ok := providerFunctionFromCall(call)
			if !ok {
				return
			}

			ref := ProviderFunctionRef{
				Function: fn,
				Range:    tfdiags.SourceRangeFromHCL(call.NameRange),
			}
			a.appendProviderFunction(ref)

			repetitionSensitive := false
			callRefs, _ := lang.ReferencesInExpr(addrs.ParseRef, call)
			for _, callRef := range callRefs {
				switch callRef.Subject.(type) {
				case addrs.CountAttr, addrs.ForEachAttr:
					repetitionSensitive = true
				}
			}

			callKey, keyDiags := a.providerFunctionCallKey(ref, call.Range(), repetitionSensitive)
			a.diags = a.diags.Append(keyDiags)
			if keyDiags.HasErrors() {
				return
			}
			if _, ok := a.providerCallSeen[callKey]; ok {
				return
			}
			a.providerCallSeen[callKey] = struct{}{}
			request, requestDiags := a.lowerProviderFunctionRequest(ref, call)
			a.diags = a.diags.Append(requestDiags)
			a.providerCalls = append(a.providerCalls, ProviderFunctionCall{
				Ref:     ref,
				Key:     callKey,
				Request: request,
			})
		},
	}))
}

func (a *targetAnalysis) analyzeLocal(name string) {
	if a == nil || a.moduleCfg == nil || name == "" {
		return
	}
	if _, ok := a.localSeen[name]; ok {
		return
	}
	a.localSeen[name] = struct{}{}

	local := a.moduleCfg.Locals[name]
	if local == nil || local.Expr == nil {
		return
	}
	a.analyzeExpr(local.Expr)
}

func (a *targetAnalysis) appendProviderFunction(ref ProviderFunctionRef) {
	if _, ok := a.providerSeen[ref]; ok {
		return
	}
	a.providerSeen[ref] = struct{}{}
	a.providerFunctions = append(a.providerFunctions, ref)
}

func (a *targetAnalysis) providerFunctionCallKey(ref ProviderFunctionRef, rng hcl.Range, repetitionSensitive bool) (digest.Digest, tfdiags.Diagnostics) {
	file := a.sources[rng.Filename]
	if file == nil {
		return digest.Digest{}, tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Build source error",
			Detail:   "Missing HCL source bytes for provider function call " + ref.Function.String() + ".",
			Subject:  rng.Ptr(),
		})
	}
	if rng.Start.Byte < 0 || rng.End.Byte > len(file.Bytes) || rng.Start.Byte > rng.End.Byte {
		return digest.Digest{}, tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Build source error",
			Detail:   "Invalid HCL source range for provider function call " + ref.Function.String() + ".",
			Subject:  rng.Ptr(),
		})
	}

	instanceKey := catalog.NoKey()
	if repetitionSensitive {
		instanceKey = a.instanceKey
	}

	key, err := digest.FromValue(struct {
		Version             string
		Function            addrs.ProviderFunction
		Source              string
		RepetitionSensitive bool
		InstanceKey         catalog.Key
	}{
		Version:             "build-provider-function-call-v1",
		Function:            ref.Function,
		Source:              string(file.Bytes[rng.Start.Byte:rng.End.Byte]),
		RepetitionSensitive: repetitionSensitive,
		InstanceKey:         instanceKey,
	})
	if err != nil {
		return digest.Digest{}, tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Build source error",
			Detail:   "Failed to key provider function call " + ref.Function.String() + ": " + err.Error(),
			Subject:  rng.Ptr(),
		})
	}
	return key, nil
}

func (a *targetAnalysis) lowerProviderFunctionRequest(ref ProviderFunctionRef, call *hclsyntax.FunctionCallExpr) (*buildprovider.FunctionRequest, tfdiags.Diagnostics) {
	if a.requestLowerer == nil || call == nil {
		return nil, nil
	}
	if a.requestLowerer.querySafety == nil {
		a.requestLowerer.querySafety = buildprovider.DefaultQuerySafetyRegistry()
	}
	if a.requestLowerer.querySafety.IsQuerySafe(ref.Function) {
		return nil, nil
	}

	args := make([]cty.Value, 0, len(call.Args))
	var diags tfdiags.Diagnostics
	for _, arg := range call.Args {
		if arg == nil {
			continue
		}

		lowerable, argDiags := a.requestLowerer.argumentIsLowerable(arg)
		diags = diags.Append(argDiags)
		if argDiags.HasErrors() {
			return nil, diags
		}
		if !lowerable {
			return nil, diags
		}

		value, valueDiags := a.requestLowerer.evaluate(arg)
		if valueDiags.HasErrors() {
			if configs.StaticEvalDefers(valueDiags.ToHCL()) {
				return nil, diags
			}
			return nil, diags.Append(valueDiags)
		}
		args = append(args, value)
	}

	request, requestDiags := buildprovider.BuildFunctionRequest(ref.Function, args, tfdiags.SourceRangeFromHCL(call.Range()))
	diags = diags.Append(requestDiags)
	if requestDiags.HasErrors() {
		return nil, diags
	}
	return request, diags
}

func (l *providerRequestLowerer) argumentIsLowerable(expr hcl.Expression) (bool, tfdiags.Diagnostics) {
	if l == nil || expr == nil {
		return false, nil
	}
	if l.querySafety == nil {
		l.querySafety = buildprovider.DefaultQuerySafetyRegistry()
	}

	refs, diags := lang.ReferencesInExpr(addrs.ParseRef, expr)
	for _, ref := range refs {
		fn, ok := ref.Subject.(addrs.ProviderFunction)
		if !ok {
			continue
		}
		if l.querySafety.IsQuerySafe(fn) {
			continue
		}
		return false, diags
	}
	return true, diags
}

func (l *providerRequestLowerer) evaluate(expr hcl.Expression) (cty.Value, tfdiags.Diagnostics) {
	if l == nil || l.evaluator == nil {
		return cty.NilVal, nil
	}
	if l.queryFunctions == nil {
		l.queryFunctions = buildprovider.DefaultQueryFunctionResolver()
	}

	ps, psDiags := l.evaluator.Prepare(l.ctx, l.options.Variables, l.options.EvalCache)
	if psDiags.HasErrors() {
		return cty.DynamicVal, tfdiags.Diagnostics{}.Append(psDiags)
	}
	ps = ps.WithOverlay(configs.EvalOverlay{
		Runtime:           l.options.Runtime,
		ProviderFunctions: l.queryFunctions.Resolve,
	}, l.options.CountAttrs, l.options.ForEachAttrs)
	value, diags := ps.Evaluate(l.ctx, expr, l.ident)
	return value, tfdiags.Diagnostics{}.Append(diags)
}

type targetFunctionWalker struct {
	onFunction func(*hclsyntax.FunctionCallExpr)
}

func (w targetFunctionWalker) Enter(node hclsyntax.Node) hcl.Diagnostics {
	if w.onFunction == nil {
		return nil
	}
	call, ok := node.(*hclsyntax.FunctionCallExpr)
	if !ok {
		return nil
	}
	w.onFunction(call)
	return nil
}

func (w targetFunctionWalker) Exit(hclsyntax.Node) hcl.Diagnostics {
	return nil
}

func providerFunctionFromCall(call *hclsyntax.FunctionCallExpr) (addrs.ProviderFunction, bool) {
	if call == nil {
		return addrs.ProviderFunction{}, false
	}
	function := addrs.ParseFunction(call.Name)
	if !function.IsNamespace(addrs.FunctionNamespaceProvider) {
		return addrs.ProviderFunction{}, false
	}
	fn, err := function.AsProviderFunction()
	if err != nil {
		return addrs.ProviderFunction{}, false
	}
	return fn, true
}

func runtimeLookupForTarget(addr catalog.Addr, resolver RuntimeResolver) configs.RuntimeValueLookup {
	return runtimeLookupForModule(addr.Module, resolver)
}

func runtimeLookupForModule(module catalog.ModulePath, resolver RuntimeResolver) configs.RuntimeValueLookup {
	if resolver == nil {
		return nil
	}
	return outputRuntimeLookup{
		module:   module,
		resolver: resolver,
	}
}

func (l *Loaded) syntaxBlockForRange(rng hcl.Range) (*hcl.File, *hclsyntax.Block) {
	if l == nil {
		return nil, nil
	}
	file := l.Sources()[rng.Filename]
	if file == nil {
		return nil, nil
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return file, nil
	}
	return file, findSyntaxBlock(body, rng)
}

func findSyntaxBlock(body *hclsyntax.Body, rng hcl.Range) *hclsyntax.Block {
	if body == nil {
		return nil
	}
	for _, block := range body.Blocks {
		if block.DefRange() == rng {
			return block
		}
		if nested := findSyntaxBlock(block.Body, rng); nested != nil {
			return nested
		}
	}
	return nil
}

func skipTopLevelTargetAttr(name string) bool {
	switch name {
	case "count", "for_each", "provider", "depends_on":
		return true
	default:
		return false
	}
}

func skipTopLevelTargetBlock(typ string) bool {
	switch typ {
	case "lifecycle", "connection", "provisioner":
		return true
	default:
		return false
	}
}


