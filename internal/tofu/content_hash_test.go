// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"testing"

	"github.com/zclconf/go-cty/cty"
)

func TestComputeContentHash(t *testing.T) {
	t.Parallel()

	baseConfig := cty.ObjectVal(map[string]cty.Value{
		"image": cty.StringVal("nginx:latest"),
		"port":  cty.NumberIntVal(8080),
	})

	tests := []struct {
		name      string
		fn        func(t *testing.T)
	}{
		{
			name: "deterministic",
			fn: func(t *testing.T) {
				t.Parallel()
				hash1 := ComputeContentHash("apko_build", baseConfig, nil)
				hash2 := ComputeContentHash("apko_build", baseConfig, nil)
				if hash1 != hash2 {
					t.Errorf("same inputs produced different hashes: %s vs %s", hash1, hash2)
				}
				if len(hash1) != 64 {
					t.Errorf("expected 64 char hex string, got %d chars", len(hash1))
				}
			},
		},
		{
			name: "different types differ",
			fn: func(t *testing.T) {
				t.Parallel()
				config := cty.ObjectVal(map[string]cty.Value{"image": cty.StringVal("nginx:latest")})
				h1 := ComputeContentHash("apko_build", config, nil)
				h2 := ComputeContentHash("cosign_sign", config, nil)
				if h1 == h2 {
					t.Error("different resource types with same config should produce different hashes")
				}
			},
		},
		{
			name: "different configs differ",
			fn: func(t *testing.T) {
				t.Parallel()
				c1 := cty.ObjectVal(map[string]cty.Value{"image": cty.StringVal("nginx:1.25")})
				c2 := cty.ObjectVal(map[string]cty.Value{"image": cty.StringVal("nginx:1.26")})
				if ComputeContentHash("apko_build", c1, nil) == ComputeContentHash("apko_build", c2, nil) {
					t.Error("different configs should produce different hashes")
				}
			},
		},
		{
			name: "dependency hashes cascade",
			fn: func(t *testing.T) {
				t.Parallel()
				config := cty.ObjectVal(map[string]cty.Value{"image": cty.StringVal("sha256:abc123")})
				h1 := ComputeContentHash("cosign_sign", config, map[string]string{"apko_build.this": "aaa111"})
				h2 := ComputeContentHash("cosign_sign", config, map[string]string{"apko_build.this": "bbb222"})
				if h1 == h2 {
					t.Error("different dependency hashes should cascade")
				}
			},
		},
		{
			name: "dependency hash order irrelevant",
			fn: func(t *testing.T) {
				t.Parallel()
				config := cty.ObjectVal(map[string]cty.Value{"input": cty.StringVal("test")})
				h1 := ComputeContentHash("r", config, map[string]string{"a": "1", "b": "2", "c": "3"})
				h2 := ComputeContentHash("r", config, map[string]string{"c": "3", "a": "1", "b": "2"})
				if h1 != h2 {
					t.Error("dependency hash order should not affect content hash")
				}
			},
		},
		{
			name: "nil vs empty deps equal",
			fn: func(t *testing.T) {
				t.Parallel()
				config := cty.ObjectVal(map[string]cty.Value{"input": cty.StringVal("test")})
				h1 := ComputeContentHash("r", config, nil)
				h2 := ComputeContentHash("r", config, map[string]string{})
				if h1 != h2 {
					t.Error("nil and empty dep hashes should produce same hash")
				}
			},
		},
		{
			name: "handles unknown values",
			fn: func(t *testing.T) {
				t.Parallel()
				config := cty.ObjectVal(map[string]cty.Value{
					"known":   cty.StringVal("hello"),
					"unknown": cty.UnknownVal(cty.String),
				})
				hash := ComputeContentHash("r", config, nil)
				if hash == "" {
					t.Error("should produce a non-empty hash with unknown values")
				}
			},
		},
		{
			name: "sensitive marks stripped",
			fn: func(t *testing.T) {
				t.Parallel()
				marked := cty.ObjectVal(map[string]cty.Value{
					"password": cty.StringVal("secret").Mark("sensitive"),
					"username": cty.StringVal("admin"),
				})
				unmarked := cty.ObjectVal(map[string]cty.Value{
					"password": cty.StringVal("secret"),
					"username": cty.StringVal("admin"),
				})
				if ComputeContentHash("r", marked, nil) != ComputeContentHash("r", unmarked, nil) {
					t.Error("sensitive marks should not affect content hash")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, tc.fn)
	}
}
