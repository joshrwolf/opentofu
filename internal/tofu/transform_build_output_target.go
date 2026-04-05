// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"fmt"
	"log"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/dag"
)

// OutputTargetTransformer prunes the graph to only the backward closure of
// the named output(s). This is the build-mode equivalent of asking "give me
// only what's needed to produce this output."
//
// If OutputTargets is empty, this transformer is a no-op.
type OutputTargetTransformer struct {
	OutputTargets []string
}

func (t *OutputTargetTransformer) Transform(_ context.Context, g *Graph) error {
	if len(t.OutputTargets) == 0 {
		return nil
	}

	targetSet := make(map[string]bool, len(t.OutputTargets))
	for _, name := range t.OutputTargets {
		targetSet[name] = true
	}

	// Find output nodes by type-asserting for the GraphNodeModulePath and
	// output-address interfaces, rather than parsing vertex name strings.
	keep := make(dag.Set)
	found := 0
	for _, v := range g.Vertices() {
		mp, isModulePath := v.(GraphNodeModulePath)
		if !isModulePath {
			continue
		}
		// Only target root module outputs.
		if !mp.ModulePath().IsRoot() {
			continue
		}
		// Check if the node provides a referenceable output address.
		// Root outputs are not referenceable (they return nil), so we
		// fall back to checking if the node's name contains the output.
		// Use the graphNodeExpandedOutput interface which carries the
		// output value address.
		on, isOutput := v.(graphNodeOutputAddr)
		if !isOutput {
			continue
		}
		if !targetSet[on.outputValueAddr().Name] {
			continue
		}

		found++
		keep.Add(v)
		ancestors, _ := g.Ancestors(v)
		for _, a := range ancestors {
			keep.Add(a)
		}
	}

	if found == 0 {
		return fmt.Errorf("no root output nodes found matching targets %v; "+
			"available outputs must be declared in the root module", t.OutputTargets)
	}

	for _, v := range g.Vertices() {
		if !keep.Include(v) {
			log.Printf("[DEBUG] OutputTargetTransformer: removing %q, not in backward closure of targeted outputs", dag.VertexName(v))
			g.Remove(v)
		}
	}

	return nil
}

// graphNodeOutputAddr is satisfied by output graph nodes that carry their
// output value address. This avoids coupling to the unexported
// nodeExpandOutput type and avoids brittle vertex-name string parsing.
type graphNodeOutputAddr interface {
	outputValueAddr() addrs.OutputValue
}
