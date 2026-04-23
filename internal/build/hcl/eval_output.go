// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func (l *Loaded) EvalOutput(ctx context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
	if l == nil || l.Catalog == nil || l.Config == nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"HCL build source is not initialized.",
		))
	}
	if req.Addr.Kind != catalog.TargetKindOutput {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"EvalOutput requires an output address.",
		))
	}

	declAddr := req.Addr
	declAddr.Module = declAddr.Module.Declaration()
	outputDecl, ok := l.Catalog.Output(declAddr)
	if !ok {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Unknown output "+req.Addr.String()+".",
		))
	}

	outputCfg, ok := outputDecl.Payload.(*configs.Output)
	if !ok || outputCfg == nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Output "+req.Addr.String()+" is not backed by an HCL output declaration.",
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
	scope, scopeErr := l.moduleScope(ctx, req.Addr.Module)
	if scopeErr != nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Failed to prepare scope for "+req.Addr.Module.String()+": "+scopeErr.Error(),
		))
	}
	evalOptions := scope.Options()
	if req.Runtime != nil {
		evalOptions.Runtime = runtimeLookupForModule(req.Addr.Module, req.Runtime)
	}

	ident := configs.StaticIdentifier{
		Module:    moduleCfg.Path,
		Subject:   "output." + outputCfg.Name,
		DeclRange: outputCfg.UsageRange(),
	}
	result, diags := l.outputAnalysis(ctx, req.Addr, outputDecl, outputCfg, moduleCfg.Module, ident, evalOptions)
	if diags.HasErrors() {
		return result, diags
	}

	querySafety := buildprovider.DefaultQuerySafetyRegistry()
	var queryFunctions *buildprovider.QueryFunctionResolver
	for _, call := range result.ProviderFunctionCalls {
		if !querySafety.IsQuerySafe(call.Ref.Function) {
			continue
		}
		if queryFunctions == nil {
			queryFunctions = buildprovider.DefaultQueryFunctionResolver()
		}
	}

	var runtimeLookup configs.RuntimeValueLookup
	if req.Runtime != nil {
		runtimeLookup = runtimeLookupForModule(req.Addr.Module, req.Runtime)
	}
	if outputCfg.IsOverridden {
		if outputCfg.OverrideValue == nil {
			result.Deferred = true
		} else {
			valueDigest, valueDiags := ctyValueDigest(*outputCfg.OverrideValue, outputCfg.UsageRange())
			if valueDiags.HasErrors() {
				return result, valueDiags
			}
			result.Value = *outputCfg.OverrideValue
			result.Digest = valueDigest
			result.Known = true
		}
	} else if outputCfg.Expr == nil {
		result.Deferred = true
	} else {
		value, evalDiags := evaluateOutputExpr(ctx, moduleCfg.Module.StaticEvaluator, outputCfg.Expr, ident, evalOptions, nil, queryFunctions)
		if evalDiags.HasErrors() {
			if configs.StaticEvalDefers(evalDiags) {
				result.Deferred = true
			} else {
				diags = diags.Append(evalDiags)
				return result, diags
			}
		} else {
			if !value.IsWhollyKnown() {
				result.Deferred = true
				return result, diags
			}
			valueDigest, valueDiags := ctyValueDigest(value, outputCfg.UsageRange())
			diags = diags.Append(valueDiags)
			if diags.HasErrors() {
				return result, diags
			}

			result.Value = value
			result.Digest = valueDigest
			result.Known = true
		}
	}

	preconditionsDeferred, preconditionDiags := evaluateOutputPreconditions(ctx, moduleCfg.Module.StaticEvaluator, ident, evalOptions, outputCfg.Preconditions, nil, queryFunctions)
	if preconditionDiags.HasErrors() {
		result = clearKnownOutputResult(result)
		return result, diags.Append(preconditionDiags)
	}
	if runtimeLookup != nil && (result.Deferred || preconditionsDeferred) {
		if outputCfg.Expr != nil && !outputCfg.IsOverridden {
			value, runtimeDiags := evaluateOutputExpr(ctx, moduleCfg.Module.StaticEvaluator, outputCfg.Expr, ident, evalOptions, runtimeLookup, queryFunctions)
			if runtimeDiags.HasErrors() {
				if configs.StaticEvalDefers(runtimeDiags) {
					result = clearKnownOutputResult(result)
					result.Deferred = true
				} else {
					result = clearKnownOutputResult(result)
					return result, diags.Append(runtimeDiags)
				}
			} else {
				if !value.IsWhollyKnown() {
					result = clearKnownOutputResult(result)
					result.Deferred = true
					return result, diags
				}
				valueDigest, valueDiags := ctyValueDigest(value, outputCfg.UsageRange())
				diags = diags.Append(valueDiags)
				if diags.HasErrors() {
					return result, diags
				}
				result.Value = value
				result.Digest = valueDigest
				result.Known = true
				result.Deferred = false
			}
		}

		preconditionsDeferred, preconditionDiags = evaluateOutputPreconditions(ctx, moduleCfg.Module.StaticEvaluator, ident, evalOptions, outputCfg.Preconditions, runtimeLookup, queryFunctions)
		if preconditionDiags.HasErrors() {
			result = clearKnownOutputResult(result)
			return result, diags.Append(preconditionDiags)
		}
	}
	if preconditionsDeferred {
		result = clearKnownOutputResult(result)
		result.Deferred = true
	}

	return result, diags
}

