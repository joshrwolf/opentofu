// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"
	"maps"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/lang/evalchecks"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func (l *Loaded) EvalTargetInstances(ctx context.Context, req EvalTargetInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
	if l == nil || l.Catalog == nil || l.Config == nil {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"HCL build source is not initialized.",
		))
	}
	if !req.DeclAddr.Actionable() {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"EvalTargetInstances requires a resource, data, or run target address.",
		))
	}
	if req.DeclAddr.Key.Kind != catalog.KeyKindNone {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"EvalTargetInstances requires a declaration address without an instance key.",
		))
	}

	declAddr := req.DeclAddr
	declAddr.Module = declAddr.Module.Declaration()
	targetDecl, ok := l.Catalog.Target(declAddr)
	if !ok {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Unknown target "+req.DeclAddr.String()+".",
		))
	}

	switch payload := targetDecl.Payload.(type) {
	case *configs.Resource:
		resource := payload
		if resource == nil {
			return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Target "+req.DeclAddr.String()+" is not backed by an HCL resource declaration.",
			))
		}

		moduleCfg := configForModulePath(l.Config, req.DeclAddr.Module)
		if moduleCfg == nil || moduleCfg.Module == nil || moduleCfg.Module.StaticEvaluator == nil {
			return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Module "+req.DeclAddr.Module.String()+" does not have an HCL static evaluator.",
			))
		}

		ident := configs.StaticIdentifier{
			Module:    moduleCfg.Path,
			Subject:   req.DeclAddr.String() + ".instances",
			DeclRange: resource.DeclRange,
		}
		scope, scopeErr := l.moduleScope(ctx, req.DeclAddr.Module)
		if scopeErr != nil {
			return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Failed to prepare scope for "+req.DeclAddr.Module.String()+": "+scopeErr.Error(),
			))
		}
		evalOptions := scope.Options()
		if req.Runtime != nil {
			evalOptions.Runtime = runtimeLookupForModule(req.DeclAddr.Module, req.Runtime)
		}
		repetition, diags := l.targetRepetition(ctx, req.DeclAddr, ident, evalOptions, resource.Count, resource.ForEach, resource.Enabled)
		if diags.HasErrors() {
			return InstanceResult{}, diags
		}
		return cloneInstanceResult(repetition.result), cloneTfdiags(repetition.diags)
	case *configs.Run:
		run := payload
		if run == nil {
			return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Target "+req.DeclAddr.String()+" is not backed by an HCL run declaration.",
			))
		}

		moduleCfg := configForModulePath(l.Config, req.DeclAddr.Module)
		if moduleCfg == nil || moduleCfg.Module == nil || moduleCfg.Module.StaticEvaluator == nil {
			return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Module "+req.DeclAddr.Module.String()+" does not have an HCL static evaluator.",
			))
		}

		ident := configs.StaticIdentifier{
			Module:    moduleCfg.Path,
			Subject:   req.DeclAddr.String() + ".instances",
			DeclRange: run.DeclRange,
		}
		scope, scopeErr := l.moduleScope(ctx, req.DeclAddr.Module)
		if scopeErr != nil {
			return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Failed to prepare scope for "+req.DeclAddr.Module.String()+": "+scopeErr.Error(),
			))
		}
		evalOptions := scope.Options()
		if req.Runtime != nil {
			evalOptions.Runtime = runtimeLookupForModule(req.DeclAddr.Module, req.Runtime)
		}
		repetition, diags := l.targetRepetition(ctx, req.DeclAddr, ident, evalOptions, run.Count, run.ForEach, run.Enabled)
		if diags.HasErrors() {
			return InstanceResult{}, diags
		}
		return cloneInstanceResult(repetition.result), cloneTfdiags(repetition.diags)
	default:
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Target "+req.DeclAddr.String()+" is not backed by a supported HCL build declaration.",
		))
	}
}

