// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
	"unique"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/instances"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/plans/objchange"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
	"github.com/opentofu/opentofu/internal/tracing/traceattrs"
)

// walkContext bundles the per-walk invariants shared across all vertex
// evaluations. Created once before the walk, read-only during it (except
// for the atomic stats counters and the providerCache mutex).
type walkContext struct {
	graph       *Graph
	results     *Results
	pc          *providerCache
	config      *configs.Config
	hashIndex   *HashIndex
	workDir     string
	workspace   string
	dryRun      bool
	sharedFuncs map[string]function.Function
	hooks       BuildHooks // nil means no hooks
	ui          BuildUI    // never nil (nilBuildUI if unset)
	stats       *walkStats
}

// walkStats tracks per-kind vertex counts during the walk.
// All fields are atomic — safe for concurrent use from pipeline goroutines.
type walkStats struct {
	variables    atomic.Int64
	locals       atomic.Int64
	localsCached atomic.Int64 // locals reusing expansion-time value
	outputs      atomic.Int64
	resources    atomic.Int64
	dataSources  atomic.Int64
	execs        atomic.Int64
	providers    atomic.Int64
	errors       atomic.Int64
	vertices     atomic.Int64 // total vertices walked (pipeline mode)
}

// newScope builds a lang.Scope for expression evaluation during the walk.
func (wc *walkContext) newScope(module addrs.ModuleInstance, repData instances.RepetitionData) *lang.Scope {
	return &lang.Scope{
		Data: &evalData{
			graph:     wc.graph,
			results:   wc.results,
			config:    wc.config,
			module:    module,
			repData:   repData,
			rootDir:   wc.config.Module.SourceDir,
			workDir:   wc.workDir,
			workspace: wc.workspace,
		},
		ParseRef:          addrs.ParseRef,
		BaseDir:           wc.workDir,
		SharedFuncs:       wc.sharedFuncs,
		ProviderFunctions: wc.providerFunction,
	}
}

// evalResourceConfig expands dynamic blocks and evaluates a resource config body.
// Shared preamble for both provider-backed and built-in resource evaluation.
func evalResourceConfig(ctx context.Context, scope *lang.Scope, v *Vertex) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	expandedBody, expandDiags := scope.ExpandBlock(ctx, v.ResourceCfg.Config, v.Schema)
	diags = diags.Append(expandDiags)
	if expandDiags.HasErrors() {
		return cty.DynamicVal, diags
	}

	configVal, evalDiags := scope.EvalBlock(ctx, expandedBody, v.Schema)
	diags = diags.Append(evalDiags)
	if evalDiags.HasErrors() {
		return cty.DynamicVal, diags
	}

	return configVal, diags
}

