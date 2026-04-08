// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"log"
	"slices"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/instances"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/lang/evalchecks"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// Expand performs Phase 1: structural expansion. It walks the config tree,
// evaluates for_each/count expressions to determine all module and resource
// instances, and returns a flat list of vertices for DAG construction.
//
// Sibling module calls within each module are expanded in parallel.
// Thread safety is structural: each subtree receives its own variable
// values — there is no shared mutable state between siblings.
func Expand(
	ctx context.Context,
	config *configs.Config,
	schemas map[addrs.Provider]providers.ProviderSchema,
	expander *instances.Expander,
	filter *TargetFilter,
	rootVars map[string]cty.Value,
	workspace string,
	workDir string,
) ([]Vertex, tfdiags.Diagnostics) {
	// Pre-build the function table once for all expansion scopes.
	// During expansion BaseDir is always workDir, so one table suffices.
	probe := &lang.Scope{BaseDir: workDir, ProviderFunctions: stubProviderFunction}
	sharedFuncs := probe.Functions()

	e := &expCtx{
		config:      config,
		schemas:     schemas,
		expander:    expander,
		filter:      filter,
		workspace:   workspace,
		workDir:     workDir,
		rootDir:     config.Module.SourceDir,
		sharedFuncs: sharedFuncs,
	}

	var diags tfdiags.Diagnostics

	// Expand the root module (recursive).
	verts, expandDiags := e.expandModule(ctx, config, addrs.RootModuleInstance, instances.RepetitionData{}, rootVars)
	diags = diags.Append(expandDiags)

	log.Printf("[INFO] chofu/expand: %d items from structural expansion", len(verts))
	return verts, diags
}

// expCtx holds immutable state shared across the entire expansion walk.
// All fields are read-only after construction — no shared mutable state.
type expCtx struct {
	config      *configs.Config
	schemas     map[addrs.Provider]providers.ProviderSchema
	expander    *instances.Expander // thread-safe (has internal sync.RWMutex)
	filter      *TargetFilter       // prunes irrelevant module calls
	workspace   string
	workDir     string
	rootDir     string
	sharedFuncs map[string]function.Function
}

// expandRootOnly expands the root module's variables, locals, and provider
// configs — but NOT outputs, resources, or child module calls. This is
// the per-module pipeline's Phase 1: evaluate root-scope content so that
// child module call arguments can be resolved and provider configs are
// available for the walk.
func (e *expCtx) expandRootOnly(
	ctx context.Context,
	rootVars map[string]cty.Value,
) (rootVerts []Vertex, localVals map[string]cty.Value, diags tfdiags.Diagnostics) {
	moduleAddr := addrs.RootModuleInstance
	repData := instances.RepetitionData{}

	rootVerts, localVals, diags = e.expandModuleScope(ctx, e.config.Module, moduleAddr, repData, rootVars)

	provVerts := e.emitProviderVerts(e.config, moduleAddr)
	rootVerts = append(rootVerts, provVerts...)

	log.Printf("[INFO] chofu/expand: expandRootOnly produced %d vertices (vars=%d locals=%d providers=%d)",
		len(rootVerts), len(e.config.Module.Variables), len(e.config.Module.Locals), len(provVerts))

	return rootVerts, localVals, diags
}

// expandModuleScope evaluates locals and emits variable + local vertices
// for a single module instance. This is the shared core of expandModule
// and ExpandRootOnly — the scope content that every module needs regardless
// of whether outputs, resources, or children are also emitted.
func (e *expCtx) expandModuleScope(
	ctx context.Context,
	mod *configs.Module,
	moduleAddr addrs.ModuleInstance,
	repData instances.RepetitionData,
	varValues map[string]cty.Value,
) (verts []Vertex, localVals map[string]cty.Value, diags tfdiags.Diagnostics) {
	// Evaluate locals in dependency order (needed for for_each/count).
	localVals, localDiags := e.evaluateLocals(ctx, mod, moduleAddr, repData, varValues)
	diags = diags.Append(localDiags)

	// Emit variable vertices with their resolved values from the parent
	// module call. This is how parent-to-child variable passing works:
	// the expansion resolves values, stores them on the vertex, and the
	// walk uses them instead of falling back to the config default.
	for name, vc := range mod.Variables {
		verts = append(verts, Vertex{
			Kind:        KindVariable,
			Module:      moduleAddr,
			RepData:     repData,
			Name:        name,
			VariableCfg: vc,
			VariableVal: varValues[name],
		})
	}

	// Emit local vertices. Carry expansion-time values for locals that
	// evaluated to wholly-known values — the walk can reuse them instead
	// of re-evaluating (e.g. jsondecode(file(...)) which is expensive).
	// Locals whose expansion-time value is unknown (references a resource
	// or other runtime-only data) are left for the walk to evaluate.
	for name, lc := range mod.Locals {
		v := Vertex{
			Kind:      KindLocal,
			Module:    moduleAddr,
			RepData:   repData,
			Name:      name,
			LocalExpr: lc.Expr,
		}
		if val, ok := localVals[name]; ok && val.IsWhollyKnown() && val != cty.NilVal {
			v.LocalVal = val
		}
		verts = append(verts, v)
	}

	return verts, localVals, diags
}

