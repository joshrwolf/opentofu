// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

func TestHashValue_Deterministic(t *testing.T) {
	t.Parallel()

	// The same value must produce the same hash every time.
	vals := []cty.Value{
		cty.StringVal("hello"),
		cty.NumberIntVal(42),
		cty.True,
		cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
		cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal("nginx"),
			"port": cty.NumberIntVal(80),
		}),
	}

	for _, val := range vals {
		h1 := sha256.New()
		h2 := sha256.New()
		hashValue(h1, val, nil)
		hashValue(h2, val, nil)
		if h1.Sum(nil)[0] != h2.Sum(nil)[0] {
			t.Errorf("non-deterministic hash for %s", val.GoString())
		}
		s1, s2 := h1.Sum(nil), h2.Sum(nil)
		for i := range s1 {
			if s1[i] != s2[i] {
				t.Fatalf("non-deterministic hash for %s at byte %d", val.GoString(), i)
			}
		}
	}
}

func TestHashValue_TypeDiscrimination(t *testing.T) {
	t.Parallel()

	// Structurally different values must hash differently.
	tests := []struct {
		name string
		a, b cty.Value
	}{
		{"string vs number", cty.StringVal("1"), cty.NumberIntVal(1)},
		{"true vs false", cty.True, cty.False},
		{"string vs bool", cty.StringVal("true"), cty.True},
		{"null vs empty string", cty.NullVal(cty.String), cty.StringVal("")},
		{"empty list vs empty object", cty.EmptyTupleVal, cty.EmptyObjectVal},
		{"different list elements",
			cty.ListVal([]cty.Value{cty.StringVal("a")}),
			cty.ListVal([]cty.Value{cty.StringVal("b")}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ha := sha256.New()
			hb := sha256.New()
			hashValue(ha, tc.a, nil)
			hashValue(hb, tc.b, nil)
			sa, sb := ha.Sum(nil), hb.Sum(nil)
			for i := range sa {
				if sa[i] != sb[i] {
					return // different — pass
				}
			}
			t.Errorf("hash collision: %s and %s produced the same digest", tc.a.GoString(), tc.b.GoString())
		})
	}
}

func TestHashValue_Marks(t *testing.T) {
	t.Parallel()

	// Marks are metadata — they must not affect the content hash.
	plain := cty.StringVal("secret")
	marked := plain.Mark("sensitive")

	hp := sha256.New()
	hm := sha256.New()
	hashValue(hp, plain, nil)
	hashValue(hm, marked, nil)

	sp, sm := hp.Sum(nil), hm.Sum(nil)
	for i := range sp {
		if sp[i] != sm[i] {
			t.Fatal("marked and unmarked values should hash identically")
		}
	}
}

func TestHashValue_DeepMarks(t *testing.T) {
	t.Parallel()

	// Marks nested inside a structure must also be stripped.
	plain := cty.ObjectVal(map[string]cty.Value{
		"key": cty.StringVal("value"),
	})
	marked := cty.ObjectVal(map[string]cty.Value{
		"key": cty.StringVal("value").Mark("sensitive"),
	})

	hp := sha256.New()
	hm := sha256.New()
	hashValue(hp, plain, nil)
	hashValue(hm, marked, nil)

	sp, sm := hp.Sum(nil), hm.Sum(nil)
	for i := range sp {
		if sp[i] != sm[i] {
			t.Fatal("deep-marked and plain values should hash identically")
		}
	}
}

func TestHashValue_Unknowns(t *testing.T) {
	t.Parallel()

	// Unknown values hash the same as null — "not yet known" is treated
	// as "no content to hash."
	unknowns := []cty.Value{
		cty.UnknownVal(cty.String),
		cty.UnknownVal(cty.Number),
		cty.UnknownVal(cty.Bool),
		cty.DynamicVal,
	}
	hNull := sha256.New()
	hashValue(hNull, cty.NullVal(cty.String), nil)
	nullDigest := hNull.Sum(nil)

	for _, u := range unknowns {
		hu := sha256.New()
		hashValue(hu, u, nil)
		ud := hu.Sum(nil)
		for i := range nullDigest {
			if nullDigest[i] != ud[i] {
				t.Errorf("unknown %s should hash same as null", u.GoString())
				break
			}
		}
	}
}

func TestHashValue_MapKeyOrder(t *testing.T) {
	t.Parallel()

	// Two maps with the same entries must hash identically regardless
	// of Go map iteration order. We build them separately to maximize
	// the chance of different internal ordering.
	a := cty.MapVal(map[string]cty.Value{
		"alpha": cty.NumberIntVal(1),
		"beta":  cty.NumberIntVal(2),
		"gamma": cty.NumberIntVal(3),
	})
	b := cty.MapVal(map[string]cty.Value{
		"gamma": cty.NumberIntVal(3),
		"alpha": cty.NumberIntVal(1),
		"beta":  cty.NumberIntVal(2),
	})

	ha := sha256.New()
	hb := sha256.New()
	hashValue(ha, a, nil)
	hashValue(hb, b, nil)

	sa, sb := ha.Sum(nil), hb.Sum(nil)
	for i := range sa {
		if sa[i] != sb[i] {
			t.Fatal("maps with same entries should hash identically")
		}
	}
}

