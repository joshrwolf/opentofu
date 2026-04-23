// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type ValueAdapter string

const (
	ValueAdapterRawV1      ValueAdapter = "raw-v1"
	ValueAdapterExternalV1 ValueAdapter = "external-v1"
	ValueAdapterNullV1     ValueAdapter = "null-v1"
)

func NormalizeValueAdapter(adapter ValueAdapter) ValueAdapter {
	if adapter == "" {
		return ValueAdapterRawV1
	}
	return adapter
}

func DecodeValue(req Request, payload []byte, adapter ValueAdapter, valueData []byte, rng tfdiags.SourceRange) (cty.Value, digest.Digest, tfdiags.Diagnostics) {
	adapter = NormalizeValueAdapter(adapter)

	result, diags := DecodeResult(req, payload, rng)
	if diags.HasErrors() {
		return cty.NilVal, digest.Digest{}, diags
	}

	switch adapter {
	case ValueAdapterRawV1:
		value, valueDiags := decodeRawValue(result, rng)
		if valueDiags.HasErrors() {
			return cty.NilVal, digest.Digest{}, valueDiags
		}
		return value, digest.FromBytes(payload), nil
	case ValueAdapterExternalV1:
		value, valueDiags := decodeExternalValue(result, rng)
		if valueDiags.HasErrors() {
			return cty.NilVal, digest.Digest{}, valueDiags
		}
		return value, digest.FromBytes(payload), nil
	case ValueAdapterNullV1:
		value, valueDigest, valueDiags := decodeNullValue(req, valueData, rng)
		if valueDiags.HasErrors() {
			return cty.NilVal, digest.Digest{}, valueDiags
		}
		return value, valueDigest, nil
	default:
		return cty.NilVal, digest.Digest{}, tfdiags.Diagnostics{}.Append(resultDiagnostic(
			rng,
			"Unsupported build run value adapter",
			fmt.Sprintf("Build run result uses unsupported value adapter %q.", adapter),
		))
	}
}

type nullValueEnvelope struct {
	Triggers map[string]string `json:"triggers,omitempty"`
}

func decodeRawValue(result Result, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	attrs := map[string]cty.Value{
		"exit_code": cty.NumberIntVal(int64(result.ExitCode)),
		"stdout":    cty.StringVal(string(result.Stdout)),
		"stderr":    cty.StringVal(string(result.Stderr)),
		"text":      cty.NullVal(cty.String),
		"json":      cty.NullVal(cty.DynamicPseudoType),
	}

	if result.Decoded != nil {
		switch result.Decoded.Mode {
		case DecodeModeText:
			attrs["text"] = cty.StringVal(result.Decoded.Text)
		case DecodeModeJSON:
			jsonValue, diags := jsonToCty(result.Decoded.JSON, rng)
			if diags.HasErrors() {
				return cty.NilVal, diags
			}
			attrs["json"] = jsonValue
		default:
			return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
				rng,
				"Invalid build run result",
				fmt.Sprintf("Build run result uses unsupported decoded mode %q.", result.Decoded.Mode),
			))
		}
	}

	return cty.ObjectVal(attrs), nil
}

func decodeExternalValue(result Result, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	if result.Decoded == nil || result.Decoded.Mode != DecodeModeJSON {
		return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
			rng,
			"Invalid build run result",
			"External compatibility targets must return a decoded JSON object with string values.",
		))
	}

	value, diags := jsonToCty(result.Decoded.JSON, rng)
	if diags.HasErrors() {
		return cty.NilVal, diags
	}
	if !value.IsKnown() || value.IsNull() {
		return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
			rng,
			"Invalid build run result",
			"External compatibility targets must return a JSON object with string values.",
		))
	}

	values := make(map[string]cty.Value)
	switch {
	case value.Type().IsObjectType():
		for key, elem := range value.AsValueMap() {
			if elem.IsNull() || !elem.IsKnown() || !elem.Type().Equals(cty.String) {
				return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
					rng,
					"Invalid build run result",
					"External compatibility targets must return a JSON object with string values.",
				))
			}
			values[key] = elem
		}
	case value.Type().IsMapType():
		if !value.Type().ElementType().Equals(cty.String) {
			return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
				rng,
				"Invalid build run result",
				"External compatibility targets must return a JSON object with string values.",
			))
		}
		iter := value.ElementIterator()
		for iter.Next() {
			key, elem := iter.Element()
			if elem.IsNull() || !elem.IsKnown() {
				return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
					rng,
					"Invalid build run result",
					"External compatibility targets must return a JSON object with string values.",
				))
			}
			values[key.AsString()] = elem
		}
	default:
		return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
			rng,
			"Invalid build run result",
			"External compatibility targets must return a JSON object with string values.",
		))
	}

	resultMap := cty.MapValEmpty(cty.String)
	if len(values) != 0 {
		resultMap = cty.MapVal(values)
	}
	return cty.ObjectVal(map[string]cty.Value{
		"id":     cty.StringVal("-"),
		"result": resultMap,
	}), nil
}

