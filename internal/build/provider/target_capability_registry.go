// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"sync"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

type targetCapabilityKey struct {
	Provider addrs.Provider
	Kind     catalog.RunnerKind
	TypeName string
}

type targetCapabilityProviderKindKey struct {
	Provider addrs.Provider
	Kind     catalog.RunnerKind
}

type TargetCapabilityMatchKind string

const (
	TargetCapabilityMatchExact        TargetCapabilityMatchKind = "exact"
	TargetCapabilityMatchProviderKind TargetCapabilityMatchKind = "provider_kind"
	TargetCapabilityMatchProvider     TargetCapabilityMatchKind = "provider"
)

type TargetCapabilityPolicy struct {
	Cacheable bool
	Volatile  bool
	LockKeys  []string
}

type TargetCapabilityRule struct {
	Supported  bool
	Revision   string
	Note       string
	PolicyFunc func(TargetRequest, Binding) (TargetCapabilityPolicy, tfdiags.Diagnostics)
}

type TargetCapability struct {
	Provider  addrs.Provider
	Kind      catalog.RunnerKind
	TypeName  string
	Supported bool
	Revision  string
	Note      string

	PolicyFunc func(TargetRequest, Binding) (TargetCapabilityPolicy, tfdiags.Diagnostics)
}

func (c TargetCapability) rule() TargetCapabilityRule {
	return TargetCapabilityRule{
		Supported:  c.Supported,
		Revision:   c.Revision,
		Note:       c.Note,
		PolicyFunc: c.PolicyFunc,
	}
}

type ResolvedTargetCapability struct {
	Provider  addrs.Provider
	Kind      catalog.RunnerKind
	TypeName  string
	MatchKind TargetCapabilityMatchKind
	Supported bool
	Revision  string
	Note      string

	PolicyFunc func(TargetRequest, Binding) (TargetCapabilityPolicy, tfdiags.Diagnostics)
}

func (c ResolvedTargetCapability) ResolvePolicy(req TargetRequest, binding Binding) (TargetCapabilityPolicy, tfdiags.Diagnostics) {
	if !c.Supported || c.PolicyFunc == nil {
		return TargetCapabilityPolicy{}, nil
	}

	policy, diags := c.PolicyFunc(req, binding)
	policy.LockKeys = scopeTargetCapabilityLockKeys(c.Provider, policy.LockKeys)
	return policy, diags
}

func scopeTargetCapabilityLockKeys(provider addrs.Provider, keys []string) []string {
	if len(keys) == 0 {
		return nil
	}

	ret := make([]string, 0, len(keys))
	prefix := "provider/" + provider.ForDisplay() + "/"
	for _, key := range keys {
		if key == "" {
			continue
		}
		ret = append(ret, prefix+key)
	}
	return ret
}

type TargetCapabilityRegistry struct {
	mu           sync.RWMutex
	exact        map[targetCapabilityKey]TargetCapabilityRule
	providerKind map[targetCapabilityProviderKindKey]TargetCapabilityRule
	provider     map[addrs.Provider]TargetCapabilityRule
}

func NewTargetCapabilityRegistry() *TargetCapabilityRegistry {
	return &TargetCapabilityRegistry{
		exact:        map[targetCapabilityKey]TargetCapabilityRule{},
		providerKind: map[targetCapabilityProviderKindKey]TargetCapabilityRule{},
		provider:     map[addrs.Provider]TargetCapabilityRule{},
	}
}

func DefaultTargetCapabilityRegistry() *TargetCapabilityRegistry {
	ret := NewTargetCapabilityRegistry()
	testProvider := addrs.NewDefaultProvider("test")
	ret.Register(TargetCapability{
		Provider:  testProvider,
		Kind:      catalog.RunnerKindProviderResource,
		TypeName:  "test_resource",
		Supported: true,
		Revision:  "v1",
	})
	ret.Register(TargetCapability{
		Provider:  testProvider,
		Kind:      catalog.RunnerKindProviderData,
		TypeName:  "test_data_source",
		Supported: true,
		Revision:  "v1",
	})
	fooTestProvider := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "foo", "test")
	ret.Register(TargetCapability{
		Provider:  fooTestProvider,
		Kind:      catalog.RunnerKindProviderResource,
		TypeName:  "test_resource",
		Supported: true,
		Revision:  "v1",
	})
	ret.Register(TargetCapability{
		Provider:  fooTestProvider,
		Kind:      catalog.RunnerKindProviderData,
		TypeName:  "test_data_source",
		Supported: true,
		Revision:  "v1",
	})

	apkoProvider := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko")
	ret.RegisterProvider(apkoProvider, TargetCapabilityRule{
		Supported: true,
		Revision:  "apko-provider-v1",
		Note:      "Build-capable by reviewed provider family default.",
	})
	ret.Register(TargetCapability{
		Provider:  apkoProvider,
		Kind:      catalog.RunnerKindProviderResource,
		TypeName:  "apko_build",
		Supported: true,
		Revision:  "v2",
		PolicyFunc: func(req TargetRequest, binding Binding) (TargetCapabilityPolicy, tfdiags.Diagnostics) {
			return apkoBuildPolicy(req)
		},
		Note: "Serialized by target repository to avoid concurrent builds writing to the same apko repo.",
	})
	ret.Register(TargetCapability{
		Provider:  apkoProvider,
		Kind:      catalog.RunnerKindProviderResource,
		TypeName:  "apko_build_raw",
		Supported: true,
		Revision:  "v2",
		PolicyFunc: func(req TargetRequest, binding Binding) (TargetCapabilityPolicy, tfdiags.Diagnostics) {
			return apkoBuildPolicy(req)
		},
		Note: "Serialized by target repository to avoid concurrent raw apko builds writing to the same apko repo.",
	})

	cosignProvider := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "cosign")
	ret.RegisterProvider(cosignProvider, TargetCapabilityRule{
		Supported: true,
		Revision:  "cosign-provider-v1",
		Note:      "Build-capable by reviewed provider family default.",
	})

	ociProvider := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "oci")
	ret.RegisterProvider(ociProvider, TargetCapabilityRule{
		Supported: true,
		Revision:  "oci-provider-v1",
		Note:      "Build-capable by reviewed provider family default.",
	})

	imagetestProvider := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "imagetest")
	ret.RegisterProvider(imagetestProvider, TargetCapabilityRule{
		Supported: true,
		Revision:  "imagetest-provider-v1",
		Note:      "Build-capable by reviewed provider family default.",
	})

	return ret
}

