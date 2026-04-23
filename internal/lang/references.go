// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package lang

import (
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/lang/blocktoattr"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// References finds all of the references in the given set of traversals,
// returning diagnostics if any of the traversals cannot be interpreted as a
// reference.
//
// This function does not do any de-duplication of references, since references
// have source location information embedded in them and so any invalid
// references that are duplicated should have errors reported for each
// occurrence.
//
// If the returned diagnostics contains errors then the result may be
// incomplete or invalid. Otherwise, the returned slice has one reference per
// given traversal, though it is not guaranteed that the references will
// appear in the same order as the given traversals.
func References(parseRef ParseRef, traversals []hcl.Traversal) ([]*addrs.Reference, tfdiags.Diagnostics) {
	if len(traversals) == 0 {
		return nil, nil
	}

	var diags tfdiags.Diagnostics
	refs := make([]*addrs.Reference, 0, len(traversals))

	for _, traversal := range traversals {
		ref, refDiags := parseRef(traversal)
		diags = diags.Append(refDiags)
		if ref == nil {
			continue
		}
		refs = append(refs, ref)
	}

	return refs, diags
}

// ReferencesInBlock is a helper wrapper around References that first searches
// the given body for traversals, before converting those traversals to
// references.
//
// A block schema must be provided so that this function can determine where in
// the body variables are expected.
func ReferencesInBlock(parseRef ParseRef, body hcl.Body, schema *configschema.Block) ([]*addrs.Reference, tfdiags.Diagnostics) {
	if body == nil {
		return nil, nil
	}

	// We use blocktoattr.ExpandedVariables instead of hcldec.Variables or
	// dynblock.VariablesHCLDec here because when we evaluate a block we'll
	// first apply the dynamic block extension and _then_ the blocktoattr
	// transform, and so blocktoattr.ExpandedVariables takes into account
	// both of those transforms when it analyzes the body to ensure we find
	// all of the references as if they'd already moved into their final
	// locations, even though we can't expand dynamic blocks yet until we
	// already know which variables are required.
	//
	// The set of cases we want to detect here is covered by the tests for
	// the plan graph builder in the main 'tofu' package, since it's
	// in a better position to test this due to having mock providers etc
	// available.
	traversals := blocktoattr.ExpandedVariables(body, schema)
	funcs := filterProviderFunctions(blocktoattr.ExpandedFunctions(body, schema))

	return References(parseRef, append(traversals, funcs...))
}

// ReferencesInExpr is a helper wrapper around References that first searches
// the given expression for traversals, before converting those traversals
// to references.
func ReferencesInExpr(parseRef ParseRef, expr hcl.Expression) ([]*addrs.Reference, tfdiags.Diagnostics) {
	if expr == nil {
		return nil, nil
	}
	traversals := expr.Variables()
	ignored := exprLocalTraversalRanges(expr)
	if fexpr, ok := expr.(hcl.ExpressionWithFunctions); ok {
		funcs := filterProviderFunctions(fexpr.Functions())
		traversals = append(traversals, funcs...)
	}
	refs, diags := References(parseRef, traversals)
	if len(ignored) == 0 {
		return refs, diags
	}

	filtered := make(tfdiags.Diagnostics, 0, len(diags))
	for _, diag := range diags {
		desc := diag.Description()
		src := diag.Source()
		if src.Subject == nil || desc.Summary != "Invalid reference" {
			filtered = append(filtered, diag)
			continue
		}
		if _, ok := ignored[*src.Subject]; ok {
			continue
		}
		filtered = append(filtered, diag)
	}
	return refs, filtered
}

// ProviderFunctionsInExpr is a helper wrapper around References that searches for provider
// function traversals in an ExpressionWithFunctions, then converts the traversals into
// references
func ProviderFunctionsInExpr(parseRef ParseRef, expr hcl.Expression) ([]*addrs.Reference, tfdiags.Diagnostics) {
	if expr == nil {
		return nil, nil
	}
	if fexpr, ok := expr.(hcl.ExpressionWithFunctions); ok {
		funcs := filterProviderFunctions(fexpr.Functions())
		return References(parseRef, funcs)
	}
	return nil, nil
}

func filterProviderFunctions(funcs []hcl.Traversal) []hcl.Traversal {
	pfuncs := make([]hcl.Traversal, 0, len(funcs))
	for _, fn := range funcs {
		if len(fn) == 0 {
			continue
		}
		if root, ok := fn[0].(hcl.TraverseRoot); ok {
			if addrs.ParseFunction(root.Name).IsNamespace(addrs.FunctionNamespaceProvider) {
				pfuncs = append(pfuncs, fn)
			}
		}
	}
	return pfuncs
}

func exprLocalTraversalRanges(expr hcl.Expression) map[tfdiags.SourceRange]struct{} {
	node, ok := expr.(hclsyntax.Node)
	if !ok {
		return nil
	}

	scopes := collectForExprScopes(node)
	if len(scopes) == 0 {
		return nil
	}

	ranges := make(map[tfdiags.SourceRange]struct{})
	for _, traversal := range expr.Variables() {
		if len(traversal) == 0 {
			continue
		}
		root := traversal.RootName()
		sourceRange := traversal.SourceRange()
		for _, scope := range scopes {
			if _, ok := scope.locals[root]; !ok {
				continue
			}
			if !scope.contains(sourceRange) {
				continue
			}
			ranges[tfdiags.SourceRangeFromHCL(sourceRange)] = struct{}{}
			break
		}
	}

	return ranges
}

type forExprScope struct {
	locals map[string]struct{}
	ranges []hcl.Range
}

func (s forExprScope) contains(rng hcl.Range) bool {
	for _, candidate := range s.ranges {
		if rangeContains(candidate, rng) {
			return true
		}
	}
	return false
}

func rangeContains(outer, inner hcl.Range) bool {
	if outer.Filename != inner.Filename {
		return false
	}
	if outer.Start.Byte > inner.Start.Byte {
		return false
	}
	if outer.End.Byte < inner.End.Byte {
		return false
	}
	return true
}

func collectForExprScopes(node hclsyntax.Node) []forExprScope {
	var scopes []forExprScope
	_ = hclsyntax.Walk(node, forExprScopeWalker{onFor: func(expr *hclsyntax.ForExpr) {
		locals := make(map[string]struct{}, 2)
		if expr.KeyVar != "" {
			locals[expr.KeyVar] = struct{}{}
		}
		if expr.ValVar != "" {
			locals[expr.ValVar] = struct{}{}
		}
		if len(locals) == 0 {
			return
		}

		ranges := make([]hcl.Range, 0, 3)
		if expr.KeyExpr != nil {
			ranges = append(ranges, expr.KeyExpr.Range())
		}
		if expr.ValExpr != nil {
			ranges = append(ranges, expr.ValExpr.Range())
		}
		if expr.CondExpr != nil {
			ranges = append(ranges, expr.CondExpr.Range())
		}
		if len(ranges) == 0 {
			return
		}

		scopes = append(scopes, forExprScope{
			locals: locals,
			ranges: ranges,
		})
	}})
	return scopes
}

type forExprScopeWalker struct {
	onFor func(*hclsyntax.ForExpr)
}

func (w forExprScopeWalker) Enter(node hclsyntax.Node) hcl.Diagnostics {
	if w.onFor == nil {
		return nil
	}
	forExpr, ok := node.(*hclsyntax.ForExpr)
	if !ok {
		return nil
	}
	w.onFor(forExpr)
	return nil
}

func (w forExprScopeWalker) Exit(hclsyntax.Node) hcl.Diagnostics {
	return nil
}
