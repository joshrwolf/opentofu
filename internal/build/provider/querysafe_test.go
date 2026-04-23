// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
)

func TestDefaultQuerySafetyRegistry(t *testing.T) {
	reg := NewQuerySafetyRegistry()

	if !reg.IsQuerySafe(addrs.ProviderFunction{ProviderName: "oci", Function: "parse"}) {
		t.Fatal("expected provider::oci::parse to be query-safe")
	}
	if !reg.IsQuerySafe(addrs.ProviderFunction{ProviderName: "time", Function: "rfc3339_parse"}) {
		t.Fatal("expected provider::time::rfc3339_parse to be query-safe")
	}
	if !reg.IsQuerySafe(addrs.ProviderFunction{ProviderName: "time", ProviderAlias: "alt", Function: "rfc3339_parse"}) {
		t.Fatal("expected aliased provider::time::alt::rfc3339_parse to be query-safe")
	}
	if reg.IsQuerySafe(addrs.ProviderFunction{ProviderName: "oci", Function: "get"}) {
		t.Fatal("expected provider::oci::get to default to non-query-safe")
	}
}

func TestDefaultQuerySafetyRegistryIsSingleton(t *testing.T) {
	a := DefaultQuerySafetyRegistry()
	b := DefaultQuerySafetyRegistry()
	if a != b {
		t.Fatal("expected default registry to return the same instance")
	}
}
