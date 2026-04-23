// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type FunctionRequest struct {
	Function addrs.ProviderFunction
	Key      digest.Digest
	Payload  []byte
}

func BuildFunctionRequest(fn addrs.ProviderFunction, args []cty.Value, rng tfdiags.SourceRange) (*FunctionRequest, tfdiags.Diagnostics) {
	switch {
	case fn.ProviderName == "oci" && fn.Function == "get":
		return buildOCIGetRequest(fn, args, rng)
	default:
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Unsupported provider function in build mode",
			fmt.Sprintf("Provider function %s does not have a build-mode request implementation yet.", fn.String()),
		))
	}
}

type ociGetRequest struct {
	Ref    string `json:"ref"`
	Pinned bool   `json:"pinned"`
}

func buildOCIGetRequest(fn addrs.ProviderFunction, args []cty.Value, rng tfdiags.SourceRange) (*FunctionRequest, tfdiags.Diagnostics) {
	if len(args) != 1 {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider function request",
			fmt.Sprintf("Provider function %s expects exactly 1 argument, got %d.", fn.String(), len(args)),
		))
	}

	ref, err := convert.Convert(args[0], cty.String)
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider function request",
			fmt.Sprintf("Provider function %s requires a string OCI reference argument: %s", fn.String(), err),
		))
	}
	if !ref.IsKnown() || ref.IsNull() {
		return nil, nil
	}

	parsed, err := name.ParseReference(ref.AsString())
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider function request",
			fmt.Sprintf("Provider function %s requires a valid OCI reference argument: %s", fn.String(), err),
		))
	}

	payload, err := json.Marshal(ociGetRequest{
		Ref:    parsed.Name(),
		Pinned: ociReferencePinned(parsed),
	})
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider function request",
			fmt.Sprintf("Failed to encode provider function request for %s: %s", fn.String(), err),
		))
	}

	key, err := digest.FromValue(struct {
		Version  string
		Function addrs.ProviderFunction
		Payload  []byte
	}{
		Version:  "build-provider-function-request-v1",
		Function: fn,
		Payload:  payload,
	})
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider function request",
			fmt.Sprintf("Failed to key provider function request for %s: %s", fn.String(), err),
		))
	}

	return &FunctionRequest{
		Function: fn,
		Key:      key,
		Payload:  payload,
	}, nil
}

func functionRequestDiagnostic(rng tfdiags.SourceRange, summary, detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  summary,
		Detail:   detail,
		Subject:  rng.ToHCL().Ptr(),
	}
}

func ociReferencePinned(ref name.Reference) bool {
	_, ok := ref.(name.Digest)
	return ok
}
