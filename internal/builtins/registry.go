// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package builtins provides the registry of providers compiled into the chofu
// binary. These run in-process via the embedded provider adapter, eliminating
// gRPC subprocess overhead and the need for `tofu init` to download them.
//
// Currently empty — providers will be added here as they are built
// specifically for chofu (as opposed to general terraform providers that
// happen to be embedded).
package builtins

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/embedded"
	"github.com/opentofu/opentofu/internal/providers"
)

// ProviderEntry maps a provider source address to its in-process factory.
type ProviderEntry struct {
	Source  addrs.Provider
	Factory providers.Factory
}

// Registry returns all built-in providers compiled into the binary.
func Registry() []ProviderEntry {
	return nil
}

// frameworkFactory wraps a terraform-plugin-framework provider constructor
// into a providers.Factory that creates in-process provider instances.
func frameworkFactory(newFn func() provider.Provider) providers.Factory {
	return func() (providers.Interface, error) {
		p := newFn()
		server, err := providerserver.NewProtocol6WithError(p)()
		if err != nil {
			return nil, fmt.Errorf("creating in-process provider server: %w", err)
		}
		return embedded.NewProvider(server), nil
	}
}
