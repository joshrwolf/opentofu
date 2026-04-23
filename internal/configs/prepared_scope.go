// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package configs

import (
	"context"
	"maps"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// PreparedScope is a build-pipeline-specific evaluation context that
// pre-builds an hcl.EvalContext once per (module, instance-key) scope.
// Expressions evaluated through a PreparedScope reuse the cached context
// directly, bypassing the per-expression context rebuild in lang.Scope.
//
// Construction resolves all input variables, eligible locals, path and
// terraform attributes, and count/each values into a single hcl.EvalContext.
// Variables that cannot be resolved at prepare time (const=false) or locals
// that are ineligible for caching are recorded as skipped; expressions that
// reference them fall back to the standard evaluation path.
type PreparedScope struct {
	evaluator *StaticEvaluator
	opts      StaticEvalOptions

	ctx *hcl.EvalContext

	skippedVars   map[string]bool
	skippedLocals map[string]bool
	hasRuntime    bool
}

// Prepare builds a PreparedScope by eagerly resolving all statically
// available values in the module. The resulting scope can be reused for
// every expression evaluation within the same (module, instance-key) pair.
// Only structural data (vars, cache) is provided; runtime/provider-functions
// and count/each are layered on via WithOverlay.
func (s *StaticEvaluator) Prepare(ctx context.Context, vars StaticModuleVariables, cache EvalCache) (*PreparedScope, hcl.Diagnostics) {
	opts := StaticEvalOptions{
		Variables: vars,
		EvalCache: cache,
	}
	ps := &PreparedScope{
		evaluator:     s,
		opts:          opts,
		skippedVars:   make(map[string]bool),
		skippedLocals: make(map[string]bool),
	}

	ident := StaticIdentifier{
		Module:  s.call.addr,
		Subject: "prepared-scope",
	}
	scope := s.scopeWithOptions(ident, opts)

	// Resolve input variables. The vars closure may evaluate parent-module
	// expressions through the standard scope path, which is safe for static
	// scopes but could trigger solver calls in runtime scopes. We catch
	// errors and skip rather than risk blocking.
	variables := make(map[string]cty.Value, len(s.cfg.Variables))
	for name, variable := range s.cfg.Variables {
		if variable.ConstSet && !variable.Const {
			ps.skippedVars[name] = true
			variables[name] = cty.DynamicVal
			continue
		}
		val, diags := scope.Data.GetInputVariable(ctx, addrs.InputVariable{Name: name}, tfdiags.SourceRange{})
		if diags.HasErrors() {
			ps.skippedVars[name] = true
			variables[name] = cty.DynamicVal
			continue
		}
		variables[name] = val
	}

	// Resolve locals only from the EvalCache — never evaluate them here.
	// The first expression that references a local through the standard
	// scope path will populate the cache; subsequent PreparedScope builds
	// for sibling instances of the same module will find it cached.
	// This avoids solver deadlocks from evaluating impure locals that
	// reference runtime values.
	locals := make(map[string]cty.Value, len(s.cfg.Locals))
	for name := range s.cfg.Locals {
		if opts.EvalCache != nil {
			if val, ok := opts.EvalCache.Lookup(name); ok {
				locals[name] = val
				continue
			}
		}
		ps.skippedLocals[name] = true
		locals[name] = cty.DynamicVal
	}

	pathAttrs := make(map[string]cty.Value, 3)
	for _, name := range []string{"module", "root", "cwd"} {
		val, diags := scope.Data.GetPathAttr(ctx, addrs.PathAttr{Name: name}, tfdiags.SourceRange{})
		if !diags.HasErrors() {
			pathAttrs[name] = val
		}
	}

	terraformAttrs := make(map[string]cty.Value, 1)
	if val, diags := scope.Data.GetTerraformAttr(ctx, addrs.TerraformAttr{Name: "workspace"}, tfdiags.SourceRange{}); !diags.HasErrors() {
		terraformAttrs["workspace"] = val
	}

	hclVars := make(map[string]cty.Value, 8)
	if len(variables) > 0 {
		hclVars["var"] = cty.ObjectVal(variables)
	}
	if len(locals) > 0 {
		hclVars["local"] = cty.ObjectVal(locals)
	}
	if len(pathAttrs) > 0 {
		hclVars["path"] = cty.ObjectVal(pathAttrs)
	}
	if len(terraformAttrs) > 0 {
		hclVars["terraform"] = cty.ObjectVal(terraformAttrs)
		hclVars["tofu"] = cty.ObjectVal(terraformAttrs)
	}

	ps.ctx = &hcl.EvalContext{
		Functions: scope.Functions(),
		Variables: hclVars,
	}

	return ps, nil
}

// Evaluate evaluates a single HCL expression using the pre-built context.
// If the expression only references pre-resolved values, evaluation is a
// single expr.Value call with zero context-building overhead. Expressions
// referencing skipped variables, skipped locals, or provider functions
// fall back to the standard evaluation path.
func (p *PreparedScope) Evaluate(ctx context.Context, expr hcl.Expression, ident StaticIdentifier) (cty.Value, hcl.Diagnostics) {
	hclCtx, fallback := p.contextForExpr(ctx, expr)
	if fallback {
		return p.evaluator.EvaluateWithOptions(ctx, expr, ident, p.opts)
	}
	val, diags := expr.Value(hclCtx)
	if diags.HasErrors() {
		return p.evaluator.EvaluateWithOptions(ctx, expr, ident, p.opts)
	}
	return val, nil
}

// DecodeBlock decodes an HCL body against a schema using the pre-built
// context. Falls back to the standard path when the body references values
// not available in the prepared context.
func (p *PreparedScope) DecodeBlock(ctx context.Context, body hcl.Body, spec hcldec.Spec, ident StaticIdentifier) (cty.Value, hcl.Diagnostics) {
	refs, fallback := p.refsFromBody(body, spec)
	if fallback {
		return p.evaluator.DecodeBlockWithOptions(ctx, body, spec, ident, p.opts)
	}
	hclCtx, needsFallback := p.contextForRefs(ctx, refs)
	if needsFallback {
		return p.evaluator.DecodeBlockWithOptions(ctx, body, spec, ident, p.opts)
	}
	val, diags := hcldec.Decode(body, spec, hclCtx)
	if diags.HasErrors() {
		return p.evaluator.DecodeBlockWithOptions(ctx, body, spec, ident, p.opts)
	}
	return val, nil
}

// EvalContext returns an hcl.EvalContext suitable for the given set of
// references. When all references are pre-resolved, this returns the
// cached context (or a thin child with provider functions). Otherwise
// it falls back to the standard path.
func (p *PreparedScope) EvalContext(ctx context.Context, ident StaticIdentifier, refs []*addrs.Reference) (*hcl.EvalContext, hcl.Diagnostics) {
	hclCtx, fallback := p.contextForRefs(ctx, refs)
	if fallback {
		evalCtx, diags := p.evaluator.EvalContextWithOptions(ctx, ident, refs, p.opts)
		return evalCtx, diags
	}
	return hclCtx, nil
}

// Options returns the StaticEvalOptions this scope was prepared with.
func (p *PreparedScope) Options() StaticEvalOptions {
	return p.opts
}

// WithOverlay creates a new PreparedScope that layers evaluation-time
// context (runtime, provider functions) and per-instance repetition data
// (count/each) on top of the receiver's pre-built context. The base
// variable/local resolution is shared.
func (p *PreparedScope) WithOverlay(overlay EvalOverlay, count, forEach map[string]cty.Value) *PreparedScope {
	mergedOpts := p.opts
	if overlay.ProviderFunctions != nil {
		mergedOpts.ProviderFunctions = overlay.ProviderFunctions
	}
	if overlay.Runtime != nil {
		mergedOpts.Runtime = overlay.Runtime
	}
	if len(count) > 0 {
		mergedOpts.CountAttrs = count
	}
	if len(forEach) > 0 {
		mergedOpts.ForEachAttrs = forEach
	}

	needsNewCtx := len(count) > 0 || len(forEach) > 0
	if !needsNewCtx {
		return &PreparedScope{
			evaluator:     p.evaluator,
			opts:          mergedOpts,
			ctx:           p.ctx,
			skippedVars:   p.skippedVars,
			skippedLocals: p.skippedLocals,
			hasRuntime:    p.hasRuntime || overlay.Runtime != nil,
		}
	}

	hclVars := make(map[string]cty.Value, len(p.ctx.Variables)+2)
	maps.Copy(hclVars, p.ctx.Variables)
	if len(count) > 0 {
		hclVars["count"] = cty.ObjectVal(count)
	}
	if len(forEach) > 0 {
		hclVars["each"] = cty.ObjectVal(forEach)
	}

	return &PreparedScope{
		evaluator:     p.evaluator,
		opts:          mergedOpts,
		ctx:           &hcl.EvalContext{Functions: p.ctx.Functions, Variables: hclVars},
		skippedVars:   p.skippedVars,
		skippedLocals: p.skippedLocals,
		hasRuntime:    p.hasRuntime || overlay.Runtime != nil,
	}
}

// contextForExpr extracts references from an expression and returns the
// appropriate hcl.EvalContext. Returns fallback=true when the expression
// cannot be served by the prepared context.
func (p *PreparedScope) contextForExpr(ctx context.Context, expr hcl.Expression) (hclCtx *hcl.EvalContext, fallback bool) {
	refs, diags := lang.ReferencesInExpr(addrs.ParseRef, expr)
	if diags.HasErrors() {
		return nil, true
	}
	return p.contextForRefs(ctx, refs)
}

// refsFromBody extracts references from an HCL body + spec, including
// provider function traversals. Returns fallback=true on parse errors.
func (p *PreparedScope) refsFromBody(body hcl.Body, spec hcldec.Spec) ([]*addrs.Reference, bool) {
	traversals := hcldec.Variables(body, spec)
	for _, traversal := range hcldec.Functions(body, spec) {
		if len(traversal) == 0 {
			continue
		}
		root, ok := traversal[0].(hcl.TraverseRoot)
		if !ok {
			continue
		}
		if !addrs.ParseFunction(root.Name).IsNamespace(addrs.FunctionNamespaceProvider) {
			continue
		}
		traversals = append(traversals, traversal)
	}
	refs, diags := lang.References(addrs.ParseRef, traversals)
	if diags.HasErrors() {
		return nil, true
	}
	return refs, false
}

// contextForRefs checks whether all references are pre-resolved and returns
// the cached context (possibly wrapped in a child for provider functions
// and/or hydrated skipped variables/locals). Returns fallback=true when
// any reference cannot be served.
func (p *PreparedScope) contextForRefs(ctx context.Context, refs []*addrs.Reference) (hclCtx *hcl.EvalContext, fallback bool) {
	var providerFuncs []addrs.ProviderFunction
	var providerRanges []tfdiags.SourceRange
	var hydratedVars map[string]cty.Value
	var hydratedLocals map[string]cty.Value

	overlay := EvalOverlay{
		Runtime:           p.opts.Runtime,
		ProviderFunctions: p.opts.ProviderFunctions,
	}

	for _, ref := range refs {
		switch subj := ref.Subject.(type) {
		case addrs.InputVariable:
			if p.skippedVars[subj.Name] {
				if p.opts.Variables == nil {
					return nil, true
				}
				variable := p.evaluator.cfg.Variables[subj.Name]
				if variable == nil {
					return nil, true
				}
				val, diags := p.opts.Variables(ctx, variable, overlay)
				if diags.HasErrors() || !val.IsKnown() || val == cty.DynamicVal {
					return nil, true
				}
				if hydratedVars == nil {
					hydratedVars = make(map[string]cty.Value)
				}
				hydratedVars[subj.Name] = val
			}
		case addrs.LocalValue:
			if p.skippedLocals[subj.Name] {
				if p.opts.EvalCache == nil {
					return nil, true
				}
				val, ok := p.opts.EvalCache.Lookup(subj.Name)
				if !ok {
					return nil, true
				}
				if hydratedLocals == nil {
					hydratedLocals = make(map[string]cty.Value)
				}
				hydratedLocals[subj.Name] = val
			}
		case addrs.PathAttr, addrs.TerraformAttr:
			// Always pre-resolved.
		case addrs.CountAttr:
			if _, ok := p.opts.CountAttrs[subj.Name]; !ok {
				return nil, true
			}
		case addrs.ForEachAttr:
			if _, ok := p.opts.ForEachAttrs[subj.Name]; !ok {
				return nil, true
			}
		case addrs.ProviderFunction:
			if p.opts.ProviderFunctions == nil {
				return nil, true
			}
			providerFuncs = append(providerFuncs, subj)
			providerRanges = append(providerRanges, ref.SourceRange)
		case addrs.Resource, addrs.ResourceInstance,
			addrs.ModuleCall, addrs.ModuleCallInstance, addrs.ModuleCallOutput, addrs.ModuleCallInstanceOutput,
			addrs.OutputValue, addrs.Run, addrs.Check:
			return nil, true
		default:
			return nil, true
		}
	}

	needsChild := len(providerFuncs) > 0 || len(hydratedVars) > 0 || len(hydratedLocals) > 0
	if !needsChild {
		return p.ctx, false
	}

	childVars := p.ctx.Variables
	if len(hydratedVars) > 0 || len(hydratedLocals) > 0 {
		childVars = make(map[string]cty.Value, len(p.ctx.Variables))
		maps.Copy(childVars, p.ctx.Variables)

		if len(hydratedVars) > 0 {
			baseVars := map[string]cty.Value{}
			if existing, ok := childVars["var"]; ok && existing.IsKnown() && existing.Type().IsObjectType() {
				baseVars = existing.AsValueMap()
				if baseVars == nil {
					baseVars = map[string]cty.Value{}
				}
			}
			merged := make(map[string]cty.Value, len(baseVars)+len(hydratedVars))
			maps.Copy(merged, baseVars)
			maps.Copy(merged, hydratedVars)
			childVars["var"] = cty.ObjectVal(merged)
		}

		if len(hydratedLocals) > 0 {
			baseLocals := map[string]cty.Value{}
			if existing, ok := childVars["local"]; ok && existing.IsKnown() && existing.Type().IsObjectType() {
				baseLocals = existing.AsValueMap()
				if baseLocals == nil {
					baseLocals = map[string]cty.Value{}
				}
			}
			merged := make(map[string]cty.Value, len(baseLocals)+len(hydratedLocals))
			maps.Copy(merged, baseLocals)
			maps.Copy(merged, hydratedLocals)
			childVars["local"] = cty.ObjectVal(merged)
		}
	}

	childFuncs := p.ctx.Functions
	if len(providerFuncs) > 0 {
		childFuncs = make(map[string]function.Function, len(p.ctx.Functions)+len(providerFuncs))
		maps.Copy(childFuncs, p.ctx.Functions)
		for i, fn := range providerFuncs {
			resolved, fnDiags := p.opts.ProviderFunctions(ctx, fn, providerRanges[i])
			if fnDiags.HasErrors() {
				return nil, true
			}
			childFuncs[fn.String()] = *resolved
		}
	}

	return &hcl.EvalContext{
		Variables: childVars,
		Functions: childFuncs,
	}, false
}
