// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package configs

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/lang/marks"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

// StaticIdentifier holds a Referenceable item and where it was declared
type StaticIdentifier struct {
	Module    addrs.Module
	Subject   string
	DeclRange hcl.Range
}

func (ref StaticIdentifier) String() string {
	val := ref.Subject
	if len(ref.Module) != 0 {
		val = ref.Module.String() + ":" + val
	}
	return val
}

// EvalOverlay carries evaluation-time context that flows through the
// Variables closure and is applied via PreparedScope.WithOverlay.
// It contains runtime/provider-functions but NOT count/each (those are
// lexically scoped and must not leak into parent expression evaluation).
type EvalOverlay struct {
	Runtime           RuntimeValueLookup
	ProviderFunctions lang.ProviderFunction
}

type StaticModuleVariables func(ctx context.Context, v *Variable, overlay EvalOverlay) (cty.Value, hcl.Diagnostics)

// StaticModuleCall contains the information required to call a given module
type StaticModuleCall struct {
	addr      addrs.Module
	declRange hcl.Range
	vars      StaticModuleVariables
	rootPath  string
	workspace string
}

func NewStaticModuleCall(addr addrs.Module, declRange hcl.Range, vars StaticModuleVariables, rootPath string, workspace string) StaticModuleCall {
	return StaticModuleCall{
		addr:      addr,
		declRange: declRange,
		vars:      vars,
		rootPath:  rootPath,
		workspace: workspace,
	}
}

func (s StaticModuleCall) Variables() StaticModuleVariables {
	return s.vars
}

func (s StaticModuleCall) WithVariables(vars StaticModuleVariables) StaticModuleCall {
	return StaticModuleCall{
		addr:      s.addr,
		declRange: s.declRange,
		vars:      vars,
		rootPath:  s.rootPath,
		workspace: s.workspace,
	}
}

// only used in testing
func RootModuleCallForTesting() StaticModuleCall {
	return NewStaticModuleCall(addrs.RootModule, hcl.Range{}, func(_ context.Context, _ *Variable, _ EvalOverlay) (cty.Value, hcl.Diagnostics) {
		panic("Variables have not been configured for this test!")
	}, "<testing>", "")
}

// A static evaluator contains the information required to build a EvalContext
// which only understands "static" (non-state) data. Internally, it relies
// on staticData
type StaticEvaluator struct {
	call StaticModuleCall
	cfg  *Module
}

type EvalCache interface {
	Lookup(name string) (cty.Value, bool)
	Store(name string, val cty.Value)
}

type StaticEvalOptions struct {
	Runtime           RuntimeValueLookup
	ProviderFunctions lang.ProviderFunction
	Variables         StaticModuleVariables
	CountAttrs        map[string]cty.Value
	ForEachAttrs      map[string]cty.Value
	EvalCache         EvalCache
}

type RuntimeLookupResult struct {
	Value cty.Value
	Known bool
	Diags tfdiags.Diagnostics
}

type RuntimeValueLookup interface {
	GetResource(context.Context, addrs.Resource, tfdiags.SourceRange) RuntimeLookupResult
	GetModule(context.Context, addrs.ModuleCall, tfdiags.SourceRange) RuntimeLookupResult
	GetRun(context.Context, addrs.Run, tfdiags.SourceRange) RuntimeLookupResult
	GetOutput(context.Context, addrs.OutputValue, tfdiags.SourceRange) RuntimeLookupResult
}

const (
	staticEvalDynamicValueSummary          = "Dynamic value in static context"
	staticEvalModuleOutputSummary          = "Module output not supported in static context"
	staticEvalProviderFunctionSummary      = "Provider function in static context"
	staticEvalUnavailableDependencySummary = "Unable to compute static value"
)

// Creates a static evaluator based from the given module and module call
func NewStaticEvaluator(mod *Module, call StaticModuleCall) *StaticEvaluator {
	return &StaticEvaluator{
		call: call,
		cfg:  mod,
	}
}

func (s *StaticEvaluator) scope(ident StaticIdentifier) *lang.Scope {
	return s.scopeWithOptions(ident, StaticEvalOptions{})
}

func (s *StaticEvaluator) scopeWithOptions(ident StaticIdentifier, opts StaticEvalOptions) *lang.Scope {
	if opts.Runtime != nil {
		return newRuntimeScopeWithOptions(s, ident, opts)
	}
	return newStaticScopeWithOptions(s, ident, opts)
}

