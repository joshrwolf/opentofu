// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type InvokeResult struct {
	OutputKey digest.Digest
	Payload   []byte
}

type OCIGetResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

type InvokerConfig struct {
	OCIGetResolver OCIGetResolver
}

type Invoker struct {
	ociGet OCIGetResolver
}

type OCIGetResolverConfig struct {
	Keychain  authn.Keychain
	Transport http.RoundTripper
}

type ociGetResolver struct {
	keychain  authn.Keychain
	transport http.RoundTripper
}

type functionResultEnvelope struct {
	Version string `json:"version"`
	Type    string `json:"type"`
	String  string `json:"string,omitempty"`
}

var (
	remoteHead = remote.Head
	remoteGet  = remote.Get
)

func NewInvoker(cfg InvokerConfig) *Invoker {
	resolver := cfg.OCIGetResolver
	if resolver == nil {
		resolver = NewOCIGetResolver(OCIGetResolverConfig{})
	}
	return &Invoker{ociGet: resolver}
}

func DefaultInvoker() *Invoker {
	return NewInvoker(InvokerConfig{})
}

func NewOCIGetResolver(cfg OCIGetResolverConfig) OCIGetResolver {
	keychain := cfg.Keychain
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	return &ociGetResolver{
		keychain:  keychain,
		transport: cfg.Transport,
	}
}

func (i *Invoker) InvokeFunction(ctx context.Context, req FunctionRequest) (InvokeResult, tfdiags.Diagnostics) {
	if i == nil {
		i = DefaultInvoker()
	}

	switch {
	case req.Function.ProviderName == "oci" && req.Function.Function == "get":
		return i.invokeOCIGet(ctx, req)
	default:
		return InvokeResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Unsupported provider function action",
			"Provider function "+req.Function.String()+" does not have a build-mode action runner yet.",
		))
	}
}

func FunctionRequestCacheable(req FunctionRequest) bool {
	switch {
	case req.Function.ProviderName == "oci" && req.Function.Function == "get":
		var payload ociGetRequest
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			return false
		}
		return payload.Pinned
	default:
		return false
	}
}

func (i *Invoker) invokeOCIGet(ctx context.Context, req FunctionRequest) (InvokeResult, tfdiags.Diagnostics) {
	if i.ociGet == nil {
		return InvokeResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			"OCI provider-function resolver is not configured.",
		))
	}

	var payload ociGetRequest
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return InvokeResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid provider function request",
			fmt.Sprintf("Failed to decode provider function request for %s: %s", req.Function.String(), err),
		))
	}

	resolved, err := i.ociGet.Resolve(ctx, payload.Ref)
	if err != nil {
		return InvokeResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Provider function execution failed",
			fmt.Sprintf("Provider function %s failed: %s", req.Function.String(), err),
		))
	}

	resultPayload, err := json.Marshal(functionResultEnvelope{
		Version: "build-provider-function-result-v1",
		Type:    "string",
		String:  resolved,
	})
	if err != nil {
		return InvokeResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Provider function execution failed",
			fmt.Sprintf("Failed to encode provider function result for %s: %s", req.Function.String(), err),
		))
	}

	return InvokeResult{
		OutputKey: digest.FromBytes(resultPayload),
		Payload:   resultPayload,
	}, nil
}

func (r *ociGetResolver) Resolve(ctx context.Context, ref string) (string, error) {
	parsed, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parse OCI reference %q: %w", ref, err)
	}
	if _, ok := parsed.(name.Digest); ok {
		return parsed.String(), nil
	}

	options := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(r.keychain),
	}
	if r.transport != nil {
		options = append(options, remote.WithTransport(r.transport))
	}

	digestRef, err := resolveReferenceDigest(parsed, options...)
	if err != nil {
		return "", fmt.Errorf("resolve OCI reference %q: %w", ref, err)
	}
	return digestRef, nil
}

func resolveReferenceDigest(ref name.Reference, options ...remote.Option) (string, error) {
	desc, err := remoteHead(ref, options...)
	if err == nil {
		return referenceWithDigest(ref, desc.Digest), nil
	}

	getDesc, getErr := remoteGet(ref, options...)
	if getErr != nil {
		return "", errors.Join(err, getErr)
	}
	return referenceWithDigest(ref, getDesc.Digest), nil
}

func referenceWithDigest(ref name.Reference, dgst v1.Hash) string {
	return ref.Context().Digest(dgst.String()).String()
}