// executeVertex dispatches evaluation for a single vertex during the walk.
func (wc *walkContext) executeVertex(ctx context.Context, id ID, v *Vertex) tfdiags.Diagnostics {
	// Fast path: pass-through vertices that don't need expression evaluation.
	// These are pure value relays — handling them before allocating evalData
	// and lang.Scope avoids ~1.6M heap allocations on a full build.
	switch v.Kind {
	case KindVariable:
		wc.stats.variables.Add(1)
		if wc.results.Values[id] == cty.NilVal {
			switch {
			case v.VariableVal != cty.NilVal:
				wc.results.Values[id] = v.VariableVal
			case v.VariableCfg != nil && v.VariableCfg.Default != cty.NilVal:
				wc.results.Values[id] = v.VariableCfg.Default
			default:
				wc.results.Values[id] = cty.DynamicVal
			}
		}
		return nil

	case KindLocal:
		if v.LocalVal != cty.NilVal {
			wc.stats.locals.Add(1)
			wc.stats.localsCached.Add(1)
			wc.results.Values[id] = v.LocalVal
			return nil
		}
		// Uncached locals fall through to the slow path below.

	case KindProviderConfig:
		// Fast path: provider already configured (seeded from root walk in
		// per-module pipeline). Skip re-evaluating the config body and the
		// redundant ConfigureProvider gRPC call.
		if wc.results.Values[id] != cty.NilVal {
			return nil
		}

	case KindModuleClose:
		wc.results.Values[id] = cty.EmptyObjectVal
		return nil
	}

	// Slow path: vertices that require expression evaluation.
	scope := wc.newScope(v.Module, v.RepData)

	var diags tfdiags.Diagnostics

	switch v.Kind {
	case KindLocal:
		// Uncached local — needs expression evaluation.
		wc.stats.locals.Add(1)
		val, evalDiags := scope.EvalExpr(ctx, v.LocalExpr, cty.DynamicPseudoType)
		diags = diags.Append(evalDiags)
		if evalDiags.HasErrors() {
			wc.results.Values[id] = cty.DynamicVal
		} else {
			wc.results.Values[id] = val
		}

	case KindOutput:
		wc.stats.outputs.Add(1)
		if v.OutputCfg == nil || v.OutputCfg.Expr == nil {
			wc.results.Values[id] = cty.DynamicVal
			return nil
		}
		val, evalDiags := scope.EvalExpr(ctx, v.OutputCfg.Expr, cty.DynamicPseudoType)
		diags = diags.Append(evalDiags)
		if evalDiags.HasErrors() {
			wc.results.Values[id] = cty.DynamicVal
		} else {
			if v.OutputCfg.Sensitive {
				val = val.WithMarks(markSensitive)
			}
			wc.results.Values[id] = val
		}

	case KindProviderConfig:
		wc.stats.providers.Add(1)
		diags = evalProviderConfig(ctx, scope, wc.pc, v, wc.results, id)

	case KindResource:
		// Built-in chofu_* resources are evaluated natively by the engine
		// instead of going through the provider protocol. Each type gets
		// its own eval method; the graph treats them as normal KindResource.
		if v.ProviderAddr == chofuProviderAddr {
			wc.stats.execs.Add(1)
			diags = wc.evalBuiltinResource(ctx, scope, v, id)
		} else {
			wc.stats.resources.Add(1)
			_, resSpan := tracing.Tracer().Start(ctx, "chofu.EvalResource",
				tracing.SpanAttributes(
					traceattrs.String("chofu.resource.addr", v.ResourceAddr.String()),
					traceattrs.String("chofu.resource.type", v.ResourceAddr.Resource.Resource.Type),
				),
			)
			diags = wc.evalResource(ctx, scope, v, id)
			resSpan.End()
		}

	case KindDataSource:
		wc.stats.dataSources.Add(1)
		_, dsSpan := tracing.Tracer().Start(ctx, "chofu.EvalDataSource",
			tracing.SpanAttributes(
				traceattrs.String("chofu.resource.addr", v.ResourceAddr.String()),
				traceattrs.String("chofu.resource.type", v.ResourceAddr.Resource.Resource.Type),
			),
		)
		diags = wc.evalResource(ctx, scope, v, id)
		dsSpan.End()

	case KindModuleExpand:
		wc.results.Values[id] = cty.EmptyObjectVal
		if len(v.ModuleCallAttrs) > 0 {
			parentModule := v.Module[:len(v.Module)-1]
			parentScope := wc.newScope(parentModule, v.RepData)
			for varName, expr := range v.ModuleCallAttrs {
				val, valDiags := parentScope.EvalExpr(ctx, expr, cty.DynamicPseudoType)
				diags = diags.Append(valDiags)
				if valDiags.HasErrors() || val == cty.NilVal {
					continue
				}
				if vc, ok := v.ModuleChildVars[varName]; ok {
					val = applyVariableConstraints(val, vc)
				}
				key := addrKey{unique.Make(v.Module.String()), unique.Make(varName)}
				if vid, ok := wc.graph.Vars[key]; ok {
					wc.results.Values[vid] = val
				}
			}
		}
	}

	if diags.HasErrors() {
		wc.stats.errors.Add(1)
	}

	return diags
}