// emitProviderVerts creates provider config vertices for the root module.
// Explicit provider blocks are emitted first, then implicit configs are
// created for any provider used by resources but not explicitly configured.
func (e *expCtx) emitProviderVerts(cfg *configs.Config, moduleAddr addrs.ModuleInstance) []Vertex {
	var verts []Vertex
	mod := cfg.Module
	emitted := make(map[string]bool)

	for key, pc := range mod.ProviderConfigs {
		providerAddr := mod.ProviderForLocalConfig(addrs.LocalProviderConfig{
			LocalName: pc.Name,
			Alias:     pc.Alias,
		})
		lk := providerLookupKey(providerAddr, pc.Alias)
		if emitted[lk] {
			continue
		}
		emitted[lk] = true
		verts = append(verts, Vertex{
			Kind:          KindProviderConfig,
			Module:        moduleAddr,
			Name:          key,
			ProviderBody:  pc.Config,
			ProviderAddr:  providerAddr,
			ProviderAlias: pc.Alias,
		})
	}

	// Implicit providers referenced by resources but not explicitly configured.
	cfg.DeepEach(func(c *configs.Config) {
		for _, rc := range c.Module.ManagedResources {
			addr := c.Module.ProviderForLocalConfig(rc.ProviderConfigAddr())
			lk := providerLookupKey(addr, "")
			if !emitted[lk] {
				emitted[lk] = true
				verts = append(verts, Vertex{
					Kind:         KindProviderConfig,
					Module:       moduleAddr,
					Name:         addr.Type,
					ProviderAddr: addr,
				})
			}
		}
		for _, rc := range c.Module.DataResources {
			addr := c.Module.ProviderForLocalConfig(rc.ProviderConfigAddr())
			lk := providerLookupKey(addr, "")
			if !emitted[lk] {
				emitted[lk] = true
				verts = append(verts, Vertex{
					Kind:         KindProviderConfig,
					Module:       moduleAddr,
					Name:         addr.Type,
					ProviderAddr: addr,
				})
			}
		}
	})

	return verts
}

// expandModule expands a single module config node and all its children.
// varValues contains the resolved variable values for this module instance,
// keyed by variable name (not qualified by module path).
func (e *expCtx) expandModule(
	ctx context.Context,
	cfg *configs.Config,
	moduleAddr addrs.ModuleInstance,
	repData instances.RepetitionData,
	varValues map[string]cty.Value,
) ([]Vertex, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	mod := cfg.Module

	// Scope: variables, locals.
	verts, localVals, scopeDiags := e.expandModuleScope(ctx, mod, moduleAddr, repData, varValues)
	diags = diags.Append(scopeDiags)

	// Outputs.
	for name, oc := range mod.Outputs {
		verts = append(verts, Vertex{
			Kind:      KindOutput,
			Module:    moduleAddr,
			RepData:   repData,
			Name:      name,
			OutputCfg: oc,
		})
	}

	// Provider configs (root module only).
	if moduleAddr.IsRoot() {
		verts = append(verts, e.emitProviderVerts(cfg, moduleAddr)...)
	}

	// 6. Expand resources within this module instance.
	for _, rc := range mod.ManagedResources {
		resVerts, resDiags := e.expandResource(ctx, mod, moduleAddr, rc, repData, varValues, localVals)
		diags = diags.Append(resDiags)
		verts = append(verts, resVerts...)
	}
	for _, rc := range mod.DataResources {
		resVerts, resDiags := e.expandResource(ctx, mod, moduleAddr, rc, repData, varValues, localVals)
		diags = diags.Append(resDiags)
		verts = append(verts, resVerts...)
	}

	// 7. Expand child module calls in parallel.
	//    Each call is independent: it receives its own child variable values
	//    and produces its own vertices. No shared mutable state.
	type callResult struct {
		verts []Vertex
		diags tfdiags.Diagnostics
	}

	calls := make([]struct {
		name string
		mc   *configs.ModuleCall
	}, 0, len(mod.ModuleCalls))
	for name, mc := range mod.ModuleCalls {
		if !e.filter.ModuleRelevant(moduleAddr.Module(), name) {
			continue
		}
		calls = append(calls, struct {
			name string
			mc   *configs.ModuleCall
		}{name, mc})
	}

	results := make([]callResult, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		idx, cn, m := i, call.name, call.mc
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[WARN] chofu/expand: panic expanding module call %s.%s: %v", moduleAddr, cn, r)
				}
			}()
			v, d := e.expandModuleCall(ctx, cfg, moduleAddr, cn, m, repData, varValues, localVals)
			results[idx] = callResult{v, d}
		})
	}
	wg.Wait()

	for _, r := range results {
		verts = append(verts, r.verts...)
		diags = diags.Append(r.diags)
	}

	return verts, diags
}

