// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	commandtesting "github.com/opentofu/opentofu/internal/command/testing"
	"github.com/opentofu/opentofu/internal/providers"
)

func TestSessionTargetSchemaUsesCapabilityRegistry(t *testing.T) {
	providerAddr := addrs.NewDefaultProvider("test")
	provider := commandtesting.NewProvider(nil)

	unsupported := NewSession(SessionConfig{
		Factories: map[addrs.Provider]providers.Factory{
			providerAddr: providers.FactoryFixed(provider.Provider),
		},
		TargetCapabilities: NewTargetCapabilityRegistry(),
	})
	schema, diags := unsupported.TargetSchema(t.Context(), catalog.RunnerKindProviderResource, providerAddr, "test_resource")
	if diags.HasErrors() {
		t.Fatalf("unexpected unsupported diagnostics: %s", diags.Err())
	}
	if schema != nil {
		t.Fatal("expected unsupported session to return no target schema")
	}

	supported := NewSession(SessionConfig{
		Factories: map[addrs.Provider]providers.Factory{
			providerAddr: providers.FactoryFixed(provider.Provider),
		},
		TargetCapabilities: DefaultTargetCapabilityRegistry(),
	})
	schema, diags = supported.TargetSchema(t.Context(), catalog.RunnerKindProviderResource, providerAddr, "test_resource")
	if diags.HasErrors() {
		t.Fatalf("unexpected supported diagnostics: %s", diags.Err())
	}
	if schema == nil {
		t.Fatal("expected supported session to return target schema")
	}
}

func TestSessionInvokeTargetReturnsUnsupportedDiagnostics(t *testing.T) {
	providerAddr := addrs.NewDefaultProvider("test")
	session := NewSession(SessionConfig{
		TargetCapabilities: NewTargetCapabilityRegistry(),
	})

	_, diags := session.InvokeTarget(t.Context(), Binding{
		Provider:  providerAddr,
		ConfigKey: digest.FromString("binding"),
	}, TargetRequest{
		Kind:     catalog.RunnerKindProviderResource,
		Provider: providerAddr,
		TypeName: "test_resource",
	})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, `Resource type "test_resource"`) {
		t.Fatalf("missing unsupported target diagnostic in:\n%s", got)
	}
}

func TestDefaultTargetCapabilityRegistryIncludesImagesPrivateTargets(t *testing.T) {
	reg := DefaultTargetCapabilityRegistry()

	for _, tc := range []struct {
		provider addrs.Provider
		kind     catalog.RunnerKind
		typeName string
		match    TargetCapabilityMatchKind
		revision string
	}{
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "apko_build",
			match:    TargetCapabilityMatchExact,
			revision: "v2",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "apko_build_raw",
			match:    TargetCapabilityMatchExact,
			revision: "v2",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko"),
			kind:     catalog.RunnerKindProviderData,
			typeName: "apko_config",
			match:    TargetCapabilityMatchProvider,
			revision: "apko-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko"),
			kind:     catalog.RunnerKindProviderData,
			typeName: "apko_tags",
			match:    TargetCapabilityMatchProvider,
			revision: "apko-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "cosign"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "cosign_sign",
			match:    TargetCapabilityMatchProvider,
			revision: "cosign-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "cosign"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "cosign_attest",
			match:    TargetCapabilityMatchProvider,
			revision: "cosign-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "cosign"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "cosign_copy",
			match:    TargetCapabilityMatchProvider,
			revision: "cosign-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "oci"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "oci_tags",
			match:    TargetCapabilityMatchProvider,
			revision: "oci-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "oci"),
			kind:     catalog.RunnerKindProviderData,
			typeName: "oci_exec_test",
			match:    TargetCapabilityMatchProvider,
			revision: "oci-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "oci"),
			kind:     catalog.RunnerKindProviderData,
			typeName: "oci_structure_test",
			match:    TargetCapabilityMatchProvider,
			revision: "oci-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "imagetest"),
			kind:     catalog.RunnerKindProviderData,
			typeName: "imagetest_inventory",
			match:    TargetCapabilityMatchProvider,
			revision: "imagetest-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "imagetest"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "imagetest_harness_docker",
			match:    TargetCapabilityMatchProvider,
			revision: "imagetest-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "imagetest"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "imagetest_harness_k3s",
			match:    TargetCapabilityMatchProvider,
			revision: "imagetest-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "imagetest"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "imagetest_feature",
			match:    TargetCapabilityMatchProvider,
			revision: "imagetest-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "imagetest"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "imagetest_tests",
			match:    TargetCapabilityMatchProvider,
			revision: "imagetest-provider-v1",
		},
		{
			provider: addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "imagetest"),
			kind:     catalog.RunnerKindProviderResource,
			typeName: "imagetest_container_volume",
			match:    TargetCapabilityMatchProvider,
			revision: "imagetest-provider-v1",
		},
	} {
		cap, ok := reg.Capability(tc.provider, tc.kind, tc.typeName)
		if !ok {
			t.Fatalf("missing capability for %s %s %q", tc.provider, tc.kind, tc.typeName)
		}
		if got, want := cap.MatchKind, tc.match; got != want {
			t.Fatalf("wrong match kind for %s %s %q: got %q want %q", tc.provider, tc.kind, tc.typeName, got, want)
		}
		if got, want := cap.Revision, tc.revision; got != want {
			t.Fatalf("wrong capability revision for %s %s %q: got %q want %q", tc.provider, tc.kind, tc.typeName, got, want)
		}
	}
}