func (l *Loaded) outputAnalysis(ctx context.Context, addr catalog.Addr, outputDecl *catalog.OutputDecl, outputCfg *configs.Output, moduleCfg *configs.Module, ident configs.StaticIdentifier, evalOptions configs.StaticEvalOptions) (EvalResult, tfdiags.Diagnostics) {
	result := EvalResult{}
	if outputDecl == nil {
		return result, nil
	}
	refSeen := make(map[catalog.AddrKey]struct{}, len(outputDecl.ExplicitDeps))
	result.Refs = appendUniqueRefs(result.Refs, refSeen, rewriteExplicitRefsForModule(outputDecl.ExplicitDeps, outputDecl.Addr.Module, addr.Module)...)
	if outputCfg == nil || moduleCfg == nil {
		return result, nil
	}

	analysis := newTargetAnalysis(addr.Module, addr.Key, moduleCfg, l.Sources(), newProviderRequestLowerer(ctx, moduleCfg.StaticEvaluator, ident, evalOptions))
	if !outputCfg.IsOverridden && outputCfg.Expr != nil {
		analysis.analyzeExpr(outputCfg.Expr)
	}
	for _, rule := range outputCfg.Preconditions {
		if rule == nil {
			continue
		}
		analysis.analyzeExpr(rule.Condition)
		analysis.analyzeExpr(rule.ErrorMessage)
	}
	if analysis.diags.HasErrors() {
		return result, analysis.diags
	}

	result.Refs = appendUniqueRefs(result.Refs, refSeen, analysis.refs...)
	result.ProviderFunctionCalls = append(result.ProviderFunctionCalls, analysis.providerCalls...)
	return result, nil
}

func rewriteExplicitRefsForModule(refs []catalog.Addr, declModule, concreteModule catalog.ModulePath) []catalog.Addr {
	if len(refs) == 0 || declModule.Identity() == concreteModule.Identity() {
		return refs
	}
	ret := make([]catalog.Addr, 0, len(refs))
	for _, ref := range refs {
		ret = append(ret, rewriteAddrModulePrefix(ref, declModule, concreteModule))
	}
	return ret
}

func rewriteAddrModulePrefix(addr catalog.Addr, declPrefix, concretePrefix catalog.ModulePath) catalog.Addr {
	if addr.Module.Identity() == declPrefix.Identity() {
		addr.Module = concretePrefix
		return addr
	}

	rewritten := concretePrefix
	for i := declPrefix.Len(); i < addr.Module.Len(); i++ {
		step := addr.Module.Step(i)
		rewritten = rewritten.Child(step.Name, step.Key)
	}
	addr.Module = rewritten
	return addr
}