// expandModuleCall evaluates a module call's for_each/count expression
// and recursively expands each resulting child instance.
func (e *expCtx) expandModuleCall(
	ctx context.Context,
	parentCfg *configs.Config,
	parentAddr addrs.ModuleInstance,
	callName string,
	mc *configs.ModuleCall,
	parentRepData instances.RepetitionData,
	parentVarValues map[string]cty.Value,
	parentLocals map[string]cty.Value,
) ([]Vertex, tfdiags.Diagnostics) {
	var verts []Vertex
	var diags tfdiags.Diagnostics

	childCfg, ok := parentCfg.Children[callName]
	if !ok {
		return nil, nil
	}

	callAddr := addrs.ModuleCall{Name: callName}
	scope := e.makeScope(parentCfg.Module, parentAddr, parentRepData, parentVarValues, parentLocals)

	// Evaluate for_each/count for the module call and determine the
	// instance keys for THIS parent instance. We must not use
	// ExpandModule here because it walks ALL instances of ancestor
	// modules — including sibling for_each instances that may not have
	// registered their child calls yet (they run in parallel).
	var instanceKeys []addrs.InstanceKey

	switch {
	case mc.ForEach != nil:
		ctxFunc := makeCtxFunc(ctx, scope)
		forEachMap, feDiags := evalchecks.EvaluateForEachExpression(mc.ForEach, ctxFunc, nil)
		if feDiags.HasErrors() {
			e.expander.SetModuleSingle(parentAddr, callAddr)
			instanceKeys = append(instanceKeys, addrs.NoKey)
		} else {
			e.expander.SetModuleForEach(parentAddr, callAddr, forEachMap)
			for key := range forEachMap {
				instanceKeys = append(instanceKeys, addrs.StringKey(key))
			}
		}

	case mc.Count != nil:
		evalFunc := makeEvalFunc(ctx, scope)
		countVal, countDiags := evalchecks.EvaluateCountExpression(mc.Count, evalFunc, nil)
		if countDiags.HasErrors() {
			e.expander.SetModuleSingle(parentAddr, callAddr)
			instanceKeys = append(instanceKeys, addrs.NoKey)
		} else {
			e.expander.SetModuleCount(parentAddr, callAddr, countVal)
			for i := range countVal {
				instanceKeys = append(instanceKeys, addrs.IntKey(i))
			}
		}

	default:
		e.expander.SetModuleSingle(parentAddr, callAddr)
		instanceKeys = append(instanceKeys, addrs.NoKey)
	}

	// Collect module call argument expressions for the expand vertex.
	// mcExprs drives DAG edges; mcAttrMap + childVarCfgs drive walk-time re-evaluation.
	mcAttrs, _ := mc.Config.JustAttributes()
	var mcExprs []hcl.Expression
	mcAttrMap := make(map[string]hcl.Expression, len(mcAttrs))
	for name, attr := range mcAttrs {
		mcExprs = append(mcExprs, attr.Expr)
		mcAttrMap[name] = attr.Expr
	}

	// Expand and recurse into each child instance of THIS parent only.
	for _, key := range instanceKeys {
		childAddr := parentAddr.Child(callName, key)
		childRepData := e.expander.GetModuleInstanceRepetitionData(childAddr)
		childScope := e.makeScope(parentCfg.Module, parentAddr, childRepData, parentVarValues, parentLocals)
		childVarValues := resolveChildVariables(ctx, childCfg.Module, mcAttrs, childScope)

		// Emit expand (gate-in) and close (gate-out) vertices for this
		// module instance. Matches upstream's nodeExpandModule/nodeCloseModule:
		// all child content depends on expand, close depends on all child content.
		// The expand vertex re-evaluates module call arguments during the walk
		// so child variables get real provider outputs instead of expansion-time unknowns.
		verts = append(verts, Vertex{
			Kind:            KindModuleExpand,
			Module:          childAddr,
			RepData:         childRepData,
			Name:            childAddr.String() + " (expand)",
			ModuleCallExprs: mcExprs,
			ModuleCallAttrs: mcAttrMap,
			ModuleChildVars: childCfg.Module.Variables,
			ModuleDependsOn: mc.DependsOn,
		})
		verts = append(verts, Vertex{
			Kind:   KindModuleClose,
			Module: childAddr,
			Name:   childAddr.String() + " (close)",
		})

		childVerts, childDiags := e.expandModule(ctx, childCfg, childAddr, childRepData, childVarValues)
		diags = diags.Append(childDiags)
		verts = append(verts, childVerts...)
	}

	return verts, diags
}