// applyVariableConstraints applies type conversion and default values
// to a variable value. Used during both expansion and walk-time
// module variable resolution.
func applyVariableConstraints(val cty.Value, vc *configs.Variable) cty.Value {
	if vc.ConstraintType != cty.NilType && val.IsKnown() {
		if converted, err := convert.Convert(val, vc.ConstraintType); err == nil {
			val = converted
		}
	}
	if vc.TypeDefaults != nil && val.IsKnown() && !val.IsNull() {
		val = vc.TypeDefaults.Apply(val)
	}
	return val
}

// providerFunction resolves a provider function during the walk phase,
// building a function.Function whose Impl delegates to provider.CallFunction.
// Returns a hard error if resolution fails — during the walk all providers
// must already be configured with schemas fetched.
func (wc *walkContext) providerFunction(ctx context.Context, pf addrs.ProviderFunction, rng tfdiags.SourceRange) (*function.Function, tfdiags.Diagnostics) {
	providerAddr := wc.config.Module.ProviderForLocalConfig(addrs.LocalProviderConfig{
		LocalName: pf.ProviderName,
		Alias:     pf.ProviderAlias,
	})

	// Fast path: return cached function object for this (provider, function) pair.
	cacheKey := providerLookupKey(providerAddr, pf.ProviderAlias) + "::" + pf.Function
	if cached, ok := wc.pc.funcs.Load(cacheKey); ok {
		fn := cached.(function.Function)
		return &fn, nil
	}

	var diags tfdiags.Diagnostics

	schema, ok := wc.pc.schemas[providerAddr]
	if !ok {
		return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error,
			fmt.Sprintf("provider %s has no schema", providerAddr),
			fmt.Sprintf("Cannot resolve function %q: provider schema was not fetched. This is a bug in the build engine.", pf.Function),
		))
	}

	provider, ok := wc.pc.GetConfigured(providerAddr, pf.ProviderAlias)
	if !ok {
		return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error,
			fmt.Sprintf("provider %s is not configured", providerAddr),
			fmt.Sprintf("Cannot resolve function %q: provider config must be evaluated before vertices that use provider functions. This is a bug in the build engine.", pf.Function),
		))
	}

	// Look up function spec: try schema first, fall back to GetFunctions
	// for functions registered post-configure.
	spec, ok := schema.Functions[pf.Function]
	if !ok {
		funcsResp := provider.GetFunctions(ctx)
		if funcsResp.Diagnostics.HasErrors() {
			return nil, funcsResp.Diagnostics
		}
		spec, ok = funcsResp.Functions[pf.Function]
		if !ok {
			return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error,
				fmt.Sprintf("function %q not found in provider %s", pf.Function, providerAddr),
				"The function is not listed in the provider schema or GetFunctions. Check that the provider version supports this function.",
			))
		}
	}

	// Build the function from the spec (mirrors tofu/context_functions.go).
	params := make([]function.Parameter, len(spec.Parameters))
	for i, p := range spec.Parameters {
		params[i] = function.Parameter{
			Name:         p.Name,
			Type:         p.Type,
			AllowNull:    p.AllowNullValue,
			AllowUnknown: p.AllowUnknownValues,
		}
	}
	var varParam *function.Parameter
	if spec.VariadicParameter != nil {
		vp := function.Parameter{
			Name:         spec.VariadicParameter.Name,
			Type:         spec.VariadicParameter.Type,
			AllowNull:    spec.VariadicParameter.AllowNullValue,
			AllowUnknown: spec.VariadicParameter.AllowUnknownValues,
		}
		varParam = &vp
	}

	fn := function.New(&function.Spec{
		Description: spec.Summary,
		Params:      params,
		VarParam:    varParam,
		Type:        function.StaticReturnType(spec.Return),
		Impl: func(args []cty.Value, retType cty.Type) (cty.Value, error) {
			resp := provider.CallFunction(ctx, providers.CallFunctionRequest{
				Name:      pf.Function,
				Arguments: args,
			})
			if argErr, ok := resp.Error.(*providers.CallFunctionArgumentError); ok {
				return resp.Result, function.NewArgError(argErr.FunctionArgument, errors.New(argErr.Text))
			}
			return resp.Result, resp.Error
		},
	})

	wc.pc.funcs.Store(cacheKey, fn)
	return &fn, nil
}

