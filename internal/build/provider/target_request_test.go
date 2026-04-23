// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

func TestDecodeTargetRequestRejectsUnsupportedVersion(t *testing.T) {
	req := mustBuildTestTargetRequest(t)

	var envelope map[string]any
	if err := json.Unmarshal(req.Payload, &envelope); err != nil {
		t.Fatalf("failed to decode request envelope: %s", err)
	}
	envelope["version"] = "build-provider-target-request-v0"
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("failed to encode request envelope: %s", err)
	}
	req.Payload = payload

	_, _, _, diags := DecodeTargetRequest(*req, tfdiags.SourceRange{})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "unsupported version") {
		t.Fatalf("missing unsupported version diagnostic in:\n%s", got)
	}
}

func TestDecodeTargetResultRejectsUnsupportedRequestVersion(t *testing.T) {
	req := mustBuildTestTargetRequest(t)

	var envelope map[string]any
	if err := json.Unmarshal(req.Payload, &envelope); err != nil {
		t.Fatalf("failed to decode request envelope: %s", err)
	}
	envelope["version"] = "build-provider-target-request-v0"
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("failed to encode request envelope: %s", err)
	}
	req.Payload = payload

	resultPayload, err := encodeTargetResult(cty.ObjectVal(map[string]cty.Value{
		"value": cty.StringVal("hello"),
	}), cty.Object(map[string]cty.Type{
		"value": cty.String,
	}))
	if err != nil {
		t.Fatalf("failed to encode result payload: %s", err)
	}

	_, _, diags := DecodeTargetResult(*req, resultPayload, tfdiags.SourceRange{})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "unsupported version") {
		t.Fatalf("missing unsupported version diagnostic in:\n%s", got)
	}
}

func TestDecodeTargetResultRejectsUnsupportedResultVersion(t *testing.T) {
	req := mustBuildTestTargetRequest(t)
	resultPayload, err := encodeTargetResult(cty.ObjectVal(map[string]cty.Value{
		"value": cty.StringVal("hello"),
	}), cty.Object(map[string]cty.Type{
		"value": cty.String,
	}))
	if err != nil {
		t.Fatalf("failed to encode result payload: %s", err)
	}

	var envelope map[string]any
	if err := json.Unmarshal(resultPayload, &envelope); err != nil {
		t.Fatalf("failed to decode result envelope: %s", err)
	}
	envelope["version"] = "build-provider-target-result-v0"
	resultPayload, err = json.Marshal(envelope)
	if err != nil {
		t.Fatalf("failed to encode result envelope: %s", err)
	}

	_, _, diags := DecodeTargetResult(*req, resultPayload, tfdiags.SourceRange{})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "unsupported version") {
		t.Fatalf("missing unsupported version diagnostic in:\n%s", got)
	}
}

func mustBuildTestTargetRequest(t *testing.T) *TargetRequest {
	t.Helper()

	req, diags := BuildTargetRequest(
		catalog.RunnerKindProviderResource,
		addrs.NewDefaultProvider("test"),
		"test_resource",
		&providers.Schema{
			Version: 1,
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"value": {
						Type:     cty.String,
						Optional: true,
					},
				},
			},
		},
		cty.ObjectVal(map[string]cty.Value{
			"value": cty.StringVal("hello"),
		}),
		tfdiags.SourceRange{},
	)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	return req
}
