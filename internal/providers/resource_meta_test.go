// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package providers

import (
	"testing"
	"time"
)

func TestGetResourceMetaFromProvider(t *testing.T) {
	t.Parallel()

	provider := &mockBuildMetaProvider{
		meta: map[string]ResourceMeta{
			"example_build": {Role: RoleBuild, CachePolicy: CachePolicy{Mode: CacheByInputs}},
			"example_test":  {Role: RoleTest, CachePolicy: CachePolicy{Mode: CacheWithTTL, TTL: 24 * time.Hour}},
			"example_tag":   {Role: RolePublish, CachePolicy: CachePolicy{Mode: NeverCache}},
		},
	}

	tests := []struct {
		typeName  string
		wantRole  ResourceRole
		wantCache CacheMode
		wantTTL   time.Duration
	}{
		{"example_build", RoleBuild, CacheByInputs, 0},
		{"example_test", RoleTest, CacheWithTTL, 24 * time.Hour},
		{"example_tag", RolePublish, NeverCache, 0},
		{"unknown_resource", RoleDefault, CacheByInputs, 0}, // fallback to default
	}

	for _, tc := range tests {
		t.Run(tc.typeName, func(t *testing.T) {
			t.Parallel()
			meta := GetResourceMetaFromProvider(provider, tc.typeName)
			if meta.Role != tc.wantRole {
				t.Errorf("Role = %s, want %s", meta.Role, tc.wantRole)
			}
			if meta.CachePolicy.Mode != tc.wantCache {
				t.Errorf("CacheMode = %d, want %d", meta.CachePolicy.Mode, tc.wantCache)
			}
			if tc.wantTTL != 0 && meta.CachePolicy.TTL != tc.wantTTL {
				t.Errorf("TTL = %s, want %s", meta.CachePolicy.TTL, tc.wantTTL)
			}
		})
	}
}

func TestDefaultResourceMeta(t *testing.T) {
	t.Parallel()
	meta := DefaultResourceMeta()
	if meta.Role != RoleDefault {
		t.Errorf("Role = %s, want RoleDefault", meta.Role)
	}
	if meta.CachePolicy.Mode != CacheByInputs {
		t.Errorf("CacheMode = %d, want CacheByInputs", meta.CachePolicy.Mode)
	}
}

// mockBuildMetaProvider implements BuildMetaProvider for testing.
type mockBuildMetaProvider struct {
	Interface // embed to satisfy the full interface
	meta      map[string]ResourceMeta
}

func (m *mockBuildMetaProvider) GetResourceMeta() map[string]ResourceMeta {
	return m.meta
}