func TestHashValue_ObjectKeyOrder(t *testing.T) {
	t.Parallel()

	a := cty.ObjectVal(map[string]cty.Value{
		"z": cty.True,
		"a": cty.False,
		"m": cty.StringVal("mid"),
	})
	b := cty.ObjectVal(map[string]cty.Value{
		"a": cty.False,
		"m": cty.StringVal("mid"),
		"z": cty.True,
	})

	ha := sha256.New()
	hb := sha256.New()
	hashValue(ha, a, nil)
	hashValue(hb, b, nil)

	sa, sb := ha.Sum(nil), hb.Sum(nil)
	for i := range sa {
		if sa[i] != sb[i] {
			t.Fatal("objects with same attributes should hash identically")
		}
	}
}

func TestHashValue_SetOrder(t *testing.T) {
	t.Parallel()

	// Sets are unordered — elements must produce the same hash regardless
	// of insertion order.
	a := cty.SetVal([]cty.Value{
		cty.StringVal("x"),
		cty.StringVal("y"),
		cty.StringVal("z"),
	})
	b := cty.SetVal([]cty.Value{
		cty.StringVal("z"),
		cty.StringVal("x"),
		cty.StringVal("y"),
	})

	ha := sha256.New()
	hb := sha256.New()
	hashValue(ha, a, nil)
	hashValue(hb, b, nil)

	sa, sb := ha.Sum(nil), hb.Sum(nil)
	for i := range sa {
		if sa[i] != sb[i] {
			t.Fatal("sets with same elements should hash identically")
		}
	}
}

func TestHashValue_NestedStructure(t *testing.T) {
	t.Parallel()

	// A representative resource config shape.
	val := cty.ObjectVal(map[string]cty.Value{
		"name": cty.StringVal("nginx"),
		"image": cty.ObjectVal(map[string]cty.Value{
			"repository": cty.StringVal("cgr.dev/chainguard/nginx"),
			"tag":        cty.StringVal("latest"),
		}),
		"ports": cty.ListVal([]cty.Value{
			cty.ObjectVal(map[string]cty.Value{
				"container_port": cty.NumberIntVal(80),
				"protocol":       cty.StringVal("TCP"),
			}),
			cty.ObjectVal(map[string]cty.Value{
				"container_port": cty.NumberIntVal(443),
				"protocol":       cty.StringVal("TCP"),
			}),
		}),
		"tags": cty.MapVal(map[string]cty.Value{
			"env":     cty.StringVal("prod"),
			"service": cty.StringVal("web"),
		}),
		"replicas": cty.NumberIntVal(3),
		"enabled":  cty.True,
	})

	h1 := sha256.New()
	h2 := sha256.New()
	hashValue(h1, val, nil)
	hashValue(h2, val, nil)

	s1, s2 := h1.Sum(nil), h2.Sum(nil)
	for i := range s1 {
		if s1[i] != s2[i] {
			t.Fatal("nested structure hash is non-deterministic")
		}
	}
}

func TestHashValue_EmptyCollections(t *testing.T) {
	t.Parallel()

	tests := map[string]cty.Value{
		"empty tuple":  cty.EmptyTupleVal,
		"empty object": cty.EmptyObjectVal,
		"empty list":   cty.ListValEmpty(cty.String),
		"empty map":    cty.MapValEmpty(cty.String),
		"empty set":    cty.SetValEmpty(cty.String),
	}

	// Each should hash without panicking and produce a non-zero digest.
	for name, val := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := sha256.New()
			hashValue(h, val, nil)
			d := h.Sum(nil)
			allZero := true
			for _, b := range d {
				if b != 0 {
					allZero = false
					break
				}
			}
			if allZero {
				t.Error("digest should not be all zeros")
			}
		})
	}
}

func TestHashValue_NumberPrecision(t *testing.T) {
	t.Parallel()

	// Integer and float representations of the same value should hash
	// identically because big.Float.Append('g', 20) normalizes them.
	a := cty.NumberIntVal(42)
	b := cty.NumberFloatVal(42.0)

	ha := sha256.New()
	hb := sha256.New()
	hashValue(ha, a, nil)
	hashValue(hb, b, nil)

	sa, sb := ha.Sum(nil), hb.Sum(nil)
	for i := range sa {
		if sa[i] != sb[i] {
			t.Fatal("42 (int) and 42.0 (float) should hash identically")
		}
	}
}

