// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package configs

import (
	"context"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func newRuntimeScopeWithOptions(eval *StaticEvaluator, stack0 StaticIdentifier, opts StaticEvalOptions, stack ...StaticIdentifier) *lang.Scope {
	return &lang.Scope{
		Data: runtimeScopeData{
			eval:  eval,
			stack: append([]StaticIdentifier{stack0}, stack...),
			opts:  opts,
		},
		ParseRef:          addrs.ParseRef,
		BaseDir:           ".",
		PureOnly:          false,
		ConsoleMode:       false,
		ProviderFunctions: opts.ProviderFunctions,
	}
}

type runtimeScopeData struct {
	eval  *StaticEvaluator
	stack []StaticIdentifier
	opts  StaticEvalOptions
}

var _ lang.Data = (*runtimeScopeData)(nil)

func (s runtimeScopeData) scope(ident StaticIdentifier) (*lang.Scope, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	for _, frame := range s.stack {
		if frame.String() == ident.String() {
			return nil, diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Circular reference",
				Detail:   fmt.Sprintf("%s is self referential", ident.String()),
				Subject:  ident.DeclRange.Ptr(),
			})
		}
	}
	return newRuntimeScopeWithOptions(s.eval, s.stack[0], s.opts, append(s.stack[1:], ident)...), diags
}

func (s runtimeScopeData) enhanceDiagnostics(ident StaticIdentifier, diags tfdiags.Diagnostics) tfdiags.Diagnostics {
	if diags.HasErrors() {
		top := s.stack[len(s.stack)-1]
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  staticEvalUnavailableDependencySummary,
			Detail:   fmt.Sprintf("%s depends on %s which is not available", top, ident.String()),
			Subject:  top.DeclRange.Ptr(),
		})
	}
	return diags
}

func (s runtimeScopeData) StaticValidateReferences(_ context.Context, refs []*addrs.Reference, _ addrs.Referenceable, _ addrs.Referenceable) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	top := s.stack[len(s.stack)-1]
	for _, ref := range refs {
		switch subject := ref.Subject.(type) {
		case addrs.LocalValue, addrs.InputVariable, addrs.PathAttr, addrs.TerraformAttr:
			continue
		case addrs.CountAttr:
			if _, ok := s.opts.CountAttrs[subject.Name]; ok {
				continue
			}
		case addrs.ForEachAttr:
			if _, ok := s.opts.ForEachAttrs[subject.Name]; ok {
				continue
			}
		case addrs.Resource, addrs.ResourceInstance, addrs.ModuleCall, addrs.ModuleCallInstance, addrs.ModuleCallOutput, addrs.OutputValue, addrs.Run:
			continue
		case addrs.ModuleCallInstanceOutput:
			continue
		case addrs.ProviderFunction:
			if s.opts.ProviderFunctions != nil {
				continue
			}
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  staticEvalProviderFunctionSummary,
				Detail:   fmt.Sprintf("Unable to use %s in static context, which is required by %s", subject.String(), top.String()),
				Subject:  ref.SourceRange.ToHCL().Ptr(),
			})
		default:
			diags = diags.Append(&hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  staticEvalDynamicValueSummary,
				Detail:   fmt.Sprintf("Unable to use %s in static context, which is required by %s", subject.String(), top.String()),
				Subject:  ref.SourceRange.ToHCL().Ptr(),
			})
		}
	}
	return diags
}

func (s runtimeScopeData) GetCountAttr(ctx context.Context, addr addrs.CountAttr, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return staticScopeData{eval: s.eval, stack: s.stack, opts: s.opts}.GetCountAttr(ctx, addr, rng)
}

func (s runtimeScopeData) GetForEachAttr(ctx context.Context, addr addrs.ForEachAttr, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return staticScopeData{eval: s.eval, stack: s.stack, opts: s.opts}.GetForEachAttr(ctx, addr, rng)
}

func (s runtimeScopeData) GetResource(ctx context.Context, addr addrs.Resource, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	if s.opts.Runtime == nil {
		return cty.DynamicVal, nil
	}
	result := s.opts.Runtime.GetResource(ctx, addr, rng)
	if result.Known {
		return result.Value, result.Diags
	}
	return cty.DynamicVal, s.unavailable(addr.String(), rng, result.Diags)
}

