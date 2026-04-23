// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"
	"fmt"
	"slices"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// moduleScope returns a cached PreparedScope for the given module path,
// creating it on demand. The scope is prepared statically (no runtime).
// Concurrent requests for the same module coalesce on a single preparation.
func (l *Loaded) moduleScope(ctx context.Context, module catalog.ModulePath) (*configs.PreparedScope, error) {
	key := module.Identity()

	l.scopeMu.Lock()
	if entry, ok := l.scopeCache[key]; ok {
		l.scopeMu.Unlock()
		<-entry.done
		return entry.scope, entry.err
	}
	entry := &scopeEntry{done: make(chan struct{})}
	l.scopeCache[key] = entry
	l.scopeMu.Unlock()

	var parentScope *configs.PreparedScope
	if module.Len() > 0 {
		var err error
		parentScope, err = l.moduleScope(ctx, module.Parent())
		if err != nil {
			entry.err = err
			close(entry.done)
			return nil, err
		}
	}

	scope, err := l.prepareModuleScope(ctx, module, parentScope)
	entry.scope = scope
	entry.err = err
	close(entry.done)
	return scope, err
}

// prepareModuleScope builds a PreparedScope for a module path given
// the parent module's cached scope. For the root module, parentScope is nil.
func (l *Loaded) prepareModuleScope(ctx context.Context, module catalog.ModulePath, parentScope *configs.PreparedScope) (*configs.PreparedScope, error) {
	if module.Len() == 0 {
		if l.Config == nil || l.Config.Module == nil || l.Config.Module.StaticEvaluator == nil {
			return nil, fmt.Errorf("root module does not have an HCL static evaluator")
		}
		cache := &evalCache{
			loaded:   l,
			module:   module,
			eligible: l.localEligibility(l.Config.Module),
		}
		ps, psDiags := l.Config.Module.StaticEvaluator.Prepare(ctx, nil, cache)
		if psDiags.HasErrors() {
			return nil, fmt.Errorf("preparing root scope: %s", psDiags.Error())
		}
		return ps, nil
	}

	if parentScope == nil {
		return nil, fmt.Errorf("module %s requires a parent scope", module)
	}

	parentOpts := parentScope.Options()
	opts, err := l.moduleEvalOptions(ctx, module, &parentOpts)
	if err != nil {
		return nil, err
	}

	moduleCfg := configForModulePath(l.Config, module)
	if moduleCfg == nil || moduleCfg.Module == nil || moduleCfg.Module.StaticEvaluator == nil {
		return nil, fmt.Errorf("module %s does not have an HCL static evaluator", module)
	}

	ps, psDiags := moduleCfg.Module.StaticEvaluator.Prepare(ctx, opts.Variables, opts.EvalCache)
	if psDiags.HasErrors() {
		return nil, fmt.Errorf("preparing scope for %s: %s", module, psDiags.Error())
	}
	return ps, nil
}