func (s StaticEvaluator) Evaluate(ctx context.Context, expr hcl.Expression, ident StaticIdentifier) (cty.Value, hcl.Diagnostics) {
	return s.EvaluateWithOptions(ctx, expr, ident, StaticEvalOptions{})
}


func (s StaticEvaluator) EvaluateWithOptions(ctx context.Context, expr hcl.Expression, ident StaticIdentifier, opts StaticEvalOptions) (cty.Value, hcl.Diagnostics) {
	val, diags := s.scopeWithOptions(ident, opts).EvalExpr(ctx, expr, cty.DynamicPseudoType)
	return val, diags.ToHCL()
}

func (s StaticEvaluator) DecodeExpression(ctx context.Context, expr hcl.Expression, ident StaticIdentifier, val any) hcl.Diagnostics {
	srcVal, diags := s.Evaluate(ctx, expr, ident)
	if diags.HasErrors() {
		return diags
	}

	if marks.Contains(srcVal, marks.Sensitive) {
		return diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Sensitive value not allowed",
			Detail:   fmt.Sprintf("Sensitive values, or values derived from sensitive values, cannot be used as %s.", ident.String()),
			Subject:  expr.Range().Ptr(),
		})
	}
	if marks.Contains(srcVal, marks.Ephemeral) {
		return diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Ephemeral value not allowed",
			Detail:   fmt.Sprintf("Ephemeral values, or values derived from ephemeral values, cannot be used as %s.", ident.String()),
			Subject:  expr.Range().Ptr(),
		})
	}

	return diags.Extend(gohcl.DecodeValue(srcVal, expr.StartRange(), expr.Range(), val))
}

func (s StaticEvaluator) DecodeBlock(ctx context.Context, body hcl.Body, spec hcldec.Spec, ident StaticIdentifier) (cty.Value, hcl.Diagnostics) {
	return s.DecodeBlockWithOptions(ctx, body, spec, ident, StaticEvalOptions{})
}


func (s StaticEvaluator) DecodeBlockWithOptions(ctx context.Context, body hcl.Body, spec hcldec.Spec, ident StaticIdentifier, opts StaticEvalOptions) (cty.Value, hcl.Diagnostics) {
	var diags hcl.Diagnostics

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

	refs, refsDiags := lang.References(addrs.ParseRef, traversals)
	diags = append(diags, refsDiags.ToHCL()...)
	if diags.HasErrors() {
		return cty.DynamicVal, diags
	}

	hclCtx, ctxDiags := s.EvalContextWithOptions(ctx, ident, refs, opts)
	diags = append(diags, ctxDiags...)
	if diags.HasErrors() {
		return cty.DynamicVal, diags
	}

	val, valDiags := hcldec.Decode(body, spec, hclCtx)
	diags = append(diags, valDiags...)
	return val, diags
}

func (s StaticEvaluator) EvalContext(ctx context.Context, ident StaticIdentifier, refs []*addrs.Reference) (*hcl.EvalContext, hcl.Diagnostics) {
	return s.EvalContextWithOptions(ctx, ident, refs, StaticEvalOptions{})
}


func (s StaticEvaluator) EvalContextWithParent(ctx context.Context, parent *hcl.EvalContext, ident StaticIdentifier, refs []*addrs.Reference) (*hcl.EvalContext, hcl.Diagnostics) {
	evalCtx, diags := s.scopeWithOptions(ident, StaticEvalOptions{}).EvalContextWithParent(ctx, parent, refs)
	return evalCtx, diags.ToHCL()
}

func (s StaticEvaluator) EvalContextWithOptions(ctx context.Context, ident StaticIdentifier, refs []*addrs.Reference, opts StaticEvalOptions) (*hcl.EvalContext, hcl.Diagnostics) {
	evalCtx, diags := s.scopeWithOptions(ident, opts).EvalContext(ctx, refs)
	return evalCtx, diags.ToHCL()
}

func StaticEvalDefers(diags hcl.Diagnostics) bool {
	if len(diags) == 0 {
		return false
	}
	for _, diag := range diags {
		if diag == nil {
			continue
		}
		switch diag.Summary {
		case staticEvalDynamicValueSummary,
			staticEvalModuleOutputSummary,
			staticEvalProviderFunctionSummary,
			staticEvalUnavailableDependencySummary:
			continue
		default:
			return false
		}
	}
	return true
}