// evalProviderConfig evaluates a provider configuration block and
// configures the provider instance.
func evalProviderConfig(
	ctx context.Context,
	scope *lang.Scope,
	pc *providerCache,
	v *Vertex,
	results *Results,
	id ID,
) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	_, span := tracing.Tracer().Start(ctx, "chofu.ConfigureProvider",
		tracing.SpanAttributes(
			traceattrs.String("chofu.provider", v.ProviderAddr.String()),
		),
	)
	defer span.End()

	var configVal cty.Value
	if v.ProviderBody != nil && v.Schema != nil {
		var evalDiags tfdiags.Diagnostics
		configVal, evalDiags = scope.EvalBlock(ctx, v.ProviderBody, v.Schema)
		diags = diags.Append(evalDiags)
		if diags.HasErrors() {
			results.Values[id] = cty.DynamicVal
			return diags
		}
	} else if v.Schema != nil {
		// Implicit provider (no explicit "provider" block) — build an empty
		// config matching the schema so the provider sees null attributes
		// instead of a raw empty object.
		configVal = v.Schema.EmptyValue()
	} else {
		results.Values[id] = cty.EmptyObjectVal
		return nil
	}

	_, cfgDiags := pc.ConfigureProvider(ctx, v.ProviderAddr, v.ProviderAlias, configVal)
	diags = diags.Append(cfgDiags)

	results.Values[id] = configVal
	return diags
}

// evalResource evaluates a resource/data source config, computes its
// content hash, and calls the provider. In dry-run mode, produces
// PlannedUnknownObject values without calling providers.
func (wc *walkContext) evalResource(
	ctx context.Context,
	scope *lang.Scope,
	v *Vertex,
	id ID,
) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	if v.ResourceCfg == nil || v.Schema == nil {
		wc.results.Values[id] = cty.DynamicVal
		return nil
	}

	configVal, evalDiags := evalResourceConfig(ctx, scope, v)
	diags = diags.Append(evalDiags)
	if evalDiags.HasErrors() {
		wc.results.Values[id] = cty.DynamicVal
		return diags
	}

	resType := v.ResourceAddr.Resource.Resource.Type
	depHashes := wc.collectDepHashes(id)
	contentHash := ComputeContentHash(resType, configVal, depHashes)
	wc.hashIndex.Record(v.ResourceAddr, contentHash)

	if wc.dryRun {
		wc.results.Values[id] = objchange.PlannedUnknownObject(v.Schema, configVal)
		return diags
	}

	provider, ok := wc.pc.GetConfigured(v.ProviderAddr, v.ProviderAlias)
	if !ok {
		diags = diags.Append(tfdiags.Sourceless(tfdiags.Error,
			fmt.Sprintf("provider %s not configured", v.ProviderAddr),
			"The provider configuration must be evaluated before any of its resources.",
		))
		wc.results.Values[id] = cty.DynamicVal
		return diags
	}

	unmarkedConfig, _ := configVal.UnmarkDeep()

	action := "create"
	if v.Kind == KindDataSource {
		action = "read"
	}
	if wc.hooks != nil {
		wc.hooks.PreBuildResource(v.ResourceAddr, action)
	}
	wc.ui.Event(BuildEvent{ResourceStart: &ResourceStartEvent{
		Addr:     v.ResourceAddr,
		Action:   action,
		Provider: v.ProviderAddr,
	}})
	execStart := time.Now()

	var execErr error
	switch v.Kind {
	case KindDataSource:
		resp := provider.ReadDataSource(ctx, providers.ReadDataSourceRequest{
			TypeName: resType,
			Config:   unmarkedConfig,
		})
		diags = diags.Append(resp.Diagnostics)
		if resp.Diagnostics.HasErrors() {
			wc.results.Values[id] = cty.DynamicVal
			execErr = resp.Diagnostics.Err()
		} else {
			wc.results.Values[id] = resp.State
		}

	case KindResource:
		nullPrior := cty.NullVal(v.Schema.ImpliedType())
		proposedNew := objchange.ProposedNew(v.Schema, nullPrior, unmarkedConfig)

		// Plan then Apply atomically — no user-visible plan phase.
		planResp := provider.PlanResourceChange(ctx, providers.PlanResourceChangeRequest{
			TypeName:         resType,
			PriorState:       nullPrior,
			ProposedNewState: proposedNew,
			Config:           unmarkedConfig,
		})
		diags = diags.Append(planResp.Diagnostics)
		if planResp.Diagnostics.HasErrors() {
			wc.results.Values[id] = cty.DynamicVal
			execErr = planResp.Diagnostics.Err()
		} else {
			applyResp := provider.ApplyResourceChange(ctx, providers.ApplyResourceChangeRequest{
				TypeName:        resType,
				PriorState:      nullPrior,
				PlannedState:    planResp.PlannedState,
				Config:          unmarkedConfig,
				PlannedPrivate:  planResp.PlannedPrivate,
				PlannedIdentity: planResp.PlannedIdentity,
			})
			diags = diags.Append(applyResp.Diagnostics)
			if applyResp.Diagnostics.HasErrors() {
				if applyResp.NewState != cty.NilVal {
					wc.results.Values[id] = applyResp.NewState
				} else {
					wc.results.Values[id] = cty.DynamicVal
				}
				execErr = applyResp.Diagnostics.Err()
			} else {
				wc.results.Values[id] = applyResp.NewState
			}
		}

	default:
		wc.results.Values[id] = cty.DynamicVal
		diags = diags.Append(tfdiags.Sourceless(tfdiags.Error,
			fmt.Sprintf("evalResource called for unexpected vertex kind %d", v.Kind),
			"",
		))
	}

	if wc.hooks != nil {
		wc.hooks.PostBuildResource(v.ResourceAddr, action, execErr)
	}
	wc.ui.Event(BuildEvent{ResourceComplete: &ResourceCompleteEvent{
		Addr:        v.ResourceAddr,
		Action:      action,
		Duration:    time.Since(execStart),
		ContentHash: contentHash,
		Err:         execErr,
	}})

	// Stamp the resource address on all diagnostics so the build UI can
	// show which resource each error/warning came from.
	return stampAddress(diags, v.ResourceAddr.String())
}