// moduleEvalOptions builds the base StaticEvalOptions for a module path.
// The returned options contain the Variables closure and EvalCache but no
// Runtime, ProviderFunctions, or count/each attrs — those are caller concerns.
//
// The Variables closure evaluates parent-module expressions using the parent's
// cached options (resolved via the solver's dice computation). It does not
// capture a RuntimeResolver, so the resulting options are runtime-agnostic
// and safe to cache across static and runtime evaluation passes.
func (l *Loaded) moduleEvalOptions(ctx context.Context, module catalog.ModulePath, parentOptions *configs.StaticEvalOptions) (configs.StaticEvalOptions, error) {
	if module.Len() == 0 {
		opts := configs.StaticEvalOptions{}
		if l.Config != nil && l.Config.Module != nil {
			opts.EvalCache = &evalCache{
				loaded:   l,
				module:   module,
				eligible: l.localEligibility(l.Config.Module),
			}
		}
		return opts, nil
	}

	if parentOptions == nil {
		return configs.StaticEvalOptions{}, fmt.Errorf("module %s requires parent options", module)
	}

	importDecl, ok := l.Catalog.Import(module.Declaration())
	if !ok {
		return configs.StaticEvalOptions{}, fmt.Errorf("unknown module import %s", catalog.ModuleAddr(module))
	}
	call, ok := importDecl.Payload.(*configs.ModuleCall)
	if !ok || call == nil {
		return configs.StaticEvalOptions{}, fmt.Errorf("module import %s is not backed by an HCL module call", catalog.ModuleAddr(module))
	}

	parentModule := module.Parent()
	parentCfg := configForModulePath(l.Config, parentModule)
	if parentCfg == nil || parentCfg.Module == nil || parentCfg.Module.StaticEvaluator == nil {
		return configs.StaticEvalOptions{}, fmt.Errorf("module %s does not have an HCL static evaluator", parentModule)
	}

	step, _ := module.LastStep()
	attr, _ := call.Config.JustAttributes()
	childPath := make(addrs.Module, len(parentCfg.Path)+1)
	copy(childPath, parentCfg.Path)
	childPath[len(parentCfg.Path)] = call.Name
	queryFunctions := buildprovider.DefaultQueryFunctionResolver()

	// Pre-compute call-site options for the static case. When the overlay
	// carries runtime, the closure recomputes them to pick up
	// runtime-dependent repetition attrs (e.g. for_each on a resource value).
	staticCallOptions := *parentOptions
	if step.Key.Kind != catalog.KeyKindNone {
		rep, err := l.moduleCallRepetitionOverlay(ctx, parentModule, step.Key, call, *parentOptions)
		if err != nil {
			return configs.StaticEvalOptions{}, err
		}
		staticCallOptions.CountAttrs = rep.CountAttrs
		staticCallOptions.ForEachAttrs = rep.ForEachAttrs
	}

	opts := configs.StaticEvalOptions{
		Variables: func(evalCtx context.Context, variable *configs.Variable, overlay configs.EvalOverlay) (cty.Value, hcl.Diagnostics) {
			v, ok := attr[variable.Name]
			if !ok {
				if variable.Required() {
					return cty.NilVal, hcl.Diagnostics{&hcl.Diagnostic{
						Severity: hcl.DiagError,
						Summary:  "Missing required variable in module call",
						Subject:  call.Config.MissingItemRange().Ptr(),
					}}
				}
				return variable.Default, nil
			}

			ident := configs.StaticIdentifier{
				Module:    childPath,
				Subject:   fmt.Sprintf("var.%s", variable.Name),
				DeclRange: v.Range,
			}

			varCallOptions := staticCallOptions
			varCallOptions.ProviderFunctions = queryFunctions.Resolve
			if overlay.Runtime != nil {
				varCallOptions.Runtime = rescopeRuntime(overlay.Runtime, parentModule)
				// Recompute repetition with runtime so for_each on
				// runtime values resolves to concrete each.key/each.value.
				if step.Key.Kind != catalog.KeyKindNone {
					rep, err := l.moduleCallRepetitionOverlay(evalCtx, parentModule, step.Key, call, varCallOptions)
					if err == nil {
						varCallOptions.CountAttrs = rep.CountAttrs
						varCallOptions.ForEachAttrs = rep.ForEachAttrs
					}
				}
			}
			if overlay.ProviderFunctions != nil {
				varCallOptions.ProviderFunctions = overlay.ProviderFunctions
			}
			return parentCfg.Module.StaticEvaluator.EvaluateWithOptions(evalCtx, v.Expr, ident, varCallOptions)
		},
	}

	moduleCfg := configForModulePath(l.Config, module)
	if moduleCfg != nil && moduleCfg.Module != nil {
		opts.EvalCache = &evalCache{
			loaded:   l,
			module:   module,
			eligible: l.localEligibility(moduleCfg.Module),
		}
	}

	return opts, nil
}

// moduleCallRepetitionOverlay evaluates the module call's count/for_each
// expression and returns the count/each attrs for the specific instance key.
func (l *Loaded) moduleCallRepetitionOverlay(ctx context.Context, parentModule catalog.ModulePath, key catalog.Key, call *configs.ModuleCall, parentOptions configs.StaticEvalOptions) (configs.StaticEvalOptions, error) {
	if key.Kind == catalog.KeyKindNone || call == nil {
		return configs.StaticEvalOptions{}, nil
	}

	parentCfg := configForModulePath(l.Config, parentModule)
	if parentCfg == nil || parentCfg.Module == nil || parentCfg.Module.StaticEvaluator == nil {
		return configs.StaticEvalOptions{}, fmt.Errorf("module %s does not have an HCL static evaluator", parentModule)
	}

	ident := configs.StaticIdentifier{
		Module:    parentCfg.Path,
		Subject:   catalog.ModuleAddr(parentModule.Child(call.Name, key)).String(),
		DeclRange: call.DeclRange,
	}
	parentInstanceKey := catalog.NoKey()
	if step, ok := parentModule.LastStep(); ok {
		parentInstanceKey = step.Key
	}
	repetition, diags := l.importRepetition(ctx, parentModule.Child(call.Name, catalog.NoKey()), call, parentModule, parentInstanceKey, ident, parentOptions)
	if diags.HasErrors() {
		return configs.StaticEvalOptions{}, wrapTfdiags(diags)
	}
	result, resultDiags := repetitionEvalOptionsFromResult(ident, key, repetition)
	if resultDiags.HasErrors() {
		return configs.StaticEvalOptions{}, wrapTfdiags(resultDiags)
	}
	return result, nil
}