func appendUniqueRefs(dst []catalog.Addr, seen map[catalog.AddrKey]struct{}, refs ...catalog.Addr) []catalog.Addr {
	for _, ref := range refs {
		if _, ok := seen[ref.Identity()]; ok {
			continue
		}
		seen[ref.Identity()] = struct{}{}
		dst = append(dst, ref)
	}
	return dst
}

type outputRuntimeLookup struct {
	module   catalog.ModulePath
	resolver RuntimeResolver
}

func (l outputRuntimeLookup) GetResource(ctx context.Context, addr addrs.Resource, rng tfdiags.SourceRange) configs.RuntimeLookupResult {
	if l.resolver == nil {
		return configs.RuntimeLookupResult{}
	}
	catalogAddr, ok := catalogAddrForReference(l.module, addr)
	if !ok {
		return configs.RuntimeLookupResult{}
	}
	value, known, diags := l.resolver.TargetValue(ctx, catalogAddr)
	if !known {
		return configs.RuntimeLookupResult{Known: false, Diags: diags}
	}
	return runtimeLookupResultForValue(value, diags)
}

func (l outputRuntimeLookup) GetModule(ctx context.Context, addr addrs.ModuleCall, rng tfdiags.SourceRange) configs.RuntimeLookupResult {
	if l.resolver == nil {
		return configs.RuntimeLookupResult{}
	}
	module := l.module.Child(addr.Name, catalog.NoKey())
	value, known, diags := l.resolver.ModuleValue(ctx, module)
	if !known {
		return configs.RuntimeLookupResult{Known: false, Diags: diags}
	}
	return runtimeLookupResultForValue(value, diags)
}

func (l outputRuntimeLookup) GetRun(ctx context.Context, addr addrs.Run, rng tfdiags.SourceRange) configs.RuntimeLookupResult {
	if l.resolver == nil {
		return configs.RuntimeLookupResult{}
	}
	catalogAddr := catalog.RunAddr(l.module, addr.Name, catalog.NoKey())
	value, known, diags := l.resolver.TargetValue(ctx, catalogAddr)
	if !known {
		return configs.RuntimeLookupResult{Known: false, Diags: diags}
	}
	return runtimeLookupResultForValue(value, diags)
}

func (l outputRuntimeLookup) GetOutput(ctx context.Context, addr addrs.OutputValue, rng tfdiags.SourceRange) configs.RuntimeLookupResult {
	if l.resolver == nil {
		return configs.RuntimeLookupResult{}
	}
	catalogAddr := catalog.OutputAddr(l.module, addr.Name)
	value, known, diags := l.resolver.OutputValue(ctx, catalogAddr)
	if !known {
		return configs.RuntimeLookupResult{Known: false, Diags: diags}
	}
	return runtimeLookupResultForValue(value, diags)
}

func (l outputRuntimeLookup) Resolver() RuntimeResolver {
	return l.resolver
}

func rescopeRuntime(runtime configs.RuntimeValueLookup, module catalog.ModulePath) configs.RuntimeValueLookup {
	if runtime == nil {
		return nil
	}
	type resolverCarrier interface {
		Resolver() RuntimeResolver
	}
	rc, ok := runtime.(resolverCarrier)
	if !ok {
		return runtime
	}
	return runtimeLookupForModule(module, rc.Resolver())
}

func runtimeLookupResultForValue(value cty.Value, diags tfdiags.Diagnostics) configs.RuntimeLookupResult {
	if diags.HasErrors() || value == cty.NilVal {
		return configs.RuntimeLookupResult{Known: false, Diags: diags}
	}
	return configs.RuntimeLookupResult{Value: value, Known: true, Diags: diags}
}

func configForModulePath(root *configs.Config, module catalog.ModulePath) *configs.Config {
	if root == nil {
		return nil
	}
	if module.Len() == 0 {
		return root
	}

	path := make(addrs.Module, module.Len())
	for i := range path {
		path[i] = module.Step(i).Name
	}
	return root.Descendent(path)
}

