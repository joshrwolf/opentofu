// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestWalk(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		graph   *Graph
		results *Results
		workers int

		// cb is called for each non-skipped vertex. Return an error
		// diagnostic to simulate failure.
		cb func(id ID, v *Vertex) tfdiags.Diagnostics

		wantValues map[ID]cty.Value // expected values after walk
		wantErrors bool             // expect walk to return errors
	}{
		"empty graph": {
			graph:   &Graph{},
			results: NewResults(0),
		},

		"single vertex evaluated": {
			graph: linearGraph(1),
			results: NewResults(1),
			cb: func(id ID, v *Vertex) tfdiags.Diagnostics {
				return nil
			},
		},

		"linear chain evaluates in order": {
			// A → B → C (C depends on B, B depends on A)
			graph:   linearGraph(3),
			results: NewResults(3),
			cb: func(id ID, v *Vertex) tfdiags.Diagnostics {
				return nil
			},
		},

		"callback writes values": {
			graph:   linearGraph(2),
			results: NewResults(2),
			cb: func(id ID, v *Vertex) tfdiags.Diagnostics {
				return nil
			},
			wantValues: map[ID]cty.Value{
				0: cty.StringVal("v0"),
				1: cty.StringVal("v1"),
			},
		},

		"error in root skips dependents": {
			// 0 → 1 → 2 (2 depends on 1, 1 depends on 0)
			// Vertex 0 errors. Vertices 1 and 2 should be skipped
			// with DynamicVal and error diagnostics.
			graph:      linearGraph(3),
			results:    NewResults(3),
			wantErrors: true,
			wantValues: map[ID]cty.Value{
				// 0 gets no value written by walk (cb errors, cb writes nothing)
				1: cty.DynamicVal, // skipped
				2: cty.DynamicVal, // skipped transitively
			},
		},

		"error in middle skips downstream only": {
			// 0 → 1 → 2
			// Vertex 1 errors. Vertex 0 should succeed, vertex 2 should be skipped.
			graph:      linearGraph(3),
			results:    NewResults(3),
			wantErrors: true,
			wantValues: map[ID]cty.Value{
				0: cty.StringVal("v0"), // succeeds
				// 1 gets no value from walk (cb errors)
				2: cty.DynamicVal, // skipped
			},
		},

		"independent vertices all evaluate": {
			// 0, 1, 2 with no edges — all independent
			graph:   independentGraph(3),
			results: NewResults(3),
			cb: func(id ID, v *Vertex) tfdiags.Diagnostics {
				return nil
			},
			wantValues: map[ID]cty.Value{
				0: cty.StringVal("v0"),
				1: cty.StringVal("v1"),
				2: cty.StringVal("v2"),
			},
		},

		"diamond graph": {
			//   0
			//  / \
			// 1   2
			//  \ /
			//   3
			graph:   diamondGraph(),
			results: NewResults(4),
			cb: func(id ID, v *Vertex) tfdiags.Diagnostics {
				return nil
			},
			wantValues: map[ID]cty.Value{
				0: cty.StringVal("v0"),
				1: cty.StringVal("v1"),
				2: cty.StringVal("v2"),
				3: cty.StringVal("v3"),
			},
		},

		"diamond graph with root error skips all": {
			graph:      diamondGraph(),
			results:    NewResults(4),
			wantErrors: true,
			wantValues: map[ID]cty.Value{
				1: cty.DynamicVal,
				2: cty.DynamicVal,
				3: cty.DynamicVal,
			},
		},

		"single worker serializes execution": {
			graph:   independentGraph(5),
			results: NewResults(5),
			workers: 1,
			cb: func(id ID, v *Vertex) tfdiags.Diagnostics {
				return nil
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			workers := tc.workers
			if workers == 0 {
				workers = 4
			}

			// Build callback. The default writes cty.StringVal("vN").
			// Override for error cases based on test name.
			cb := tc.cb
			if cb == nil && !tc.wantErrors {
				cb = func(id ID, v *Vertex) tfdiags.Diagnostics {
					return nil
				}
			}

			// For error tests, build a callback that errors on specific vertices.
			var errorVertex ID = -1
			if tc.wantErrors && cb == nil {
				switch name {
				case "error in root skips dependents", "diamond graph with root error skips all":
					errorVertex = 0
				case "error in middle skips downstream only":
					errorVertex = 1
				}
				cb = func(id ID, v *Vertex) tfdiags.Diagnostics {
					if id == errorVertex {
						var diags tfdiags.Diagnostics
						diags = diags.Append(tfdiags.Sourceless(tfdiags.Error, fmt.Sprintf("vertex %d failed", id), ""))
						return diags
					}
					return nil
				}
			}

			// Wrap cb to also write values for non-error cases.
			wrappedCb := func(ctx context.Context, id ID, v *Vertex) tfdiags.Diagnostics {
				diags := cb(id, v)
				if !diags.HasErrors() {
					tc.results.Values[id] = cty.StringVal(fmt.Sprintf("v%d", id))
				}
				return diags
			}

			diags := Walk(t.Context(), tc.graph, tc.results, workers, wrappedCb)

			if tc.wantErrors && !diags.HasErrors() {
				t.Fatal("expected errors, got none")
			}
			if !tc.wantErrors && diags.HasErrors() {
				t.Fatalf("unexpected errors: %s", diags.Err())
			}

			for id, want := range tc.wantValues {
				got := tc.results.Values[id]
				if !got.RawEquals(want) {
					t.Errorf("results[%d]: got %s, want %s", id, got.GoString(), want.GoString())
				}
			}
		})
	}
}

