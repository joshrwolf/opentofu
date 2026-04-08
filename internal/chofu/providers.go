// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"log"
	"sync"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/plugins"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// providerCache manages provider schemas and configured instances.
type providerCache struct {
	manager   plugins.ProviderManager
	schemas   map[addrs.Provider]providers.ProviderSchema
	mu        sync.Mutex
	instances map[string]providers.Configured // keyed by providerLookupKey
	funcs     sync.Map                        // providerKey::funcName → function.Function (populated lazily, read-heavy)
}

func newProviderCache(manager plugins.ProviderManager) *providerCache {
	return &providerCache{
		manager:   manager,
		schemas:   make(map[addrs.Provider]providers.ProviderSchema),
		instances: make(map[string]providers.Configured),
	}
}

// FetchSchemas concurrently fetches schemas for all provider types in the config.
func (pc *providerCache) FetchSchemas(ctx context.Context, providerTypes []addrs.Provider) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, addr := range providerTypes {
		wg.Go(func() {
			schema, schemaDiags := pc.manager.GetProviderSchema(ctx, addr)
			mu.Lock()
			pc.schemas[addr] = schema
			diags = diags.Append(schemaDiags)
			mu.Unlock()
		})
	}
	wg.Wait()

	log.Printf("[INFO] chofu/providers: fetched schemas for %d providers", len(pc.schemas))
	return diags
}

// ConfigureProvider creates and configures a provider instance. Called
// when a KindProviderConfig vertex is evaluated during the walk.
// The lock is held through creation to prevent duplicate instances
// when concurrent walks configure the same provider.
func (pc *providerCache) ConfigureProvider(ctx context.Context, addr addrs.Provider, alias string, configVal cty.Value) (providers.Configured, tfdiags.Diagnostics) {
	key := providerLookupKey(addr, alias)

	pc.mu.Lock()
	if inst, ok := pc.instances[key]; ok {
		pc.mu.Unlock()
		return inst, nil
	}

	configured, diags := pc.manager.NewConfiguredProvider(ctx, addr, configVal)
	if diags.HasErrors() {
		pc.mu.Unlock()
		return nil, diags
	}

	pc.instances[key] = configured
	pc.mu.Unlock()

	log.Printf("[TRACE] chofu/providers: configured %s", key)
	return configured, diags
}

// GetConfigured returns an already-configured provider instance.
func (pc *providerCache) GetConfigured(addr addrs.Provider, alias string) (providers.Configured, bool) {
	key := providerLookupKey(addr, alias)
	pc.mu.Lock()
	defer pc.mu.Unlock()
	inst, ok := pc.instances[key]
	return inst, ok
}

// Close shuts down all provider instances.
func (pc *providerCache) Close(ctx context.Context) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	for key, inst := range pc.instances {
		if err := inst.Close(ctx); err != nil {
			log.Printf("[WARN] chofu/providers: error closing %s: %s", key, err)
		}
	}
	pc.instances = make(map[string]providers.Configured)
}

// collectProviderTypes returns all distinct provider types from a config tree.
func collectProviderTypes(config *configs.Config) []addrs.Provider {
	seen := make(map[addrs.Provider]bool)
	var result []addrs.Provider

	config.DeepEach(func(c *configs.Config) {
		for _, rc := range c.Module.ManagedResources {
			addr := c.Module.ProviderForLocalConfig(rc.ProviderConfigAddr())
			if !seen[addr] {
				seen[addr] = true
				result = append(result, addr)
			}
		}
		for _, rc := range c.Module.DataResources {
			addr := c.Module.ProviderForLocalConfig(rc.ProviderConfigAddr())
			if !seen[addr] {
				seen[addr] = true
				result = append(result, addr)
			}
		}
	})
	return result
}
