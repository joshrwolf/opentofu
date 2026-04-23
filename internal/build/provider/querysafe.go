// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"sync"

	"github.com/opentofu/opentofu/internal/addrs"
)

type querySafetyKey struct {
	ProviderName string
	Function     string
}

type QuerySafetyRegistry struct {
	mu    sync.RWMutex
	allow map[querySafetyKey]struct{}
}

func NewQuerySafetyRegistry() *QuerySafetyRegistry {
	ret := &QuerySafetyRegistry{
		allow: map[querySafetyKey]struct{}{},
	}
	ret.MarkQuerySafe(addrs.ProviderFunction{ProviderName: "oci", Function: "parse"})
	ret.MarkQuerySafe(addrs.ProviderFunction{ProviderName: "time", Function: "rfc3339_parse"})
	return ret
}

func (r *QuerySafetyRegistry) MarkQuerySafe(fn addrs.ProviderFunction) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allow[querySafetyLookupKey(fn)] = struct{}{}
}

func (r *QuerySafetyRegistry) IsQuerySafe(fn addrs.ProviderFunction) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.allow[querySafetyLookupKey(fn)]
	return ok
}

var defaultQuerySafety = NewQuerySafetyRegistry()

func DefaultQuerySafetyRegistry() *QuerySafetyRegistry {
	return defaultQuerySafety
}

func querySafetyLookupKey(fn addrs.ProviderFunction) querySafetyKey {
	return querySafetyKey{
		ProviderName: fn.ProviderName,
		Function:     fn.Function,
	}
}