func TestWalk_AllVerticesComplete(t *testing.T) {
	t.Parallel()

	// Verify that every vertex is either evaluated or skipped —
	// the completed counter must reach n so the channel closes.
	g := linearGraph(10)
	results := NewResults(10)
	var evaluated atomic.Int64

	diags := Walk(t.Context(), g, results, 4, func(ctx context.Context, id ID, v *Vertex) tfdiags.Diagnostics {
		evaluated.Add(1)
		results.Values[id] = cty.True
		return nil
	})

	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}
	if got := evaluated.Load(); got != 10 {
		t.Errorf("evaluated %d vertices, want 10", got)
	}
}

func TestWalk_SkippedVertexGetsDiagnostic(t *testing.T) {
	t.Parallel()

	// Vertex 0 errors → vertex 1 should be skipped with a diagnostic
	// containing "skipped" and "upstream error".
	g := linearGraph(2)
	results := NewResults(2)

	diags := Walk(t.Context(), g, results, 1, func(ctx context.Context, id ID, v *Vertex) tfdiags.Diagnostics {
		if id == 0 {
			var d tfdiags.Diagnostics
			d = d.Append(tfdiags.Sourceless(tfdiags.Error, "root failed", ""))
			return d
		}
		results.Values[id] = cty.True
		return nil
	})

	if !diags.HasErrors() {
		t.Fatal("expected errors")
	}

	// Should have at least 2 errors: the root failure + the skip diagnostic.
	errCount := 0
	foundSkip := false
	for _, d := range diags {
		if d.Severity() == tfdiags.Error {
			errCount++
			if desc := d.Description(); desc.Summary != "" {
				if len(desc.Summary) > 7 && desc.Summary[:7] == "skipped" {
					foundSkip = true
				}
			}
		}
	}

	if errCount < 2 {
		t.Errorf("expected at least 2 error diagnostics, got %d", errCount)
	}
	if !foundSkip {
		t.Error("expected a 'skipped' diagnostic for vertex 1")
	}
	if results.Values[1] != cty.DynamicVal {
		t.Errorf("skipped vertex should have DynamicVal, got %s", results.Values[1].GoString())
	}
}

func TestWalk_LargeFanOut(t *testing.T) {
	t.Parallel()

	// Simulates images-private's ~1,881 independent root module instances.
	// All vertices are independent — tests that the completion counter
	// and channel close work correctly under high concurrency.
	const n = 500
	g := independentGraph(n)
	results := NewResults(n)
	var evaluated atomic.Int64

	diags := Walk(t.Context(), g, results, 16, func(ctx context.Context, id ID, v *Vertex) tfdiags.Diagnostics {
		evaluated.Add(1)
		results.Values[id] = cty.StringVal(fmt.Sprintf("v%d", id))
		return nil
	})

	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}
	if got := evaluated.Load(); got != n {
		t.Errorf("evaluated %d vertices, want %d", got, n)
	}
	for i := range n {
		if results.Values[i] == cty.NilVal {
			t.Errorf("results[%d] is NilVal", i)
			break // one failure is enough
		}
	}
}

// linearGraph builds a chain: 0 ← 1 ← 2 ← ... (each depends on previous).
func linearGraph(n int) *Graph {
	g := &Graph{
		Verts:   make([]Vertex, n),
		Deps:    make([][]ID, n),
		RevDeps: make([][]ID, n),
	}
	for i := range n {
		g.Verts[i] = Vertex{
			Kind:         KindResource,
			Module:       addrs.RootModuleInstance,
			Name:         fmt.Sprintf("v%d", i),
			ResourceAddr: resAddr("test_instance", fmt.Sprintf("v%d", i), addrs.NoKey),
		}
		if i > 0 {
			g.Deps[i] = []ID{ID(i - 1)}
			g.RevDeps[i-1] = append(g.RevDeps[i-1], ID(i))
		}
	}
	return g
}

