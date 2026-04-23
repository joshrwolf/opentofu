// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package configs

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/opentofu/opentofu/internal/addrs"
)

// Run represents a native "run" block in a module or file.
type Run struct {
	Name      string
	Config    hcl.Body
	Count     hcl.Expression
	Enabled   hcl.Expression
	ForEach   hcl.Expression
	DependsOn []hcl.Traversal
	DeclRange hcl.Range
}

func (r *Run) Addr() addrs.Run {
	return addrs.Run{Name: r.Name}
}

func (r *Run) moduleUniqueKey() string {
	return r.Addr().String()
}

func decodeRunBlock(block *hcl.Block, _ bool) (*Run, hcl.Diagnostics) {
	r := &Run{
		Name:      block.Labels[0],
		DeclRange: block.DefRange,
	}

	content, remain, diags := block.Body.PartialContent(runBlockSchema)
	r.Config = remain

	if !hclsyntax.ValidIdentifier(r.Name) {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid run block name",
			Detail:   badIdentifierDetail,
			Subject:  &block.LabelRanges[0],
		})
	}

	repetitionArgs := 0
	var countRng, forEachRng, enabledRng hcl.Range
	if attr, exists := content.Attributes["count"]; exists {
		r.Count = attr.Expr
		countRng = attr.NameRange
		repetitionArgs++
	}
	if attr, exists := content.Attributes["for_each"]; exists {
		r.ForEach = attr.Expr
		forEachRng = attr.NameRange
		repetitionArgs++
	}
	if attr, exists := content.Attributes["depends_on"]; exists {
		deps, depsDiags := decodeDependsOn(attr)
		diags = append(diags, depsDiags...)
		r.DependsOn = append(r.DependsOn, deps...)
	}

	var seenLifecycle *hcl.Block
	for _, block := range content.Blocks {
		switch block.Type {
		case "lifecycle":
			if seenLifecycle != nil {
				diags = append(diags, &hcl.Diagnostic{
					Severity: hcl.DiagError,
					Summary:  "Duplicate lifecycle block",
					Detail:   fmt.Sprintf("This run block already has a lifecycle block at %s.", seenLifecycle.DefRange),
					Subject:  &block.DefRange,
				})
				continue
			}
			seenLifecycle = block

			lcContent, lcDiags := block.Body.Content(runLifecycleBlockSchema)
			diags = append(diags, lcDiags...)
			if attr, exists := lcContent.Attributes["enabled"]; exists {
				r.Enabled = attr.Expr
				enabledRng = attr.NameRange
				repetitionArgs++
			}
		default:
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Reserved block type name in run block",
				Detail:   fmt.Sprintf("The block type name %q is reserved for use by OpenTofu in a future version.", block.Type),
				Subject:  &block.TypeRange,
			})
		}
	}

	if repetitionArgs >= 2 {
		complainRng, complainMsg := complainRngAndMsg(countRng, enabledRng, forEachRng)
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  fmt.Sprintf(`Invalid combination of %s`, complainMsg),
			Detail:   fmt.Sprintf(`The %s meta-arguments are mutually-exclusive. Only one should be used to be explicit about the number of run instances to be created.`, complainMsg),
			Subject:  complainRng,
		})
	}

	return r, diags
}

var runBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "count"},
		{Name: "for_each"},
		{Name: "depends_on"},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "lifecycle"},
	},
}

var runLifecycleBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "enabled"},
	},
}