func (l *Loaded) EvalImportInstances(ctx context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
	if l == nil || l.Catalog == nil || l.Config == nil {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"HCL build source is not initialized.",
		))
	}
	module := req.Module
	if module.Len() == 0 {
		module = req.DeclAddr.Module
	}
	if module.Len() == 0 {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"EvalImportInstances requires a module address.",
		))
	}
	if req.DeclAddr.Kind != "" && req.DeclAddr.Kind != catalog.TargetKindModule {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"EvalImportInstances requires a module address.",
		))
	}
	step, ok := module.LastStep()
	if !ok {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"EvalImportInstances requires a child module declaration address.",
		))
	}
	if step.Key.Kind != catalog.KeyKindNone {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"EvalImportInstances requires a declaration address without a module instance key on the final step.",
		))
	}

	declModule := module.Declaration()
	importDecl, ok := l.Catalog.Import(declModule)
	if !ok {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Unknown module import "+catalog.ModuleAddr(module).String()+".",
		))
	}

	call, ok := importDecl.Payload.(*configs.ModuleCall)
	if !ok || call == nil {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Module import "+catalog.ModuleAddr(module).String()+" is not backed by an HCL module call declaration.",
		))
	}

	parentModule := module.Parent()
	parentCfg := configForModulePath(l.Config, parentModule)
	if parentCfg == nil || parentCfg.Module == nil || parentCfg.Module.StaticEvaluator == nil {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Module "+parentModule.String()+" does not have an HCL static evaluator.",
		))
	}

	parentKey := catalog.NoKey()
	if step, ok := parentModule.LastStep(); ok {
		parentKey = step.Key
	}

	ident := configs.StaticIdentifier{
		Module:    parentCfg.Path,
		Subject:   catalog.ModuleAddr(module).String() + ".instances",
		DeclRange: call.DeclRange,
	}
	scope, scopeErr := l.moduleScope(ctx, parentModule)
	if scopeErr != nil {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Failed to prepare scope for "+parentModule.String()+": "+scopeErr.Error(),
		))
	}
	evalOptions := scope.Options()
	if req.Runtime != nil {
		evalOptions.Runtime = runtimeLookupForModule(parentModule, req.Runtime)
	}
	repetition, diags := l.importRepetition(ctx, module, call, parentModule, parentKey, ident, evalOptions)
	if diags.HasErrors() {
		return InstanceResult{}, diags
	}
	return cloneInstanceResult(repetition.result), cloneTfdiags(repetition.diags)
}

func (l *Loaded) targetRepetition(ctx context.Context, declAddr catalog.Addr, ident configs.StaticIdentifier, options configs.StaticEvalOptions, countExpr, forEachExpr, enabledExpr hcl.Expression) (cachedRepetitionEval, tfdiags.Diagnostics) {
	cacheKey := declAddr.Identity()
	if options.Runtime == nil {
		if cached, ok := l.cachedTargetRepetition(cacheKey); ok {
			return cached, cloneTfdiags(cached.diags)
		}
	}

	moduleCfg := configForModulePath(l.Config, declAddr.Module)
	if moduleCfg == nil || moduleCfg.Module == nil || moduleCfg.Module.StaticEvaluator == nil {
		diags := tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Module "+declAddr.Module.String()+" does not have an HCL static evaluator.",
		))
		return cachedRepetitionEval{}, diags
	}

	repetition, diags := evalRepetition(ctx, moduleCfg.Module.StaticEvaluator, moduleCfg.Module, declAddr.Module, catalog.NoKey(), l.Sources(), ident, options, countExpr, forEachExpr, enabledExpr)
	if options.Runtime == nil && !diags.HasErrors() && !repetition.result.Deferred {
		l.storeTargetRepetition(cacheKey, repetition)
	}
	return repetition, diags
}