func wrapTfdiags(diags tfdiags.Diagnostics) error {
	if !diags.HasErrors() {
		return nil
	}
	return diags.Err()
}

func (l *Loaded) targetEvalOptions(ctx context.Context, addr catalog.Addr, declRange hcl.Range, countExpr, forEachExpr, enabledExpr hcl.Expression, moduleOptions configs.StaticEvalOptions) (configs.StaticEvalOptions, tfdiags.Diagnostics) {
	repetition, repetitionDiags := l.actionableInstanceEvalOptions(ctx, addr, declRange, countExpr, forEachExpr, enabledExpr, moduleOptions)
	if repetitionDiags.HasErrors() {
		return configs.StaticEvalOptions{}, repetitionDiags
	}

	moduleOptions.CountAttrs = repetition.CountAttrs
	moduleOptions.ForEachAttrs = repetition.ForEachAttrs
	return moduleOptions, repetitionDiags
}

func (l *Loaded) actionableInstanceEvalOptions(ctx context.Context, addr catalog.Addr, declRange hcl.Range, countExpr, forEachExpr, enabledExpr hcl.Expression, moduleOptions configs.StaticEvalOptions) (configs.StaticEvalOptions, tfdiags.Diagnostics) {
	if addr.Key.Kind == catalog.KeyKindNone {
		return configs.StaticEvalOptions{}, nil
	}

	moduleCfg := configForModulePath(l.Config, addr.Module)
	if moduleCfg == nil || moduleCfg.Module == nil || moduleCfg.Module.StaticEvaluator == nil {
		return configs.StaticEvalOptions{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Module "+addr.Module.String()+" does not have an HCL static evaluator.",
		))
	}

	ident := configs.StaticIdentifier{
		Module:    moduleCfg.Path,
		Subject:   addr.String(),
		DeclRange: declRange,
	}
	declAddr := addr
	declAddr.Key = catalog.NoKey()
	repetition, diags := l.targetRepetition(ctx, declAddr, ident, moduleOptions, countExpr, forEachExpr, enabledExpr)
	if diags.HasErrors() {
		return configs.StaticEvalOptions{}, diags
	}
	return repetitionEvalOptionsFromResult(ident, addr.Key, repetition)
}

func repetitionEvalOptionsFromResult(ident configs.StaticIdentifier, key catalog.Key, repetition cachedRepetitionEval) (configs.StaticEvalOptions, tfdiags.Diagnostics) {
	switch repetition.kind {
	case repetitionKindCount:
		if key.Kind != catalog.KeyKindInt {
			return configs.StaticEvalOptions{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Count-based instance "+ident.Subject+" does not have an integer instance key.",
			))
		}
		if repetition.result.Deferred {
			return configs.StaticEvalOptions{
				CountAttrs: map[string]cty.Value{
					"index": cty.NumberIntVal(int64(key.Int)),
				},
			}, nil
		}
		if !slices.Contains(repetition.result.Keys, key) {
			return configs.StaticEvalOptions{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"Count-based instance "+ident.Subject+" does not have an element for key "+key.String()+".",
			))
		}
		return configs.StaticEvalOptions{
			CountAttrs: map[string]cty.Value{
				"index": cty.NumberIntVal(int64(key.Int)),
			},
		}, nil
	case repetitionKindForEach:
		if key.Kind != catalog.KeyKindString {
			return configs.StaticEvalOptions{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"For-each instance "+ident.Subject+" does not have a string instance key.",
			))
		}
		if repetition.result.Deferred {
			value := cty.DynamicVal
			if existing, ok := repetition.values[key.Str]; ok {
				value = existing
			}
			return configs.StaticEvalOptions{
				ForEachAttrs: map[string]cty.Value{
					"key":   cty.StringVal(key.Str),
					"value": value,
				},
			}, nil
		}
		value, ok := repetition.values[key.Str]
		if !ok {
			return configs.StaticEvalOptions{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build source error",
				"For-each instance "+ident.Subject+" does not have an element for key "+key.String()+".",
			))
		}
		return configs.StaticEvalOptions{
			ForEachAttrs: map[string]cty.Value{
				"key":   cty.StringVal(key.Str),
				"value": value,
			},
		}, nil
	case repetitionKindEnabled:
		return configs.StaticEvalOptions{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Optional instance "+ident.Subject+" does not have keyed repetition values.",
		))
	default:
		return configs.StaticEvalOptions{}, nil
	}
}

func cloneTfdiags(diags tfdiags.Diagnostics) tfdiags.Diagnostics {
	if len(diags) == 0 {
		return nil
	}
	ret := make(tfdiags.Diagnostics, len(diags))
	copy(ret, diags)
	return ret
}
