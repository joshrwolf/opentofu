// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestTargetCapabilityRegistryResolvesExactBeforeBroaderRules(t *testing.T) {
	reg := NewTargetCapabilityRegistry()
	provider := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko")

	reg.RegisterProvider(provider, TargetCapabilityRule{
		Supported: true,
		Revision:  "provider-v1",
		Note:      "provider default",
	})
	reg.RegisterProviderKind(provider, catalog.RunnerKindProviderResource, TargetCapabilityRule{
		Supported: true,
		Revision:  "provider-kind-v1",
		Note:      "provider-kind default",
	})
	reg.Register(TargetCapability{
		Provider:  provider,
		Kind:      catalog.RunnerKindProviderResource,
		TypeName:  "apko_build",
		Supported: true,
		Revision:  "exact-v1",
		Note:      "exact override",
	})

	capability, ok := reg.Capability(provider, catalog.RunnerKindProviderResource, "apko_build")
	if !ok {
		t.Fatal("expected resolved capability")
	}
	if got, want := capability.MatchKind, TargetCapabilityMatchExact; got != want {
		t.Fatalf("wrong match kind: got %q want %q", got, want)
	}
	if got, want := capability.Revision, "exact-v1"; got != want {
		t.Fatalf("wrong revision: got %q want %q", got, want)
	}
	if got, want := capability.Note, "exact override"; got != want {
		t.Fatalf("wrong note: got %q want %q", got, want)
	}
}

func TestTargetCapabilityRegistryFallsBackToProviderKindAndProviderRules(t *testing.T) {
	reg := NewTargetCapabilityRegistry()
	provider := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "cosign")

	reg.RegisterProvider(provider, TargetCapabilityRule{
		Supported: true,
		Revision:  "provider-v1",
		Note:      "provider default",
	})
	reg.RegisterProviderKind(provider, catalog.RunnerKindProviderResource, TargetCapabilityRule{
		Supported: true,
		Revision:  "provider-kind-v1",
		Note:      "provider-kind default",
	})

	resourceCapability, ok := reg.Capability(provider, catalog.RunnerKindProviderResource, "cosign_sign")
	if !ok {
		t.Fatal("expected resolved resource capability")
	}
	if got, want := resourceCapability.MatchKind, TargetCapabilityMatchProviderKind; got != want {
		t.Fatalf("wrong resource match kind: got %q want %q", got, want)
	}
	if got, want := resourceCapability.Revision, "provider-kind-v1"; got != want {
		t.Fatalf("wrong resource revision: got %q want %q", got, want)
	}

	dataCapability, ok := reg.Capability(provider, catalog.RunnerKindProviderData, "cosign_lookup")
	if !ok {
		t.Fatal("expected resolved data capability")
	}
	if got, want := dataCapability.MatchKind, TargetCapabilityMatchProvider; got != want {
		t.Fatalf("wrong data match kind: got %q want %q", got, want)
	}
	if got, want := dataCapability.Revision, "provider-v1"; got != want {
		t.Fatalf("wrong data revision: got %q want %q", got, want)
	}
}

func TestResolvedTargetCapabilityExactNilPolicyDoesNotInheritBroaderPolicy(t *testing.T) {
	reg := NewTargetCapabilityRegistry()
	provider := addrs.NewDefaultProvider("test")

	reg.RegisterProvider(provider, TargetCapabilityRule{
		Supported: true,
		Revision:  "provider-v1",
		PolicyFunc: func(TargetRequest, Binding) (TargetCapabilityPolicy, tfdiags.Diagnostics) {
			return TargetCapabilityPolicy{
				Cacheable: true,
				LockKeys:  []string{"shared"},
			}, nil
		},
	})
	reg.Register(TargetCapability{
		Provider:  provider,
		Kind:      catalog.RunnerKindProviderResource,
		TypeName:  "test_resource",
		Supported: true,
		Revision:  "exact-v1",
	})

	capability, ok := reg.Capability(provider, catalog.RunnerKindProviderResource, "test_resource")
	if !ok {
		t.Fatal("expected resolved capability")
	}
	policy, diags := capability.ResolvePolicy(TargetRequest{
		Provider: provider,
		Kind:     catalog.RunnerKindProviderResource,
		TypeName: "test_resource",
	}, Binding{Provider: provider})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if policy.Cacheable {
		t.Fatal("did not expect inherited cacheable policy")
	}
	if diff := cmp.Diff([]string(nil), policy.LockKeys); diff != "" {
		t.Fatalf("wrong lock keys (-want +got):\n%s", diff)
	}
}

func TestResolvedTargetCapabilityScopesLogicalLockKeys(t *testing.T) {
	reg := NewTargetCapabilityRegistry()
	provider := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko")

	reg.Register(TargetCapability{
		Provider:  provider,
		Kind:      catalog.RunnerKindProviderResource,
		TypeName:  "apko_build",
		Supported: true,
		Revision:  "v1",
		PolicyFunc: func(TargetRequest, Binding) (TargetCapabilityPolicy, tfdiags.Diagnostics) {
			return TargetCapabilityPolicy{
				LockKeys: []string{"repo/cgr.dev/example/image"},
			}, nil
		},
	})

	capability, ok := reg.Capability(provider, catalog.RunnerKindProviderResource, "apko_build")
	if !ok {
		t.Fatal("expected resolved capability")
	}
	policy, diags := capability.ResolvePolicy(TargetRequest{
		Provider: provider,
		Kind:     catalog.RunnerKindProviderResource,
		TypeName: "apko_build",
	}, Binding{Provider: provider})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if diff := cmp.Diff([]string{"provider/chainguard-dev/apko/repo/cgr.dev/example/image"}, policy.LockKeys); diff != "" {
		t.Fatalf("wrong scoped lock keys (-want +got):\n%s", diff)
	}
}