// independentGraph builds n vertices with no edges.
func independentGraph(n int) *Graph {
	g := &Graph{
		Verts:   make([]Vertex, n),
		Deps:    make([][]ID, n),
		RevDeps: make([][]ID, n),
	}
	for i := range n {
		g.Verts[i] = Vertex{
			Kind:         KindResource,
			Module:       addrs.RootModuleInstance,
			Name:         fmt.Sprintf("v%d", i),
			ResourceAddr: resAddr("test_instance", fmt.Sprintf("v%d", i), addrs.NoKey),
		}
	}
	return g
}

// TestExecuteVertex_LocalCaching verifies that locals with expansion-time
// cached values (LocalVal) skip re-evaluation, while locals without cached
// values are evaluated from their expression during the walk.
func TestExecuteVertex_LocalCaching(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		localVal  cty.Value      // expansion-cached value (NilVal = not cached)
		localExpr string         // HCL expression to evaluate if not cached
		wantValue cty.Value      // expected result after executeVertex
		wantCache bool           // expect localsCached counter to increment
	}{
		"cached known value skips evaluation": {
			localVal:  cty.StringVal(`{"key":"value"}`),
			localExpr: `"should not evaluate"`,
			wantValue: cty.StringVal(`{"key":"value"}`),
			wantCache: true,
		},
		"cached complex object": {
			localVal: cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal("nginx"),
				"tags": cty.ListVal([]cty.Value{cty.StringVal("latest")}),
			}),
			localExpr: `"should not evaluate"`,
			wantValue: cty.ObjectVal(map[string]cty.Value{
				"name": cty.StringVal("nginx"),
				"tags": cty.ListVal([]cty.Value{cty.StringVal("latest")}),
			}),
			wantCache: true,
		},
		"uncached local evaluates expression": {
			localVal:  cty.NilVal,
			localExpr: `"evaluated"`,
			wantValue: cty.StringVal("evaluated"),
			wantCache: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			g := &Graph{
				Verts: []Vertex{{
					Kind:      KindLocal,
					Module:    addrs.RootModuleInstance,
					Name:      "test_local",
					LocalExpr: testExpr(t, tc.localExpr),
					LocalVal:  tc.localVal,
				}},
				Deps:    [][]ID{nil},
				RevDeps: [][]ID{nil},
				Vars:    map[addrKey]ID{},
				Locals:  map[addrKey]ID{},
				Outputs: map[addrKey]ID{},
				Providers: map[string]ID{},
				ResInst: map[string][]ID{},
				Expands: map[string]ID{},
				Closes:  map[string]ID{},
			}
			results := NewResults(1)

			wc := &walkContext{
				graph:   g,
				results: results,
				config: &configs.Config{
					Module: &configs.Module{SourceDir: "/root"},
				},
				workDir:     "/work",
				workspace:   "default",
				sharedFuncs: (&lang.Scope{BaseDir: "/work"}).Functions(),
				ui:          nilBuildUI{},
				stats:       &walkStats{},
			}

			diags := wc.executeVertex(t.Context(), 0, &g.Verts[0])
			if diags.HasErrors() {
				t.Fatalf("unexpected error: %s", diags.Err())
			}

			got := results.Values[0]
			if !got.RawEquals(tc.wantValue) {
				t.Errorf("result: got %s, want %s", got.GoString(), tc.wantValue.GoString())
			}

			cached := wc.stats.localsCached.Load() > 0
			if cached != tc.wantCache {
				t.Errorf("localsCached: got %v, want %v", cached, tc.wantCache)
			}
		})
	}
}

// diamondGraph builds:
//
//	  0
//	 / \
//	1   2
//	 \ /
//	  3
func diamondGraph() *Graph {
	g := &Graph{
		Verts:   make([]Vertex, 4),
		Deps:    make([][]ID, 4),
		RevDeps: make([][]ID, 4),
	}
	for i := range 4 {
		g.Verts[i] = Vertex{
			Kind:         KindResource,
			Module:       addrs.RootModuleInstance,
			Name:         fmt.Sprintf("v%d", i),
			ResourceAddr: resAddr("test_instance", fmt.Sprintf("v%d", i), addrs.NoKey),
		}
	}
	// 1 depends on 0, 2 depends on 0, 3 depends on 1 and 2
	g.Deps[1] = []ID{0}
	g.Deps[2] = []ID{0}
	g.Deps[3] = []ID{1, 2}
	g.RevDeps[0] = []ID{1, 2}
	g.RevDeps[1] = []ID{3}
	g.RevDeps[2] = []ID{3}
	return g
}