func (l *Loaded) importRepetition(ctx context.Context, module catalog.ModulePath, call *configs.ModuleCall, parentModule catalog.ModulePath, parentKey catalog.Key, ident configs.StaticIdentifier, options configs.StaticEvalOptions) (cachedRepetitionEval, tfdiags.Diagnostics) {
	cacheKey := module.Identity()
	if options.Runtime == nil {
		if cached, ok := l.cachedImportRepetition(cacheKey); ok {
			return cached, cloneTfdiags(cached.diags)
		}
	}

	parentCfg := configForModulePath(l.Config, parentModule)
	if parentCfg == nil || parentCfg.Module == nil || parentCfg.Module.StaticEvaluator == nil {
		diags := tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Module "+parentModule.String()+" does not have an HCL static evaluator.",
		))
		return cachedRepetitionEval{}, diags
	}

	repetition, diags := evalRepetition(ctx, parentCfg.Module.StaticEvaluator, parentCfg.Module, parentModule, parentKey, l.Sources(), ident, options, call.Count, call.ForEach, call.Enabled)
	if options.Runtime == nil && !diags.HasErrors() && !repetition.result.Deferred {
		l.storeImportRepetition(cacheKey, repetition)
	}
	return repetition, diags
}

func (l *Loaded) cachedTargetRepetition(key catalog.AddrKey) (cachedRepetitionEval, bool) {
	if l == nil {
		return cachedRepetitionEval{}, false
	}
	l.repetitionMu.RLock()
	defer l.repetitionMu.RUnlock()
	result, ok := l.targetRepetitionCache[key]
	if !ok {
		return cachedRepetitionEval{}, false
	}
	return cloneCachedRepetitionEval(result), true
}

func (l *Loaded) storeTargetRepetition(key catalog.AddrKey, repetition cachedRepetitionEval) {
	if l == nil {
		return
	}
	l.repetitionMu.Lock()
	l.targetRepetitionCache[key] = cloneCachedRepetitionEval(repetition)
	l.repetitionMu.Unlock()
}

func (l *Loaded) cachedImportRepetition(key catalog.ModulePathKey) (cachedRepetitionEval, bool) {
	if l == nil {
		return cachedRepetitionEval{}, false
	}
	l.repetitionMu.RLock()
	defer l.repetitionMu.RUnlock()
	result, ok := l.importRepetitionCache[key]
	if !ok {
		return cachedRepetitionEval{}, false
	}
	return cloneCachedRepetitionEval(result), true
}

func (l *Loaded) storeImportRepetition(key catalog.ModulePathKey, repetition cachedRepetitionEval) {
	if l == nil {
		return
	}
	l.repetitionMu.Lock()
	l.importRepetitionCache[key] = cloneCachedRepetitionEval(repetition)
	l.repetitionMu.Unlock()
}

func evalRepetition(ctx context.Context, evaluator *configs.StaticEvaluator, moduleCfg *configs.Module, module catalog.ModulePath, instanceKey catalog.Key, sources map[string]*hcl.File, ident configs.StaticIdentifier, options configs.StaticEvalOptions, countExpr, forEachExpr, enabledExpr hcl.Expression) (cachedRepetitionEval, tfdiags.Diagnostics) {
	switch {
	case countExpr != nil:
		return evalCountInstances(ctx, evaluator, moduleCfg, module, instanceKey, sources, ident, options, countExpr)
	case forEachExpr != nil:
		return evalForEachInstances(ctx, evaluator, moduleCfg, module, instanceKey, sources, ident, options, forEachExpr)
	case enabledExpr != nil:
		return evalEnabledInstances(ctx, evaluator, moduleCfg, module, instanceKey, sources, ident, options, enabledExpr)
	default:
		result, diags := instanceResultWithKeys(InstanceResult{}, InstanceShapeSingle, []catalog.Key{catalog.NoKey()})
		if diags.HasErrors() {
			return cachedRepetitionEval{}, diags
		}
		return cachedRepetitionEval{
			kind:   repetitionKindNone,
			result: result,
		}, nil
	}
}