// addressedDiag wraps a Diagnostic to add a resource address to its Description.
type addressedDiag struct {
	wrapped tfdiags.Diagnostic
	addr    string
}

func (d addressedDiag) Severity() tfdiags.Severity { return d.wrapped.Severity() }
func (d addressedDiag) Description() tfdiags.Description {
	desc := d.wrapped.Description()
	if desc.Address == "" {
		desc.Address = d.addr
	}
	return desc
}
func (d addressedDiag) Source() tfdiags.Source      { return d.wrapped.Source() }
func (d addressedDiag) FromExpr() *tfdiags.FromExpr { return d.wrapped.FromExpr() }
func (d addressedDiag) ExtraInfo() any              { return d.wrapped.ExtraInfo() }

func stampAddress(diags tfdiags.Diagnostics, addr string) tfdiags.Diagnostics {
	if len(diags) == 0 {
		return diags
	}
	stamped := make(tfdiags.Diagnostics, len(diags))
	for i, d := range diags {
		stamped[i] = addressedDiag{wrapped: d, addr: addr}
	}
	return stamped
}

// collectDepHashes gathers content hashes for upstream resource/data
// source dependencies by walking the graph edges for this vertex.
func (wc *walkContext) collectDepHashes(id ID) map[string]string {
	deps := wc.graph.Deps[id]
	if len(deps) == 0 {
		return nil
	}

	var upstreamRes []addrs.AbsResource
	seen := make(map[string]bool)
	for _, dep := range deps {
		v := &wc.graph.Verts[dep]
		if v.Kind != KindResource && v.Kind != KindDataSource {
			continue
		}
		absRes := v.ResourceAddr.ContainingResource()
		key := absRes.String()
		if !seen[key] {
			seen[key] = true
			upstreamRes = append(upstreamRes, absRes)
		}
	}

	if len(upstreamRes) == 0 {
		return nil
	}

	return wc.hashIndex.CollectDependencyHashes(upstreamRes)
}
