// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/tfdiags"
)

// WalkFunc is called for each vertex during the graph walk. It receives the
// vertex ID and the vertex itself, and returns the evaluated value plus any
// diagnostics. The walk guarantees that all dependencies have completed
// before a vertex is dispatched.
type WalkFunc func(ctx context.Context, id ID, v *Vertex) tfdiags.Diagnostics

// Walk executes a topological walk of the graph using a bounded worker pool.
// Vertices become ready when all their dependencies have completed. Workers
// pull ready vertices from a shared queue.
//
// When a vertex produces errors, all transitive dependents are skipped and
// receive a synthetic error diagnostic. Skipped vertices have their result
// slot set to cty.DynamicVal so downstream code never encounters cty.NilVal
// (which has a nil type pointer and panics inside hcldec/cty).
func Walk(ctx context.Context, g *Graph, results *Results, workers int, cb WalkFunc) tfdiags.Diagnostics {
	n := len(g.Verts)
	if n == 0 {
		return nil
	}
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers > n {
		workers = n
	}

	// Copy in-degrees so we can decrement atomically.
	pending := make([]atomic.Int32, n)
	for i := range g.Deps {
		pending[i].Store(int32(len(g.Deps[i])))
	}

	// Per-vertex diagnostics, collected after walk.
	vertDiags := make([]tfdiags.Diagnostics, n)

	// Ready queue seeded with zero-indegree vertices.
	ready := make(chan ID, n)
	for i := range n {
		if pending[i].Load() == 0 {
			ready <- ID(i)
		}
	}

	var completed atomic.Int64

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for id := range ready {
				// Skip if any dependency errored or was itself skipped.
				// This cascades transitively: a skipped vertex gets an
				// error diagnostic, so its dependents also skip.
				var skipDep ID
				skip := false
				for _, dep := range g.Deps[id] {
					if vertDiags[dep].HasErrors() {
						skipDep = dep
						skip = true
						break
					}
				}
				if skip {
					results.Values[id] = cty.DynamicVal
					vertDiags[id] = tfdiags.Diagnostics{}.Append(
						tfdiags.Sourceless(tfdiags.Error,
							fmt.Sprintf("skipped %s due to upstream error in vertex %d", g.Verts[id].Name, skipDep),
							"",
						),
					)
				} else {
					vertDiags[id] = cb(ctx, id, &g.Verts[id])
				}

				// Notify dependents.
				for _, succ := range g.RevDeps[id] {
					if pending[succ].Add(-1) == 0 {
						ready <- succ
					}
				}

				if completed.Add(1) == int64(n) {
					close(ready)
				}
			}
		})
	}

	wg.Wait()

	// Collect all diagnostics.
	var diags tfdiags.Diagnostics
	for _, d := range vertDiags {
		diags = diags.Append(d)
	}
	return diags
}