// resolveChildVariables evaluates module call arguments using chofu's scope
// instead of the upstream StaticEvaluator. This avoids re-evaluating locals
// from scratch on every call and supports each.key/count.index which the
// StaticEvaluator cannot handle (it panics on instance-specific data).
func resolveChildVariables(ctx context.Context, childMod *configs.Module, mcAttrs hcl.Attributes, scope *lang.Scope) map[string]cty.Value {
	vals := make(map[string]cty.Value, len(childMod.Variables))
	for name, vc := range childMod.Variables {
		attr, ok := mcAttrs[name]
		if ok {
			val, valDiags := scope.EvalExpr(ctx, attr.Expr, cty.DynamicPseudoType)
			if !valDiags.HasErrors() && val != cty.NilVal {
				vals[name] = applyVariableConstraints(val, vc)
				continue
			}
		}
		if vc.Default != cty.NilVal {
			vals[name] = vc.Default
		} else {
			vals[name] = cty.DynamicVal
		}
	}
	return vals
}

// expandResource evaluates a resource's for_each/count and emits one
// Vertex per instance.
func (e *expCtx) expandResource(
	ctx context.Context,
	mod *configs.Module,
	moduleAddr addrs.ModuleInstance,
	rc *configs.Resource,
	repData instances.RepetitionData,
	varValues map[string]cty.Value,
	locals map[string]cty.Value,
) ([]Vertex, tfdiags.Diagnostics) {
	var verts []Vertex
	var diags tfdiags.Diagnostics

	resAddr := rc.Addr()
	providerCfgAddr := rc.ProviderConfigAddr()
	providerAddr := mod.ProviderForLocalConfig(providerCfgAddr)

	kind := KindResource
	if rc.Mode == addrs.DataResourceMode {
		kind = KindDataSource
	}

	scope := e.makeScope(mod, moduleAddr, repData, varValues, locals)

	switch {
	case rc.ForEach != nil:
		ctxFunc := makeCtxFunc(ctx, scope)
		forEachMap, feDiags := evalchecks.EvaluateForEachExpression(rc.ForEach, ctxFunc, nil)
		if feDiags.HasErrors() {
			// for_each depends on a value not yet known (e.g. a resource
			// output). Fall back to a singleton — the actual instance set
			// will be determined during apply. The error is expected, not
			// a user-facing diagnostic.
			e.expander.SetResourceSingle(moduleAddr, resAddr)
		} else {
			e.expander.SetResourceForEach(moduleAddr, resAddr, forEachMap)
		}

	case rc.Count != nil:
		evalFunc := makeEvalFunc(ctx, scope)
		countVal, countDiags := evalchecks.EvaluateCountExpression(rc.Count, evalFunc, nil)
		if countDiags.HasErrors() {
			// count depends on a value not yet known. Fall back to a
			// singleton so the resource still appears in the graph.
			e.expander.SetResourceSingle(moduleAddr, resAddr)
		} else {
			e.expander.SetResourceCount(moduleAddr, resAddr, countVal)
		}

	default:
		e.expander.SetResourceSingle(moduleAddr, resAddr)
	}

	absRes := resAddr.Absolute(moduleAddr)
	instanceAddrs := e.expander.ExpandResource(absRes)

	for _, instAddr := range instanceAddrs {
		instRepData := e.expander.GetResourceInstanceRepetitionData(instAddr)
		verts = append(verts, Vertex{
			Kind:          kind,
			Module:        moduleAddr,
			RepData:       instRepData,
			Name:          resAddr.String(),
			ResourceCfg:   rc,
			ProviderAddr:  providerAddr,
			ProviderAlias: providerCfgAddr.Alias,
			ResourceAddr:  instAddr,
		})
	}

	return verts, diags
}