func TestComputeContentHash_Deterministic(t *testing.T) {
	t.Parallel()

	configVal := cty.ObjectVal(map[string]cty.Value{
		"type":   cty.StringVal("apko_build"),
		"config": cty.StringVal(`{"packages":["nginx"]}`),
		"count":  cty.NumberIntVal(1),
	})
	depHashes := map[string]string{
		"module.nginx.data.apko_config.this": "abc123",
		"module.nginx.random_pet.suffix":     "def456",
	}

	h1 := ComputeContentHash("apko_build", configVal, depHashes)
	h2 := ComputeContentHash("apko_build", configVal, depHashes)

	if h1 != h2 {
		t.Fatalf("non-deterministic content hash: %s vs %s", h1, h2)
	}
	if len(h1) != 64 { // SHA-256 hex
		t.Fatalf("unexpected hash length: %d", len(h1))
	}
}

func TestComputeContentHash_Sensitivity(t *testing.T) {
	t.Parallel()

	base := cty.ObjectVal(map[string]cty.Value{
		"image": cty.StringVal("nginx:1.27"),
	})
	changed := cty.ObjectVal(map[string]cty.Value{
		"image": cty.StringVal("nginx:1.28"),
	})

	h1 := ComputeContentHash("apko_build", base, nil)
	h2 := ComputeContentHash("apko_build", changed, nil)

	if h1 == h2 {
		t.Fatal("different config values should produce different hashes")
	}
}

func TestComputeContentHash_DepHashOrder(t *testing.T) {
	t.Parallel()

	config := cty.ObjectVal(map[string]cty.Value{
		"name": cty.StringVal("test"),
	})

	// Dependency hashes are sorted by key, so insertion order must not matter.
	deps1 := map[string]string{"a": "1", "b": "2", "c": "3"}
	deps2 := map[string]string{"c": "3", "a": "1", "b": "2"}

	h1 := ComputeContentHash("test", config, deps1)
	h2 := ComputeContentHash("test", config, deps2)

	if h1 != h2 {
		t.Fatal("dependency hash order should not affect content hash")
	}
}

// BenchmarkComputeContentHash_JSONBaseline benchmarks the old approach:
// cty/json.Marshal → SHA-256. This exists to quantify the improvement from
// the streaming hashValue approach.
func BenchmarkComputeContentHash_JSONBaseline(b *testing.B) {
	configVal := benchConfigVal()
	depHashes := benchDepHashes()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		computeContentHashJSON(configVal, depHashes)
	}
}

// computeContentHashJSON is the old JSON-based implementation, kept as a
// benchmark baseline.
func computeContentHashJSON(configVal cty.Value, depHashes map[string]string) string {
	h := sha256.New()
	h.Write([]byte("apko_build"))
	h.Write([]byte{0})

	unmarked, _ := configVal.UnmarkDeep()
	unmarked = cty.UnknownAsNull(unmarked)
	jsonBytes, err := ctyjson.Marshal(unmarked, unmarked.Type())
	if err != nil {
		jsonBytes = []byte(unmarked.GoString())
	}
	h.Write(jsonBytes)
	h.Write([]byte{0})

	keys := make([]string, 0, len(depHashes))
	for k := range depHashes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(depHashes[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func benchConfigVal() cty.Value {
	return cty.ObjectVal(map[string]cty.Value{
		"name":              cty.StringVal("nginx"),
		"target_repository": cty.StringVal("cgr.dev/chainguard/nginx"),
		"main_package":      cty.StringVal("nginx-mainline"),
		"eol":               cty.False,
		"update_repo":       cty.True,
		"configs": cty.ObjectVal(map[string]cty.Value{
			"config": cty.StringVal(`{"contents":{"packages":["nginx-mainline","nginx-mainline-config"]},"accounts":{"groups":[{"groupname":"nginx","gid":65532}],"users":[{"username":"nginx","uid":65532,"gid":65532}]}}`),
		}),
		"tags": cty.MapVal(map[string]cty.Value{
			"latest":        cty.StringVal("sha256:abc123"),
			"1.27":          cty.StringVal("sha256:abc123"),
			"1.27.0":        cty.StringVal("sha256:abc123"),
			"1.27.0-r0":     cty.StringVal("sha256:abc123"),
			"latest-dev":    cty.StringVal("sha256:def456"),
			"1.27-dev":      cty.StringVal("sha256:def456"),
			"1.27.0-dev":    cty.StringVal("sha256:def456"),
			"1.27.0-r0-dev": cty.StringVal("sha256:def456"),
		}),
		"replicas": cty.NumberIntVal(1),
		"ports": cty.ListVal([]cty.Value{
			cty.NumberIntVal(80),
			cty.NumberIntVal(443),
		}),
	})
}

func benchDepHashes() map[string]string {
	return map[string]string{
		"module.nginx.module.build.module.this.apko_build.this":           "aabbccdd",
		"module.nginx.module.build.module.this.data.apko_config.this":     "eeff0011",
		"module.nginx.module.build.module.this-dev.apko_build.this":       "22334455",
		"module.nginx.module.build.module.this-dev.data.apko_config.this": "66778899",
	}
}

func BenchmarkComputeContentHash(b *testing.B) {
	configVal := benchConfigVal()
	depHashes := benchDepHashes()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ComputeContentHash("apko_build", configVal, depHashes)
	}
}
