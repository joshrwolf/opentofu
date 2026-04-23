// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
)

type Binding struct {
	Module        catalog.ModulePath
	Local         addrs.LocalProviderConfig
	Provider      addrs.Provider
	ConfigKey     digest.Digest
	ConfigRequest *ConfigRequest
}

func NewBinding(module catalog.ModulePath, local addrs.LocalProviderConfig, provider addrs.Provider, configKey digest.Digest, configRequest *ConfigRequest) Binding {
	return Binding{
		Module:        module,
		Local:         local,
		Provider:      provider,
		ConfigKey:     configKey,
		ConfigRequest: configRequest,
	}
}

func BindingKey(module catalog.ModulePath, local addrs.LocalProviderConfig, provider addrs.Provider, configDigest digest.Digest) digest.Digest {
	return digest.FromStrings(
		"build-provider-binding-v1",
		module.Identity().String(),
		local.LocalName,
		local.Alias,
		provider.String(),
		configDigest.String(),
	)
}

func EmptyConfigDigest() digest.Digest {
	return digest.FromStrings("build-provider-config-v1", "empty")
}

func EmptyConfigKey(module catalog.ModulePath, local addrs.LocalProviderConfig, provider addrs.Provider) digest.Digest {
	return BindingKey(module, local, provider, EmptyConfigDigest())
}