func normalizeTargetCapabilityRule(rule TargetCapabilityRule) TargetCapabilityRule {
	if rule.Revision == "" {
		rule.Revision = "v1"
	}
	return rule
}

func resolvedTargetCapability(provider addrs.Provider, kind catalog.RunnerKind, typeName string, matchKind TargetCapabilityMatchKind, rule TargetCapabilityRule) ResolvedTargetCapability {
	rule = normalizeTargetCapabilityRule(rule)
	return ResolvedTargetCapability{
		Provider:   provider,
		Kind:       kind,
		TypeName:   typeName,
		MatchKind:  matchKind,
		Supported:  rule.Supported,
		Revision:   rule.Revision,
		Note:       rule.Note,
		PolicyFunc: rule.PolicyFunc,
	}
}

func (r *TargetCapabilityRegistry) Register(cap TargetCapability) {
	if r == nil {
		return
	}

	rule := normalizeTargetCapabilityRule(cap.rule())

	r.mu.Lock()
	defer r.mu.Unlock()
	r.exact[targetCapabilityKey{
		Provider: cap.Provider,
		Kind:     cap.Kind,
		TypeName: cap.TypeName,
	}] = rule
}

func (r *TargetCapabilityRegistry) RegisterProviderKind(provider addrs.Provider, kind catalog.RunnerKind, rule TargetCapabilityRule) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.providerKind[targetCapabilityProviderKindKey{
		Provider: provider,
		Kind:     kind,
	}] = normalizeTargetCapabilityRule(rule)
}

func (r *TargetCapabilityRegistry) RegisterProvider(provider addrs.Provider, rule TargetCapabilityRule) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.provider[provider] = normalizeTargetCapabilityRule(rule)
}

func (r *TargetCapabilityRegistry) Capability(provider addrs.Provider, kind catalog.RunnerKind, typeName string) (ResolvedTargetCapability, bool) {
	if r == nil {
		return ResolvedTargetCapability{}, false
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if rule, ok := r.exact[targetCapabilityKey{
		Provider: provider,
		Kind:     kind,
		TypeName: typeName,
	}]; ok {
		return resolvedTargetCapability(provider, kind, typeName, TargetCapabilityMatchExact, rule), true
	}
	if rule, ok := r.providerKind[targetCapabilityProviderKindKey{
		Provider: provider,
		Kind:     kind,
	}]; ok {
		return resolvedTargetCapability(provider, kind, typeName, TargetCapabilityMatchProviderKind, rule), true
	}
	if rule, ok := r.provider[provider]; ok {
		return resolvedTargetCapability(provider, kind, typeName, TargetCapabilityMatchProvider, rule), true
	}

	return ResolvedTargetCapability{}, false
}

func apkoBuildPolicy(req TargetRequest) (TargetCapabilityPolicy, tfdiags.Diagnostics) {
	_, _, config, diags := DecodeTargetRequest(req, tfdiags.SourceRange{})
	if diags.HasErrors() {
		return TargetCapabilityPolicy{}, diags
	}

	repo := config.GetAttr("repo")
	if !repo.IsKnown() || repo.IsNull() || repo.Type() != cty.String {
		return TargetCapabilityPolicy{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid apko_build request",
			fmt.Sprintf("Target request for %s %q must include a known string repo attribute to derive build-mode lock policy.", req.Kind, req.TypeName),
		))
	}

	return TargetCapabilityPolicy{
		Cacheable: false,
		Volatile:  false,
		LockKeys:  []string{"repo/" + repo.AsString()},
	}, nil
}
