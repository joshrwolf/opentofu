// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package run

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

func TestDecodeValueRawAdapter(t *testing.T) {
	req := mustBuildTestRequest(t)
	payload, err := EncodeResult(req, Result{
		ExitCode: 0,
		Stdout:   []byte("{\"value\":\"hello\",\"count\":2}\n"),
		Stderr:   []byte("stderr\n"),
		Decoded: &DecodedValue{
			Mode: DecodeModeJSON,
			JSON: []byte("{\"value\":\"hello\",\"count\":2}\n"),
		},
	})
	if err != nil {
		t.Fatalf("unexpected encode error: %s", err)
	}

	value, valueDigest, diags := DecodeValue(req, payload, ValueAdapterRawV1, nil, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if valueDigest == (digest.Digest{}) {
		t.Fatal("expected non-zero digest")
	}

	if got, want := value.GetAttr("exit_code"), cty.NumberIntVal(0); !got.RawEquals(want) {
		t.Fatalf("wrong exit_code: got %s want %s", got.GoString(), want.GoString())
	}
	if got, want := value.GetAttr("stdout"), cty.StringVal("{\"value\":\"hello\",\"count\":2}\n"); !got.RawEquals(want) {
		t.Fatalf("wrong stdout: got %s want %s", got.GoString(), want.GoString())
	}
	if got, want := value.GetAttr("stderr"), cty.StringVal("stderr\n"); !got.RawEquals(want) {
		t.Fatalf("wrong stderr: got %s want %s", got.GoString(), want.GoString())
	}
	if got, want := value.GetAttr("json"), cty.ObjectVal(map[string]cty.Value{
		"count": cty.NumberIntVal(2),
		"value": cty.StringVal("hello"),
	}); !got.RawEquals(want) {
		t.Fatalf("wrong json value: got %s want %s", got.GoString(), want.GoString())
	}
}

func TestDecodeValueRawAdapterPreservesLargeJSONNumbers(t *testing.T) {
	req := mustBuildTestRequest(t)
	payload, err := EncodeResult(req, Result{
		ExitCode: 0,
		Stdout:   []byte("{\"big\":9007199254740993,\"decimal\":1.25}\n"),
		Decoded: &DecodedValue{
			Mode: DecodeModeJSON,
			JSON: []byte("{\"big\":9007199254740993,\"decimal\":1.25}\n"),
		},
	})
	if err != nil {
		t.Fatalf("unexpected encode error: %s", err)
	}

	value, _, diags := DecodeValue(req, payload, ValueAdapterRawV1, nil, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}

	if got, want := value.GetAttr("json"), cty.ObjectVal(map[string]cty.Value{
		"big":     cty.MustParseNumberVal("9007199254740993"),
		"decimal": cty.MustParseNumberVal("1.25"),
	}); !got.RawEquals(want) {
		t.Fatalf("wrong json value: got %s want %s", got.GoString(), want.GoString())
	}
}

func TestDecodeValueExternalAdapter(t *testing.T) {
	req := mustBuildTestRequest(t)
	payload, err := EncodeResult(req, Result{
		ExitCode: 0,
		Stdout:   []byte("{\"image_ref\":\"cgr.dev/example:latest\"}\n"),
		Decoded: &DecodedValue{
			Mode: DecodeModeJSON,
			JSON: []byte("{\"image_ref\":\"cgr.dev/example:latest\"}\n"),
		},
	})
	if err != nil {
		t.Fatalf("unexpected encode error: %s", err)
	}

	value, _, diags := DecodeValue(req, payload, ValueAdapterExternalV1, nil, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if got, want := value, cty.ObjectVal(map[string]cty.Value{
		"id": cty.StringVal("-"),
		"result": cty.MapVal(map[string]cty.Value{
			"image_ref": cty.StringVal("cgr.dev/example:latest"),
		}),
	}); !got.RawEquals(want) {
		t.Fatalf("wrong external adapter value: got %s want %s", got.GoString(), want.GoString())
	}
}

func TestDecodeValueExternalAdapterRejectsNonStringResult(t *testing.T) {
	req := mustBuildTestRequest(t)
	payload, err := EncodeResult(req, Result{
		ExitCode: 0,
		Stdout:   []byte("{\"count\":2}\n"),
		Decoded: &DecodedValue{
			Mode: DecodeModeJSON,
			JSON: []byte("{\"count\":2}\n"),
		},
	})
	if err != nil {
		t.Fatalf("unexpected encode error: %s", err)
	}

	_, _, diags := DecodeValue(req, payload, ValueAdapterExternalV1, nil, tfdiags.SourceRange{})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "string values") {
		t.Fatalf("missing string-values diagnostic in:\n%s", got)
	}
}

func TestDecodeValueNullAdapter(t *testing.T) {
	req, diags := BuildRequest(RequestArgs{
		Argv: []string{"/bin/echo", "hello"},
		Cwd:  t.TempDir(),
	}, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected request diagnostics: %s", diags.Err())
	}
	payload, err := EncodeResult(*req, Result{
		ExitCode: 0,
		Stdout:   []byte("ok\n"),
		Stderr:   []byte("stderr\n"),
	})
	if err != nil {
		t.Fatalf("unexpected encode error: %s", err)
	}
	valueData, err := json.Marshal(map[string]map[string]string{
		"triggers": {
			"image": "cgr.dev/example:latest",
		},
	})
	if err != nil {
		t.Fatalf("unexpected value-data error: %s", err)
	}

	value, valueDigest, diags := DecodeValue(*req, payload, ValueAdapterNullV1, valueData, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if valueDigest == (digest.Digest{}) {
		t.Fatal("expected non-zero digest")
	}
	if got, want := value, cty.ObjectVal(map[string]cty.Value{
		"id": cty.StringVal(valueDigest.String()),
		"triggers": cty.MapVal(map[string]cty.Value{
			"image": cty.StringVal("cgr.dev/example:latest"),
		}),
	}); !got.RawEquals(want) {
		t.Fatalf("wrong null adapter value: got %s want %s", got.GoString(), want.GoString())
	}
}

func TestDecodeValueNullAdapterIDTracksValueData(t *testing.T) {
	req, diags := BuildRequest(RequestArgs{
		Argv: []string{"/bin/echo", "hello"},
		Cwd:  t.TempDir(),
	}, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected request diagnostics: %s", diags.Err())
	}
	payload, err := EncodeResult(*req, Result{
		ExitCode: 0,
		Stdout:   []byte("ok\n"),
	})
	if err != nil {
		t.Fatalf("unexpected encode error: %s", err)
	}

	firstValueData := []byte(`{"triggers":{"image":"one"}}`)
	secondValueData := []byte(`{"triggers":{"image":"two"}}`)

	firstValue, firstDigest, firstDiags := DecodeValue(*req, payload, ValueAdapterNullV1, firstValueData, tfdiags.SourceRange{})
	if firstDiags.HasErrors() {
		t.Fatalf("unexpected first diagnostics: %s", firstDiags.Err())
	}
	secondValue, secondDigest, secondDiags := DecodeValue(*req, payload, ValueAdapterNullV1, secondValueData, tfdiags.SourceRange{})
	if secondDiags.HasErrors() {
		t.Fatalf("unexpected second diagnostics: %s", secondDiags.Err())
	}
	if firstDigest == secondDigest {
		t.Fatal("expected distinct null adapter digests for distinct value data")
	}
	if got, want := firstValue.GetAttr("id"), cty.StringVal(firstDigest.String()); !got.RawEquals(want) {
		t.Fatalf("wrong first id: got %s want %s", got.GoString(), want.GoString())
	}
	if got, want := secondValue.GetAttr("id"), cty.StringVal(secondDigest.String()); !got.RawEquals(want) {
		t.Fatalf("wrong second id: got %s want %s", got.GoString(), want.GoString())
	}
}

func TestDecodeValueRawAdapterRejectsTrailingJSONData(t *testing.T) {
	req := mustBuildTestRequest(t)
	payload, err := EncodeResult(req, Result{
		ExitCode: 0,
		Stdout:   []byte("{\"value\":\"hello\"}\n"),
		Decoded: &DecodedValue{
			Mode: DecodeModeJSON,
			JSON: []byte("{\"value\":\"hello\"}\n"),
		},
	})
	if err != nil {
		t.Fatalf("unexpected encode error: %s", err)
	}
	corrupted := cloneJSONMap(t, payload)
	decoded := corrupted["decoded"].(map[string]any)
	decoded["json"] = base64.StdEncoding.EncodeToString([]byte("{\"value\":\"hello\"} trailing"))
	payload = mustJSONMarshal(t, corrupted)

	_, _, diags := DecodeValue(req, payload, ValueAdapterRawV1, nil, tfdiags.SourceRange{})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "trailing data after JSON value") {
		t.Fatalf("missing trailing-json diagnostic in:\n%s", got)
	}
}

func TestDecodeValueRejectsUnsupportedAdapter(t *testing.T) {
	req, diags := BuildRequest(RequestArgs{
		Argv: []string{"/bin/echo", "hello"},
		Cwd:  t.TempDir(),
	}, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected request diagnostics: %s", diags.Err())
	}
	payload, err := EncodeResult(*req, Result{ExitCode: 0})
	if err != nil {
		t.Fatalf("unexpected encode error: %s", err)
	}

	_, _, decodeDiags := DecodeValue(*req, payload, ValueAdapter("custom-v1"), nil, tfdiags.SourceRange{})
	if !decodeDiags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := decodeDiags.Err().Error(); !strings.Contains(got, `unsupported value adapter "custom-v1"`) {
		t.Fatalf("missing adapter diagnostic in:\n%s", got)
	}
}

func TestNormalizeValueAdapterDefaultsRawV1(t *testing.T) {
	if got, want := NormalizeValueAdapter(""), ValueAdapterRawV1; got != want {
		t.Fatalf("wrong normalized adapter for empty value: got %q want %q", got, want)
	}
	if got, want := NormalizeValueAdapter(ValueAdapterRawV1), ValueAdapterRawV1; got != want {
		t.Fatalf("wrong normalized adapter for explicit raw value: got %q want %q", got, want)
	}
}