func decodeNullValue(req Request, valueData []byte, rng tfdiags.SourceRange) (cty.Value, digest.Digest, tfdiags.Diagnostics) {
	triggers := map[string]string{}
	if len(valueData) != 0 {
		var decoded nullValueEnvelope
		if err := json.Unmarshal(valueData, &decoded); err != nil {
			return cty.NilVal, digest.Digest{}, tfdiags.Diagnostics{}.Append(resultDiagnostic(
				rng,
				"Invalid build run value adapter data",
				fmt.Sprintf("Failed to decode null_resource compatibility value data: %s", err),
			))
		}
		triggers = decoded.Triggers
	}

	triggerValues := cty.MapValEmpty(cty.String)
	if len(triggers) != 0 {
		values := make(map[string]cty.Value, len(triggers))
		for key, value := range triggers {
			values[key] = cty.StringVal(value)
		}
		triggerValues = cty.MapVal(values)
	}

	valueDigest, err := digest.FromValue(struct {
		Request   digest.Digest
		ValueData digest.Digest
	}{
		Request:   req.Key,
		ValueData: digest.FromBytes(valueData),
	})
	if err != nil {
		return cty.NilVal, digest.Digest{}, tfdiags.Diagnostics{}.Append(resultDiagnostic(
			rng,
			"Invalid build run value adapter data",
			fmt.Sprintf("Failed to digest null_resource compatibility value data: %s", err),
		))
	}
	value := cty.ObjectVal(map[string]cty.Value{
		"id":       cty.StringVal(valueDigest.String()),
		"triggers": triggerValues,
	})
	return value, valueDigest, nil
}

func jsonToCty(src []byte, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	decoder := json.NewDecoder(bytes.NewReader(src))
	decoder.UseNumber()

	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
			rng,
			"Invalid build run result",
			fmt.Sprintf("Failed to decode JSON value payload: %s", err),
		))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
			rng,
			"Invalid build run result",
			"Failed to decode JSON value payload: trailing data after JSON value.",
		))
	}

	value, err := nativeToCty(decoded)
	if err != nil {
		return cty.NilVal, tfdiags.Diagnostics{}.Append(resultDiagnostic(
			rng,
			"Invalid build run result",
			fmt.Sprintf("Failed to decode JSON value payload: %s", err),
		))
	}
	return value, nil
}

func nativeToCty(value any) (cty.Value, error) {
	switch v := value.(type) {
	case nil:
		return cty.NullVal(cty.DynamicPseudoType), nil
	case bool:
		return cty.BoolVal(v), nil
	case string:
		return cty.StringVal(v), nil
	case json.Number:
		return cty.ParseNumberVal(v.String())
	case []any:
		values := make([]cty.Value, 0, len(v))
		for _, elem := range v {
			value, err := nativeToCty(elem)
			if err != nil {
				return cty.NilVal, err
			}
			values = append(values, value)
		}
		return cty.TupleVal(values), nil
	case map[string]any:
		values := make(map[string]cty.Value, len(v))
		for key, elem := range v {
			value, err := nativeToCty(elem)
			if err != nil {
				return cty.NilVal, err
			}
			values[key] = value
		}
		return cty.ObjectVal(values), nil
	default:
		return cty.NilVal, fmt.Errorf("unsupported JSON value type %T", value)
	}
}

func resultDiagnostic(rng tfdiags.SourceRange, summary, detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  summary,
		Detail:   detail,
		Subject:  rng.ToHCL().Ptr(),
	}
}