func evalCountInstances(ctx context.Context, evaluator *configs.StaticEvaluator, moduleCfg *configs.Module, module catalog.ModulePath, instanceKey catalog.Key, sources map[string]*hcl.File, ident configs.StaticIdentifier, options configs.StaticEvalOptions, expr hcl.Expression) (cachedRepetitionEval, tfdiags.Diagnostics) {
	result, queryFunctions, hasNonQuerySafe, diags := analyzeInstanceExpr(ctx, evaluator, moduleCfg, ident, options, module, instanceKey, sources, expr)
	if diags.HasErrors() {
		return cachedRepetitionEval{kind: repetitionKindCount, result: result}, diags
	}
	if hasNonQuerySafe {
		result.Deferred = true
		return cachedRepetitionEval{kind: repetitionKindCount, result: result}, nil
	}

	value, evalDiags := evalchecks.EvaluateCountExpressionValue(expr, func(expr hcl.Expression) (cty.Value, tfdiags.Diagnostics) {
		return evaluateStaticInstanceExpr(ctx, evaluator, expr, ident, options, queryFunctions)
	})
	diags = diags.Append(evalDiags)
	if diags.HasErrors() {
		if configs.StaticEvalDefers(diags.ToHCL()) {
			result.Deferred = true
			return cachedRepetitionEval{kind: repetitionKindCount, result: result}, nil
		}
		return cachedRepetitionEval{kind: repetitionKindCount, result: result}, diags
	}
	if !value.IsKnown() || value.IsNull() {
		result.Deferred = true
		return cachedRepetitionEval{kind: repetitionKindCount, result: result}, nil
	}

	count, _ := value.AsBigFloat().Int64()
	keys := make([]catalog.Key, 0, int(count))
	for i := 0; i < int(count); i++ {
		keys = append(keys, catalog.IntKey(i))
	}
	result, keyDiags := instanceResultWithKeys(result, InstanceShapeList, keys)
	if keyDiags.HasErrors() {
		return cachedRepetitionEval{}, keyDiags
	}
	return cachedRepetitionEval{
		kind:   repetitionKindCount,
		result: result,
	}, nil
}

func evalForEachInstances(ctx context.Context, evaluator *configs.StaticEvaluator, moduleCfg *configs.Module, module catalog.ModulePath, instanceKey catalog.Key, sources map[string]*hcl.File, ident configs.StaticIdentifier, options configs.StaticEvalOptions, expr hcl.Expression) (cachedRepetitionEval, tfdiags.Diagnostics) {
	result, queryFunctions, hasNonQuerySafe, diags := analyzeInstanceExpr(ctx, evaluator, moduleCfg, ident, options, module, instanceKey, sources, expr)
	if diags.HasErrors() {
		return cachedRepetitionEval{kind: repetitionKindForEach, result: result}, diags
	}
	if hasNonQuerySafe {
		result.Deferred = true
		return cachedRepetitionEval{kind: repetitionKindForEach, result: result}, nil
	}

	var forEachProviderFunctions lang.ProviderFunction
	if queryFunctions != nil {
		forEachProviderFunctions = queryFunctions.Resolve
	}
	forEachPS, forEachPSDiags := evaluator.Prepare(ctx, options.Variables, options.EvalCache)
	if forEachPSDiags.HasErrors() {
		return cachedRepetitionEval{kind: repetitionKindForEach, result: result}, tfdiags.Diagnostics{}.Append(forEachPSDiags)
	}
	forEachPS = forEachPS.WithOverlay(configs.EvalOverlay{
		Runtime:           options.Runtime,
		ProviderFunctions: forEachProviderFunctions,
	}, options.CountAttrs, options.ForEachAttrs)
	values, evalDiags := evalchecks.EvaluateForEachExpression(expr, func(refs []*addrs.Reference) (*hcl.EvalContext, tfdiags.Diagnostics) {
		evalCtx, hclDiags := forEachPS.EvalContext(ctx, ident, refs)
		return evalCtx, tfdiags.Diagnostics{}.Append(hclDiags)
	}, nil)
	diags = diags.Append(evalDiags)
	if diags.HasErrors() {
		if configs.StaticEvalDefers(diags.ToHCL()) {
			result.Deferred = true
			return cachedRepetitionEval{kind: repetitionKindForEach, result: result}, nil
		}
		return cachedRepetitionEval{kind: repetitionKindForEach, result: result}, diags
	}

	sortedNames := slices.Sorted(maps.Keys(values))

	keys := make([]catalog.Key, 0, len(sortedNames))
	for _, name := range sortedNames {
		keys = append(keys, catalog.StringKey(name))
	}
	result, keyDiags := instanceResultWithKeys(result, InstanceShapeMap, keys)
	if keyDiags.HasErrors() {
		return cachedRepetitionEval{}, keyDiags
	}
	return cachedRepetitionEval{
		kind:   repetitionKindForEach,
		result: result,
		values: cloneForEachValues(values),
	}, nil
}