func ctyValueDigest(value cty.Value, rng hcl.Range) (digest.Digest, tfdiags.Diagnostics) {
	src, err := ctyjson.Marshal(value, value.Type())
	if err == nil {
		return digest.FromBytes(src), nil
	}

	return digest.Digest{}, tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "Failed to digest build output value",
		Detail:   err.Error(),
		Subject:  rng.Ptr(),
	})
}

func evaluateOutputExpr(ctx context.Context, evaluator *configs.StaticEvaluator, expr hcl.Expression, ident configs.StaticIdentifier, options configs.StaticEvalOptions, runtime configs.RuntimeValueLookup, queryFunctions *buildprovider.QueryFunctionResolver) (cty.Value, hcl.Diagnostics) {
	options.Runtime = runtime
	if queryFunctions != nil {
		options.ProviderFunctions = queryFunctions.Resolve
	}
	return evaluator.EvaluateWithOptions(ctx, expr, ident, options)
}

func evaluateOutputPreconditions(ctx context.Context, evaluator *configs.StaticEvaluator, ident configs.StaticIdentifier, options configs.StaticEvalOptions, rules []*configs.CheckRule, runtime configs.RuntimeValueLookup, queryFunctions *buildprovider.QueryFunctionResolver) (bool, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	var deferred bool

	for _, rule := range rules {
		if rule == nil || rule.Condition == nil {
			continue
		}

		result, conditionDiags := evaluateOutputExpr(ctx, evaluator, rule.Condition, ident, options, runtime, queryFunctions)
		if conditionDiags.HasErrors() {
			if configs.StaticEvalDefers(conditionDiags) {
				deferred = true
				continue
			}
			diags = diags.Append(conditionDiags)
			continue
		}

		if !result.IsKnown() {
			deferred = true
			continue
		}
		if result.IsNull() {
			diags = diags.Append(&hcl.Diagnostic{
				Severity:   hcl.DiagError,
				Summary:    "Invalid condition result",
				Detail:     "Condition expression must return either true or false, not null.",
				Subject:    rule.Condition.Range().Ptr(),
				Expression: rule.Condition,
			})
			continue
		}

		boolResult, err := convert.Convert(result, cty.Bool)
		if err != nil {
			diags = diags.Append(&hcl.Diagnostic{
				Severity:   hcl.DiagError,
				Summary:    "Invalid condition result",
				Detail:     fmt.Sprintf("Invalid condition result value: %s.", tfdiags.FormatError(err)),
				Subject:    rule.Condition.Range().Ptr(),
				Expression: rule.Condition,
			})
			continue
		}
		boolResult, _ = boolResult.Unmark()
		if boolResult.True() {
			continue
		}

		errorMessage := "This output precondition failed, but its error message could not be evaluated."
		if rule.ErrorMessage != nil {
			msgValue, messageDiags := evaluateOutputExpr(ctx, evaluator, rule.ErrorMessage, ident, options, runtime, queryFunctions)
			if messageDiags.HasErrors() {
				if !configs.StaticEvalDefers(messageDiags) {
					diags = diags.Append(messageDiags)
				}
			} else {
				msgValue, err = convert.Convert(msgValue, cty.String)
				if err != nil {
					diags = diags.Append(&hcl.Diagnostic{
						Severity:   hcl.DiagError,
						Summary:    "Invalid error message",
						Detail:     fmt.Sprintf("Unsuitable value for error message: %s.", tfdiags.FormatError(err)),
						Subject:    rule.ErrorMessage.Range().Ptr(),
						Expression: rule.ErrorMessage,
					})
				} else if msgValue.IsKnown() && !msgValue.IsNull() {
					errorMessage = msgValue.AsString()
				}
			}
		}

		diags = diags.Append(&hcl.Diagnostic{
			Severity:   hcl.DiagError,
			Summary:    addrs.OutputPrecondition.Description() + " failed",
			Detail:     errorMessage,
			Subject:    rule.Condition.Range().Ptr(),
			Expression: rule.Condition,
		})
	}

	return deferred, diags
}

func clearKnownOutputResult(result EvalResult) EvalResult {
	result.Value = cty.NilVal
	result.Digest = digest.Digest{}
	result.Known = false
	return result
}
