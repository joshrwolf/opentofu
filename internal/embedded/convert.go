// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package embedded

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
	"github.com/zclconf/go-cty/cty/msgpack"

	"github.com/opentofu/opentofu/internal/tfdiags"
)

// tftypeToCtyType converts a tftypes.Type to a cty.Type directly.
// Both type systems represent the same Terraform protocol concepts;
// this is a straightforward recursive structural mapping.
func tftypeToCtyType(typ tftypes.Type) (cty.Type, error) {
	if typ == nil {
		return cty.NilType, nil
	}

	// Primitives — must check with .Is() since the underlying type is unexported.
	switch {
	case typ.Is(tftypes.String):
		return cty.String, nil
	case typ.Is(tftypes.Number):
		return cty.Number, nil
	case typ.Is(tftypes.Bool):
		return cty.Bool, nil
	case typ.Is(tftypes.DynamicPseudoType):
		return cty.DynamicPseudoType, nil
	}

	// Compound types — exported structs, so type assertion works.
	switch t := typ.(type) {
	case tftypes.List:
		elem, err := tftypeToCtyType(t.ElementType)
		if err != nil {
			return cty.NilType, err
		}
		return cty.List(elem), nil
	case tftypes.Set:
		elem, err := tftypeToCtyType(t.ElementType)
		if err != nil {
			return cty.NilType, err
		}
		return cty.Set(elem), nil
	case tftypes.Map:
		elem, err := tftypeToCtyType(t.ElementType)
		if err != nil {
			return cty.NilType, err
		}
		return cty.Map(elem), nil
	case tftypes.Tuple:
		elems := make([]cty.Type, len(t.ElementTypes))
		for i, et := range t.ElementTypes {
			var err error
			elems[i], err = tftypeToCtyType(et)
			if err != nil {
				return cty.NilType, err
			}
		}
		return cty.Tuple(elems), nil
	case tftypes.Object:
		attrs := make(map[string]cty.Type, len(t.AttributeTypes))
		for name, at := range t.AttributeTypes {
			var err error
			attrs[name], err = tftypeToCtyType(at)
			if err != nil {
				return cty.NilType, err
			}
		}
		return cty.Object(attrs), nil
	default:
		return cty.NilType, fmt.Errorf("unsupported tftypes.Type: %s", typ)
	}
}

// marshalValue encodes a cty.Value as a tfprotov6.DynamicValue using msgpack.
// Both cty and tftypes use identical msgpack wire formats for all value types
// relevant to provider communication (primitives, collections, nulls, unknowns).
func marshalValue(val cty.Value, ty cty.Type) (*tfprotov6.DynamicValue, error) {
	mp, err := msgpack.Marshal(val, ty)
	if err != nil {
		return nil, err
	}
	return &tfprotov6.DynamicValue{MsgPack: mp}, nil
}

// unmarshalValue decodes a tfprotov6.DynamicValue into a cty.Value.
// A nil DynamicValue is treated as a typed null (not cty.NilVal, which would
// panic on inspection). This matches GRPCProvider's decodeDynamicValue behavior.
func unmarshalValue(dv *tfprotov6.DynamicValue, ty cty.Type) (cty.Value, error) {
	if dv == nil {
		return cty.NullVal(ty), nil
	}
	if len(dv.MsgPack) > 0 {
		return msgpack.Unmarshal(dv.MsgPack, ty)
	}
	if len(dv.JSON) > 0 {
		return ctyjson.Unmarshal(dv.JSON, ty)
	}
	return cty.NullVal(ty), nil
}

// convertDiagnostics translates tfprotov6 diagnostics to tfdiags.Diagnostics.
func convertDiagnostics(ds []*tfprotov6.Diagnostic) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	for _, d := range ds {
		if d == nil {
			continue
		}

		var severity tfdiags.Severity
		switch d.Severity {
		case tfprotov6.DiagnosticSeverityWarning:
			severity = tfdiags.Warning
		default:
			severity = tfdiags.Error
		}

		if d.Attribute != nil {
			diags = diags.Append(tfdiags.AttributeValue(
				severity, d.Summary, d.Detail,
				attributePathToPath(d.Attribute),
			))
		} else {
			diags = diags.Append(tfdiags.WholeContainingBody(
				severity, d.Summary, d.Detail,
			))
		}
	}
	return diags
}

// attributePathToPath converts a tftypes.AttributePath to a cty.Path.
func attributePathToPath(ap *tftypes.AttributePath) cty.Path {
	if ap == nil {
		return nil
	}
	var p cty.Path
	for _, step := range ap.Steps() {
		switch s := step.(type) {
		case tftypes.AttributeName:
			p = p.GetAttr(string(s))
		case tftypes.ElementKeyString:
			p = p.Index(cty.StringVal(string(s)))
		case tftypes.ElementKeyInt:
			p = p.Index(cty.NumberIntVal(int64(s)))
		}
	}
	return p
}