func evalEnabledInstances(ctx context.Context, evaluator *configs.StaticEvaluator, moduleCfg *configs.Module, module catalog.ModulePath, instanceKey catalog.Key, sources map[string]*hcl.File, ident configs.StaticIdentifier, options configs.StaticEvalOptions, expr hcl.Expression) (cachedRepetitionEval, tfdiags.Diagnostics) {
	result, queryFunctions, hasNonQuerySafe, diags := analyzeInstanceExpr(ctx, evaluator, moduleCfg, ident, options, module, instanceKey, sources, expr)
	if diags.HasErrors() {
		return cachedRepetitionEval{kind: repetitionKindEnabled, result: result}, diags
	}
	if hasNonQuerySafe {
		result.Deferred = true
		return cachedRepetitionEval{kind: repetitionKindEnabled, result: result}, nil
	}

	var enabledProviderFunctions lang.ProviderFunction
	if queryFunctions != nil {
		enabledProviderFunctions = queryFunctions.Resolve
	}
	enabledPS, enabledPSDiags := evaluator.Prepare(ctx, options.Variables, options.EvalCache)
	if enabledPSDiags.HasErrors() {
		return cachedRepetitionEval{kind: repetitionKindEnabled, result: result}, tfdiags.Diagnostics{}.Append(enabledPSDiags)
	}
	enabledPS = enabledPS.WithOverlay(configs.EvalOverlay{
		Runtime:           options.Runtime,
		ProviderFunctions: enabledProviderFunctions,
	}, options.CountAttrs, options.ForEachAttrs)
	value, evalDiags := evalchecks.EvaluateEnabledExpressionValue(expr, func(refs []*addrs.Reference) (*hcl.EvalContext, tfdiags.Diagnostics) {
		evalCtx, hclDiags := enabledPS.EvalContext(ctx, ident, refs)
		return evalCtx, tfdiags.Diagnostics{}.Append(hclDiags)
	}, true)
	diags = diags.Append(evalDiags)
	if diags.HasErrors() {
		if configs.StaticEvalDefers(diags.ToHCL()) {
			result.Deferred = true
			return cachedRepetitionEval{kind: repetitionKindEnabled, result: result}, nil
		}
		return cachedRepetitionEval{kind: repetitionKindEnabled, result: result}, diags
	}
	if !value.IsKnown() || value.IsNull() {
		result.Deferred = true
		return cachedRepetitionEval{kind: repetitionKindEnabled, result: result}, nil
	}
	if value.True() {
		result, keyDiags := instanceResultWithKeys(result, InstanceShapeOptional, []catalog.Key{catalog.NoKey()})
		if keyDiags.HasErrors() {
			return cachedRepetitionEval{}, keyDiags
		}
		return cachedRepetitionEval{
			kind:   repetitionKindEnabled,
			result: result,
		}, nil
	}
	result, keyDiags := instanceResultWithKeys(result, InstanceShapeOptional, []catalog.Key{})
	if keyDiags.HasErrors() {
		return cachedRepetitionEval{}, keyDiags
	}
	return cachedRepetitionEval{
		kind:   repetitionKindEnabled,
		result: result,
	}, nil
}