// evaluateLocals evaluates all locals in a module in dependency order.
// Returns a map of local name → value for use during expansion.
func (e *expCtx) evaluateLocals(
	ctx context.Context,
	mod *configs.Module,
	moduleAddr addrs.ModuleInstance,
	repData instances.RepetitionData,
	varValues map[string]cty.Value,
) (map[string]cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	vals := make(map[string]cty.Value, len(mod.Locals))

	if len(mod.Locals) == 0 {
		return vals, nil
	}

	// Build dependency graph among locals.
	deps := make(map[string][]string)
	for name, lc := range mod.Locals {
		refs, _ := lang.ReferencesInExpr(addrs.ParseRef, lc.Expr)
		for _, ref := range refs {
			if localAddr, ok := ref.Subject.(addrs.LocalValue); ok {
				deps[name] = append(deps[name], localAddr.Name)
			}
		}
	}

	order := topoSortLocals(mod.Locals, deps)

	// One scope for the entire module instance. The expandData holds vals
	// by reference, so each iteration sees previously evaluated locals.
	scope := e.makeScope(mod, moduleAddr, repData, varValues, vals)

	for _, name := range order {
		lc := mod.Locals[name]
		if lc == nil || lc.Expr == nil {
			vals[name] = cty.DynamicVal
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[WARN] chofu/expand: panic evaluating local %s.%s: %v", moduleAddr, name, r)
					vals[name] = cty.DynamicVal
				}
			}()
			val, valDiags := scope.EvalExpr(ctx, lc.Expr, cty.DynamicPseudoType)
			diags = diags.Append(valDiags)
			if valDiags.HasErrors() {
				vals[name] = cty.DynamicVal
			} else {
				vals[name] = val
			}
		}()
	}

	return vals, diags
}

// makeScope creates a lang.Scope for expression evaluation during expansion.
func (e *expCtx) makeScope(
	mod *configs.Module,
	moduleAddr addrs.ModuleInstance,
	repData instances.RepetitionData,
	varValues map[string]cty.Value,
	locals map[string]cty.Value,
) *lang.Scope {
	data := &expandData{
		expCtx:     e,
		mod:        mod,
		moduleAddr: moduleAddr,
		repData:    repData,
		varValues:  varValues,
		locals:     locals,
	}
	return &lang.Scope{
		Data:              data,
		ParseRef:          addrs.ParseRef,
		BaseDir:           e.workDir,
		SharedFuncs:       e.sharedFuncs,
		ProviderFunctions: stubProviderFunction,
	}
}

// makeCtxFunc wraps a lang.Scope into an evalchecks.ContextFunc.
func makeCtxFunc(ctx context.Context, scope *lang.Scope) evalchecks.ContextFunc {
	return func(refs []*addrs.Reference) (*hcl.EvalContext, tfdiags.Diagnostics) {
		return scope.EvalContext(ctx, refs)
	}
}

// makeEvalFunc wraps a lang.Scope into an evalchecks.EvaluateFunc
// (used by EvaluateCountExpression).
func makeEvalFunc(ctx context.Context, scope *lang.Scope) evalchecks.EvaluateFunc {
	return func(expr hcl.Expression) (cty.Value, tfdiags.Diagnostics) {
		return scope.EvalExpr(ctx, expr, cty.DynamicPseudoType)
	}
}