func (s runtimeScopeData) GetLocalValue(ctx context.Context, ident addrs.LocalValue, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	local, ok := s.eval.cfg.Locals[ident.Name]
	if !ok {
		return cty.DynamicVal, diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Undefined local",
			Detail:   fmt.Sprintf("Undefined local %s", ident.String()),
			Subject:  rng.ToHCL().Ptr(),
		})
	}

	if c := s.opts.EvalCache; c != nil {
		if val, ok := c.Lookup(ident.Name); ok {
			return val, nil
		}
	}

	id := StaticIdentifier{
		Module:    s.eval.call.addr,
		Subject:   fmt.Sprintf("local.%s", local.Name),
		DeclRange: local.DeclRange,
	}

	scope, scopeDiags := s.scope(id)
	diags = diags.Append(scopeDiags)
	if diags.HasErrors() {
		return cty.DynamicVal, diags
	}

	val, valDiags := scope.EvalExpr(ctx, local.Expr, cty.DynamicPseudoType)
	if c := s.opts.EvalCache; c != nil && !valDiags.HasErrors() {
		c.Store(ident.Name, val)
	}
	return val, s.enhanceDiagnostics(id, diags.Append(valDiags))
}

func (s runtimeScopeData) GetModule(ctx context.Context, addr addrs.ModuleCall, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	if s.opts.Runtime == nil {
		return cty.DynamicVal, nil
	}
	result := s.opts.Runtime.GetModule(ctx, addr, rng)
	if result.Known {
		return result.Value, result.Diags
	}
	return cty.DynamicVal, s.unavailable(addr.String(), rng, result.Diags)
}

func (s runtimeScopeData) GetRun(ctx context.Context, addr addrs.Run, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	if s.opts.Runtime == nil {
		return cty.DynamicVal, nil
	}
	result := s.opts.Runtime.GetRun(ctx, addr, rng)
	if result.Known {
		return result.Value, result.Diags
	}
	return cty.DynamicVal, s.unavailable(addr.String(), rng, result.Diags)
}

func (s runtimeScopeData) GetPathAttr(ctx context.Context, addr addrs.PathAttr, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return staticScopeData{eval: s.eval, stack: s.stack, opts: s.opts}.GetPathAttr(ctx, addr, rng)
}

func (s runtimeScopeData) GetTerraformAttr(ctx context.Context, addr addrs.TerraformAttr, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return staticScopeData{eval: s.eval, stack: s.stack, opts: s.opts}.GetTerraformAttr(ctx, addr, rng)
}

func (s runtimeScopeData) GetInputVariable(ctx context.Context, ident addrs.InputVariable, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return staticScopeData{eval: s.eval, stack: s.stack, opts: s.opts}.GetInputVariable(ctx, ident, rng)
}

func (s runtimeScopeData) GetOutput(ctx context.Context, addr addrs.OutputValue, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	if s.opts.Runtime == nil {
		return cty.DynamicVal, nil
	}
	result := s.opts.Runtime.GetOutput(ctx, addr, rng)
	if result.Known {
		return result.Value, result.Diags
	}
	return cty.DynamicVal, s.unavailable(addr.String(), rng, result.Diags)
}

func (s runtimeScopeData) GetCheckBlock(ctx context.Context, addr addrs.Check, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	return staticScopeData{eval: s.eval, stack: s.stack, opts: s.opts}.GetCheckBlock(ctx, addr, rng)
}

func (s runtimeScopeData) unavailable(subject string, rng tfdiags.SourceRange, diags tfdiags.Diagnostics) tfdiags.Diagnostics {
	top := s.stack[len(s.stack)-1]
	diags = diags.Append(&hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  staticEvalUnavailableDependencySummary,
		Detail:   fmt.Sprintf("%s depends on %s which is not available", top.String(), subject),
		Subject:  rng.ToHCL().Ptr(),
	})
	return diags
}