func analyzeInstanceExpr(ctx context.Context, evaluator *configs.StaticEvaluator, moduleCfg *configs.Module, ident configs.StaticIdentifier, options configs.StaticEvalOptions, module catalog.ModulePath, instanceKey catalog.Key, sources map[string]*hcl.File, expr hcl.Expression) (InstanceResult, *buildprovider.QueryFunctionResolver, bool, tfdiags.Diagnostics) {
	analysis := newTargetAnalysis(module, instanceKey, moduleCfg, sources, newProviderRequestLowerer(ctx, evaluator, ident, options))
	analysis.analyzeExpr(expr)

	result := InstanceResult{
		Refs:                  analysis.refs,
		ProviderFunctionCalls: analysis.providerCalls,
	}
	if analysis.diags.HasErrors() {
		return result, nil, false, analysis.diags
	}

	querySafety := buildprovider.DefaultQuerySafetyRegistry()
	var queryFunctions *buildprovider.QueryFunctionResolver
	hasNonQuerySafe := false
	for _, call := range result.ProviderFunctionCalls {
		if querySafety.IsQuerySafe(call.Ref.Function) {
			if queryFunctions == nil {
				queryFunctions = buildprovider.DefaultQueryFunctionResolver()
			}
			continue
		}
		hasNonQuerySafe = true
	}

	return result, queryFunctions, hasNonQuerySafe, nil
}

func evaluateStaticInstanceExpr(ctx context.Context, evaluator *configs.StaticEvaluator, expr hcl.Expression, ident configs.StaticIdentifier, options configs.StaticEvalOptions, queryFunctions *buildprovider.QueryFunctionResolver) (cty.Value, tfdiags.Diagnostics) {
	var providerFunctions lang.ProviderFunction
	if queryFunctions != nil {
		providerFunctions = queryFunctions.Resolve
	}
	ps, psDiags := evaluator.Prepare(ctx, options.Variables, options.EvalCache)
	if psDiags.HasErrors() {
		return cty.DynamicVal, tfdiags.Diagnostics{}.Append(psDiags)
	}
	ps = ps.WithOverlay(configs.EvalOverlay{
		Runtime:           options.Runtime,
		ProviderFunctions: providerFunctions,
	}, options.CountAttrs, options.ForEachAttrs)
	value, diags := ps.Evaluate(ctx, expr, ident)
	return value, tfdiags.Diagnostics{}.Append(diags)
}

func instanceResultWithKeys(result InstanceResult, shape InstanceShape, keys []catalog.Key) (InstanceResult, tfdiags.Diagnostics) {
	keyDigest, err := digest.FromValue(struct {
		Version string
		Shape   InstanceShape
		Keys    []catalog.Key
	}{
		Version: "build-target-instances-v1",
		Shape:   shape,
		Keys:    keys,
	})
	if err != nil {
		return InstanceResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Failed to digest target instances: "+err.Error(),
		))
	}

	result.Keys = keys
	result.Shape = shape
	result.Digest = keyDigest
	return result, nil
}

func cloneInstanceResult(result InstanceResult) InstanceResult {
	result.Keys = append([]catalog.Key(nil), result.Keys...)
	result.Refs = append([]catalog.Addr(nil), result.Refs...)
	result.ProviderFunctionCalls = append([]ProviderFunctionCall(nil), result.ProviderFunctionCalls...)
	return result
}

func cloneForEachValues(values map[string]cty.Value) map[string]cty.Value {
	if len(values) == 0 {
		return nil
	}
	ret := make(map[string]cty.Value, len(values))
	maps.Copy(ret, values)
	return ret
}

func cloneCachedRepetitionEval(repetition cachedRepetitionEval) cachedRepetitionEval {
	repetition.result = cloneInstanceResult(repetition.result)
	repetition.values = cloneForEachValues(repetition.values)
	repetition.diags = cloneTfdiags(repetition.diags)
	return repetition
}