// topoSortLocals returns local names in dependency order using Kahn's algorithm.
func topoSortLocals(locals map[string]*configs.Local, deps map[string][]string) []string {
	inDeg := make(map[string]int, len(locals))
	for name := range locals {
		inDeg[name] = 0
	}

	// Build reverse adjacency: depOf[x] = names that depend on x.
	depOf := make(map[string][]string, len(locals))
	for name, ds := range deps {
		count := 0
		for _, d := range ds {
			if _, ok := locals[d]; ok {
				count++
				depOf[d] = append(depOf[d], name)
			}
		}
		inDeg[name] = count
	}

	var queue []string
	for name, deg := range inDeg {
		if deg == 0 {
			queue = append(queue, name)
		}
	}
	slices.Sort(queue)

	order := make([]string, 0, len(locals))
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		order = append(order, name)

		for _, dependent := range depOf[name] {
			inDeg[dependent]--
			if inDeg[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	// Append any locals not reached (cycles or orphans).
	if len(order) < len(locals) {
		inOrder := make(map[string]bool, len(order))
		for _, name := range order {
			inOrder[name] = true
		}
		for name := range locals {
			if !inOrder[name] {
				order = append(order, name)
			}
		}
	}

	return order
}

// expandData implements lang.Data for the expansion phase.
// It supports variables, locals, path/terraform attrs, and count/each.
// Resource and module references return DynamicVal (not available during expansion).
type expandData struct {
	*expCtx
	mod        *configs.Module
	moduleAddr addrs.ModuleInstance
	repData    instances.RepetitionData
	varValues  map[string]cty.Value // this module's variable values, keyed by name
	locals     map[string]cty.Value
}

var _ lang.Data = (*expandData)(nil)

func (d *expandData) StaticValidateReferences(_ context.Context, _ []*addrs.Reference, _ addrs.Referenceable, _ addrs.Referenceable) tfdiags.Diagnostics {
	return nil
}

func (d *expandData) GetCountAttr(_ context.Context, addr addrs.CountAttr, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	if addr.Name == "index" && d.repData.CountIndex != cty.NilVal {
		return d.repData.CountIndex, nil
	}
	return cty.UnknownVal(cty.Number), nil
}

func (d *expandData) GetForEachAttr(_ context.Context, addr addrs.ForEachAttr, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	switch addr.Name {
	case "key":
		if d.repData.EachKey != cty.NilVal {
			return d.repData.EachKey, nil
		}
	case "value":
		if d.repData.EachValue != cty.NilVal {
			return d.repData.EachValue, nil
		}
	}
	return cty.DynamicVal, nil
}

func (d *expandData) GetInputVariable(_ context.Context, addr addrs.InputVariable, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	if val, ok := d.varValues[addr.Name]; ok {
		return val, nil
	}
	if vc, ok := d.mod.Variables[addr.Name]; ok && vc.Default != cty.NilVal {
		return vc.Default, nil
	}
	return cty.DynamicVal, nil
}

func (d *expandData) GetLocalValue(_ context.Context, addr addrs.LocalValue, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	if d.locals != nil {
		if val, ok := d.locals[addr.Name]; ok {
			return val, nil
		}
	}
	return cty.DynamicVal, nil
}

func (d *expandData) GetPathAttr(_ context.Context, addr addrs.PathAttr, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	switch addr.Name {
	case "cwd":
		return cty.StringVal(d.workDir), nil
	case "root":
		return cty.StringVal(d.rootDir), nil
	case "module":
		return cty.StringVal(d.mod.SourceDir), nil
	}
	return cty.DynamicVal, nil
}

func (d *expandData) GetTerraformAttr(_ context.Context, addr addrs.TerraformAttr, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	switch addr.Name {
	case "workspace":
		return cty.StringVal(d.workspace), nil
	case "env":
		return cty.StringVal(d.workspace), nil
	case "applying":
		return cty.False, nil
	}
	return cty.DynamicVal, nil
}

func (d *expandData) GetResource(_ context.Context, _ addrs.Resource, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return cty.DynamicVal, nil
}

func (d *expandData) GetModule(_ context.Context, _ addrs.ModuleCall, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return cty.DynamicVal, nil
}

func (d *expandData) GetOutput(_ context.Context, _ addrs.OutputValue, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return cty.DynamicVal, nil
}

func (d *expandData) GetCheckBlock(_ context.Context, _ addrs.Check, _ tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return cty.DynamicVal, nil
}
