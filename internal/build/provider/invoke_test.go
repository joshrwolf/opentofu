// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/digest"
)

type stubOCIGetResolver struct {
	resolved string
	err      error
}

func (r stubOCIGetResolver) Resolve(_ context.Context, _ string) (string, error) {
	return r.resolved, r.err
}

func TestInvokerInvokeOCIGet(t *testing.T) {
	req := FunctionRequest{
		Function: addrs.ProviderFunction{ProviderName: "oci", Function: "get"},
		Key:      digest.FromString("request"),
		Payload:  []byte(`{"ref":"cgr.dev/example/image:latest","pinned":false}`),
	}

	result, diags := NewInvoker(InvokerConfig{
		OCIGetResolver: stubOCIGetResolver{
			resolved: "cgr.dev/example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}).InvokeFunction(t.Context(), req)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.OutputKey == (digest.Digest{}) {
		t.Fatal("expected non-zero output key")
	}
	if got, want := string(result.Payload), `{"version":"build-provider-function-result-v1","type":"string","string":"cgr.dev/example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`; got != want {
		t.Fatalf("wrong payload: got %q want %q", got, want)
	}
}

func TestInvokerRejectsInvalidRequestPayload(t *testing.T) {
	req := FunctionRequest{
		Function: addrs.ProviderFunction{ProviderName: "oci", Function: "get"},
		Key:      digest.FromString("request"),
		Payload:  []byte(`{`),
	}

	_, diags := DefaultInvoker().InvokeFunction(t.Context(), req)
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
}

func TestResolveReferenceDigestReturnsFallbackErrorChain(t *testing.T) {
	parsed, err := name.ParseReference("index.docker.io/library/busybox:latest")
	if err != nil {
		t.Fatalf("failed to parse reference: %s", err)
	}

	headErr := errors.New("head failed")
	getErr := errors.New("get failed")
	oldHead := remoteHead
	oldGet := remoteGet
	remoteHead = func(name.Reference, ...remote.Option) (*v1.Descriptor, error) {
		return nil, headErr
	}
	remoteGet = func(name.Reference, ...remote.Option) (*remote.Descriptor, error) {
		return nil, getErr
	}
	defer func() {
		remoteHead = oldHead
		remoteGet = oldGet
	}()

	_, err = resolveReferenceDigest(parsed)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, headErr) {
		t.Fatal("expected joined error to include head failure")
	}
	if !errors.Is(err, getErr) {
		t.Fatal("expected joined error to include get failure")
	}
}

func TestFunctionRequestCacheableForPinnedOCIRef(t *testing.T) {
	req := FunctionRequest{
		Function: addrs.ProviderFunction{ProviderName: "oci", Function: "get"},
		Key:      digest.FromString("request"),
		Payload:  []byte(`{"ref":"cgr.dev/example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","pinned":true}`),
	}
	if !FunctionRequestCacheable(req) {
		t.Fatal("expected pinned OCI request to be cacheable")
	}
}
