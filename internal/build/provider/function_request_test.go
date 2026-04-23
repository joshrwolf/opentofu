// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

func TestBuildFunctionRequestOCIGet(t *testing.T) {
	req, diags := BuildFunctionRequest(
		addrs.ProviderFunction{ProviderName: "oci", Function: "get"},
		[]cty.Value{cty.StringVal("cgr.dev/example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		tfdiags.SourceRange{},
	)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if req == nil {
		t.Fatal("expected request")
	}
	if req.Key == (digest.Digest{}) {
		t.Fatal("expected non-zero request key")
	}
	if got, want := req.Function, (addrs.ProviderFunction{ProviderName: "oci", Function: "get"}); got != want {
		t.Fatalf("wrong function: got %#v want %#v", got, want)
	}
	if got, want := string(req.Payload), `{"ref":"cgr.dev/example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","pinned":true}`; got != want {
		t.Fatalf("wrong payload: got %q want %q", got, want)
	}
}

func TestBuildFunctionRequestOCIGetCanonicalizesAndMarksMutableRefs(t *testing.T) {
	req, diags := BuildFunctionRequest(
		addrs.ProviderFunction{ProviderName: "oci", Function: "get"},
		[]cty.Value{cty.StringVal("busybox")},
		tfdiags.SourceRange{},
	)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if req == nil {
		t.Fatal("expected request")
	}
	if got, want := string(req.Payload), `{"ref":"index.docker.io/library/busybox:latest","pinned":false}`; got != want {
		t.Fatalf("wrong canonical payload: got %q want %q", got, want)
	}
	if FunctionRequestCacheable(*req) {
		t.Fatal("did not expect mutable OCI ref request to be cacheable")
	}
}

func TestBuildFunctionRequestOCIGetRejectsInvalidRef(t *testing.T) {
	req, diags := BuildFunctionRequest(
		addrs.ProviderFunction{ProviderName: "oci", Function: "get"},
		[]cty.Value{cty.StringVal("not a valid ref")},
		tfdiags.SourceRange{},
	)
	if req != nil {
		t.Fatal("did not expect request")
	}
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "valid OCI reference argument") {
		t.Fatalf("missing invalid OCI ref diagnostic in:\n%s", got)
	}
}

func TestBuildFunctionRequestUnsupportedFunction(t *testing.T) {
	req, diags := BuildFunctionRequest(
		addrs.ProviderFunction{ProviderName: "oci", Function: "head"},
		[]cty.Value{cty.StringVal("cgr.dev/example/image")},
		tfdiags.SourceRange{},
	)
	if req != nil {
		t.Fatal("did not expect request")
	}
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); got == "" || !strings.Contains(got, "does not have a build-mode request implementation yet") {
		t.Fatalf("missing unsupported provider function diagnostic in:\n%s", got)
	}
}
