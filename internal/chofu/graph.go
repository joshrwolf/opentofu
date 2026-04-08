// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"fmt"
	"log"
	"runtime"
	"slices"
	"sync"
	"unique"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// BuildGraph constructs a flat, integer-indexed DAG from pre-built vertices.
// Two passes: (1) populate lookup maps, (2) create edges from
// static HCL reference extraction.
func BuildGraph(
	verts []Vertex,
	schemas map[addrs.Provider]providers.ProviderSchema,
) (*Graph, EdgeStats, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	g := &Graph{Verts: verts}
	g.initMaps(len(verts))

	// moduleExternals maps module instance string → externally-observable
	// child vertex IDs (outputs, resources, data sources). Built before
	// edges, used for Close gate dependencies in Pass 2.
	//
	// Close vertices only need to depend on externally-observable content:
	// outputs (read by GetModule/module.X.output_name), resources, and data
	// sources (side effects that depends_on must wait for). Variables and
	// locals are module-internal — they're not referenceable from outside
	// and transitively complete before outputs do.
	moduleExternals := make(map[string][]ID, 64)
	for i := range g.Verts {
		v := &g.Verts[i]
		if len(v.Module) > 0 {
			switch v.Kind {
			case KindOutput, KindResource, KindDataSource:
				modStr := v.Module.String()
				moduleExternals[modStr] = append(moduleExternals[modStr], ID(i))
			}
		}
	}

	// Pass 1: Populate lookup maps from vertices.
	g.rebuildIndexes()
	log.Printf("[INFO] chofu/graph: %d vertices created", len(g.Verts))

	// Attach schemas before edge creation — ReferencesInBlock needs them.
	g.AttachSchemas(schemas)

	// Pass 2: Create edges from HCL reference extraction.
	//
	// Each vertex's edges depend only on the read-only maps built in Pass 1
	// and the vertex's own config. We shard the vertex range across workers
	// and accumulate per-worker edge stats.
	g.Deps = make([][]ID, len(g.Verts))

	n := len(g.Verts)
	workers := max(1, min(runtime.GOMAXPROCS(0), n))

	workerStats := make([]EdgeStats, workers)
	chunk := (n + workers - 1) / workers

	var edgeWg sync.WaitGroup
	for w := range workers {
		start := w * chunk
		end := min(start+chunk, n)
		if start >= end {
			break
		}
		es := &workerStats[w]
		edgeWg.Go(func() {
			for i := start; i < end; i++ {
				g.Deps[i] = g.buildVertexDeps(i, moduleExternals, es)
			}
		})
	}
	edgeWg.Wait()

	// Sum per-worker edge stats.
	var es EdgeStats
	for _, ws := range workerStats {
		es.Total += ws.Total
		es.ExpandGate += ws.ExpandGate
		es.CloseGate += ws.CloseGate
		es.HCLRefs += ws.HCLRefs
		es.DependsOn += ws.DependsOn
		es.Provider += ws.Provider
		es.ExpandRefs += ws.ExpandRefs
		es.ParentExp += ws.ParentExp
	}

	g.buildReverseEdges()

	edgeCount := 0
	for _, deps := range g.Deps {
		edgeCount += len(deps)
	}
	log.Printf("[INFO] chofu/graph: %d edges created (expand_gate=%d close_gate=%d hcl_refs=%d depends_on=%d provider=%d expand_refs=%d parent_expand=%d)",
		edgeCount, es.ExpandGate, es.CloseGate, es.HCLRefs,
		es.DependsOn, es.Provider, es.ExpandRefs, es.ParentExp)

	// Cycle detection via Kahn's dry run.
	if err := g.checkCycles(); err != nil {
		diags = diags.Append(err)
	}

	return g, es, diags
}

// initMaps allocates the lookup maps sized for n vertices.
func (g *Graph) initMaps(n int) {
	g.Vars = make(map[addrKey]ID, n/4)
	g.Locals = make(map[addrKey]ID, n/4)
	g.Outputs = make(map[addrKey]ID, n/8)
	g.Providers = make(map[string]ID, 16)
	g.ResInst = make(map[string][]ID, n/2)
	g.Expands = make(map[string]ID, 64)
	g.Closes = make(map[string]ID, 64)
	g.ChildOutputs = make(map[addrKey][]ID, n/8)
	g.ChildCloses = make(map[addrKey][]ID, 64)
}

// rebuildIndexes populates all lookup maps from the current vertex slice.
// Must be called after Verts is set and whenever Verts changes (e.g. after
// filtering or compaction).
func (g *Graph) rebuildIndexes() {
	for i := range g.Verts {
		id := ID(i)
		v := &g.Verts[i]

		switch v.Kind {
		case KindVariable:
			g.Vars[addrKey{unique.Make(v.Module.String()), unique.Make(v.Name)}] = id

		case KindLocal:
			g.Locals[addrKey{unique.Make(v.Module.String()), unique.Make(v.Name)}] = id

		case KindOutput:
			g.Outputs[addrKey{unique.Make(v.Module.String()), unique.Make(v.Name)}] = id
			if len(v.Module) > 0 {
				parent := v.Module[:len(v.Module)-1]
				callName := v.Module[len(v.Module)-1].Name
				coKey := addrKey{unique.Make(parent.String()), unique.Make(callName)}
				g.ChildOutputs[coKey] = append(g.ChildOutputs[coKey], id)
			}

		case KindResource, KindDataSource:
			resKey := v.ResourceAddr.ContainingResource().String()
			g.ResInst[resKey] = append(g.ResInst[resKey], id)

		case KindProviderConfig:
			provKey := providerLookupKey(v.ProviderAddr, v.ProviderAlias)
			g.Providers[provKey] = id

		case KindModuleExpand:
			g.Expands[v.Module.String()] = id

		case KindModuleClose:
			g.Closes[v.Module.String()] = id
			if len(v.Module) > 0 {
				parent := v.Module[:len(v.Module)-1]
				callName := v.Module[len(v.Module)-1].Name
				ccKey := addrKey{unique.Make(parent.String()), unique.Make(callName)}
				g.ChildCloses[ccKey] = append(g.ChildCloses[ccKey], id)
			}
		}
	}
}

// buildReverseEdges populates RevDeps from Deps.
func (g *Graph) buildReverseEdges() {
	g.RevDeps = make([][]ID, len(g.Verts))
	for v, deps := range g.Deps {
		for _, dep := range deps {
			g.RevDeps[dep] = append(g.RevDeps[dep], ID(v))
		}
	}
}

// buildVertexDeps computes the dependency list for a single vertex.
// All map lookups are against read-only maps built in Pass 1, so this
// is safe to call concurrently for different vertex indices.
func (g *Graph) buildVertexDeps(i int, moduleExternals map[string][]ID, es *EdgeStats) []ID {
	v := &g.Verts[i]
	var deps []ID

	switch v.Kind {
	case KindVariable:
		if len(v.Module) > 0 {
			if eid, ok := g.Expands[v.Module.String()]; ok {
				deps = appendUnique(deps, eid)
				es.ExpandGate++
			}
		}

	case KindLocal:
		refs, _ := lang.ReferencesInExpr(addrs.ParseRef, v.LocalExpr)
		resolved := g.resolveRefs(refs, v.Module)
		es.HCLRefs += len(resolved)
		deps = append(deps, resolved...)

	case KindOutput:
		if v.OutputCfg != nil && v.OutputCfg.Expr != nil {
			refs, _ := lang.ReferencesInExpr(addrs.ParseRef, v.OutputCfg.Expr)
			resolved := g.resolveRefs(refs, v.Module)
			es.HCLRefs += len(resolved)
			deps = append(deps, resolved...)
		}

	case KindResource, KindDataSource:
		if v.ResourceCfg != nil && v.Schema != nil {
			refs, _ := lang.ReferencesInBlock(addrs.ParseRef, v.ResourceCfg.Config, v.Schema)
			resolved := g.resolveRefs(refs, v.Module)
			es.HCLRefs += len(resolved)
			deps = append(deps, resolved...)
		}
		if v.ResourceCfg != nil {
			for _, trav := range v.ResourceCfg.DependsOn {
				ref, _ := addrs.ParseRef(trav)
				if ref != nil {
					resolved := g.resolveRef(ref, v.Module)
					es.DependsOn += len(resolved)
					deps = append(deps, resolved...)
				}
			}
		}
		provKey := providerLookupKey(v.ProviderAddr, v.ProviderAlias)
		if pid, ok := g.Providers[provKey]; ok {
			deps = appendUnique(deps, pid)
			es.Provider++
		}

	case KindProviderConfig:
		if v.ProviderBody != nil && v.Schema != nil {
			refs, _ := lang.ReferencesInBlock(addrs.ParseRef, v.ProviderBody, v.Schema)
			resolved := g.resolveRefs(refs, v.Module)
			es.HCLRefs += len(resolved)
			deps = resolved
		}

	case KindModuleExpand:
		parentModule := v.Module[:len(v.Module)-1]
		for _, expr := range v.ModuleCallExprs {
			refs, _ := lang.ReferencesInExpr(addrs.ParseRef, expr)
			resolved := g.resolveRefs(refs, parentModule)
			es.ExpandRefs += len(resolved)
			deps = append(deps, resolved...)
		}
		for _, trav := range v.ModuleDependsOn {
			ref, _ := addrs.ParseRef(trav)
			if ref != nil {
				resolved := g.resolveRef(ref, parentModule)
				es.DependsOn += len(resolved)
				deps = append(deps, resolved...)
			}
		}
		if len(parentModule) > 0 {
			if eid, ok := g.Expands[parentModule.String()]; ok {
				deps = appendUnique(deps, eid)
				es.ParentExp++
			}
		}

	case KindModuleClose:
		children := moduleExternals[v.Module.String()]
		es.CloseGate += len(children)
		deps = append(deps, children...)
	}

	return deps
}

// AttachSchemas populates the Schema field on resource, data source,
// and provider config vertices. Must be called before edge creation
// for resources (since ReferencesInBlock needs a schema).
func (g *Graph) AttachSchemas(schemas map[addrs.Provider]providers.ProviderSchema) {
	for i := range g.Verts {
		v := &g.Verts[i]
		switch v.Kind {
		case KindResource, KindDataSource:
			if v.ResourceCfg == nil {
				continue
			}
			ps, ok := schemas[v.ProviderAddr]
			if !ok {
				continue
			}
			s, _ := ps.SchemaForResourceAddr(v.ResourceCfg.Addr())
			if s != nil {
				v.Schema = s.Block
			}
		case KindProviderConfig:
			ps, ok := schemas[v.ProviderAddr]
			if !ok {
				continue
			}
			v.Schema = ps.Provider.Block
		}
	}
}

// resolveRefs converts a list of parsed references into vertex IDs.
func (g *Graph) resolveRefs(refs []*addrs.Reference, fromModule addrs.ModuleInstance) []ID {
	var ids []ID
	for _, ref := range refs {
		ids = append(ids, g.resolveRef(ref, fromModule)...)
	}
	return ids
}

// resolveRef maps a single reference to the vertex IDs it points to.
func (g *Graph) resolveRef(ref *addrs.Reference, fromModule addrs.ModuleInstance) []ID {
	switch subj := ref.Subject.(type) {
	case addrs.InputVariable:
		key := addrKey{unique.Make(fromModule.String()), unique.Make(subj.Name)}
		if id, ok := g.Vars[key]; ok {
			return []ID{id}
		}

	case addrs.LocalValue:
		key := addrKey{unique.Make(fromModule.String()), unique.Make(subj.Name)}
		if id, ok := g.Locals[key]; ok {
			return []ID{id}
		}

	case addrs.OutputValue:
		key := addrKey{unique.Make(fromModule.String()), unique.Make(subj.Name)}
		if id, ok := g.Outputs[key]; ok {
			return []ID{id}
		}

	case addrs.Resource:
		absRes := subj.Absolute(fromModule)
		if ids, ok := g.ResInst[absRes.String()]; ok {
			return ids
		}

	case addrs.ResourceInstance:
		absRes := subj.Resource.Absolute(fromModule)
		if ids, ok := g.ResInst[absRes.String()]; ok {
			return ids
		}

	case addrs.ModuleCall:
		return g.childModuleDeps(fromModule, subj.Name)

	case addrs.ModuleCallInstance:
		return g.childModuleDeps(fromModule, subj.Call.Name)

	case addrs.ModuleCallInstanceOutput:
		childAddr := fromModule.Child(subj.Call.Call.Name, subj.Call.Key)
		key := addrKey{unique.Make(childAddr.String()), unique.Make(subj.Name)}
		if id, ok := g.Outputs[key]; ok {
			return []ID{id}
		}
		// Singleton fallback: if the module was expanded as a singleton
		// (count/for_each depended on a runtime value), the output is
		// registered with NoKey but the reference uses [0] or a string key.
		if subj.Call.Key != addrs.NoKey {
			fallbackAddr := fromModule.Child(subj.Call.Call.Name, addrs.NoKey)
			fallbackKey := addrKey{unique.Make(fallbackAddr.String()), unique.Make(subj.Name)}
			if id, ok := g.Outputs[fallbackKey]; ok {
				return []ID{id}
			}
		}

	case addrs.CountAttr, addrs.ForEachAttr, addrs.PathAttr, addrs.TerraformAttr:
		return nil

	case addrs.ProviderFunction:
		return nil
	}

	return nil
}

// childModuleDeps returns dependency IDs for a reference to module.X.
// Prefers close vertices (which depend on all child content). Falls back
// to output vertices for modules without expand/close (e.g. the root).
func (g *Graph) childModuleDeps(parent addrs.ModuleInstance, callName string) []ID {
	key := addrKey{unique.Make(parent.String()), unique.Make(callName)}
	if ids := g.ChildCloses[key]; len(ids) > 0 {
		return ids
	}
	return g.ChildOutputs[key]
}

// checkCycles runs Kahn's algorithm without executing anything. If not
// all vertices are visited, there is a cycle.
func (g *Graph) checkCycles() error {
	n := len(g.Verts)
	inDeg := make([]int, n)
	for i, deps := range g.Deps {
		inDeg[i] = len(deps)
	}

	var queue []int
	for i, deg := range inDeg {
		if deg == 0 {
			queue = append(queue, i)
		}
	}

	visited := 0
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		visited++
		for _, succ := range g.RevDeps[ID(v)] {
			inDeg[succ]--
			if inDeg[succ] == 0 {
				queue = append(queue, int(succ))
			}
		}
	}

	if visited < n {
		return fmt.Errorf("chofu/graph: dependency cycle detected (%d of %d vertices reachable)", visited, n)
	}
	return nil
}

// FilterTargets extracts the subgraph relevant to the given targets.
// Returns the original graph unchanged if the filter is inactive.
//
// The algorithm:
//  1. Mark vertices directly addressed by a target.
//  2. Include all transitive dependencies (ancestors) of targeted vertices.
//  3. Include outputs whose resource ancestors are all within the targeted set.
//  4. Build a new graph with dense ID remapping.
func FilterTargets(g *Graph, f *TargetFilter) *Graph {
	if !f.Active() {
		return g
	}

	n := len(g.Verts)
	keep := make([]bool, n)

	// Step 1: Mark directly targeted vertices.
	for i := range g.Verts {
		if f.VertexTargeted(&g.Verts[i]) {
			keep[i] = true
		}
	}

	// Step 2: Transitively include all dependencies of targeted vertices.
	changed := true
	for changed {
		changed = false
		for i := range g.Verts {
			if !keep[i] {
				continue
			}
			for _, dep := range g.Deps[i] {
				if !keep[dep] {
					keep[dep] = true
					changed = true
				}
			}
		}
	}

	// Step 3: Include outputs whose resource ancestors are all targeted.
	for i := range g.Verts {
		if keep[i] || g.Verts[i].Kind != KindOutput {
			continue
		}
		allTargeted := true
		hasResource := false
		for _, dep := range g.Deps[i] {
			switch g.Verts[dep].Kind {
			case KindResource, KindDataSource:
				hasResource = true
				if !keep[dep] {
					allTargeted = false
				}
			}
		}
		if hasResource && allTargeted {
			keep[i] = true
		}
	}

	// Step 4: Build the new graph with dense ID remapping.
	ng := remapGraph(g, keep)

	log.Printf("[INFO] chofu/graph: target filter: %d → %d vertices", n, len(ng.Verts))
	return ng
}

// CompactGraph removes pass-through vertices that don't require walk-time
// computation, producing a denser graph with fewer vertices and edges.
//
// Removed vertex kinds:
//   - Cached locals (LocalVal != NilVal): values known at expansion time,
//     stored in InlinedLocals for GetLocalValue to read.
//   - Close gates: structural exit points that just set EmptyObjectVal.
//     Dependents are rewired to depend on the close gate's children directly.
//
// Variables are NOT removed because expand vertices write to their result
// slots during the walk (re-evaluating module call arguments).
func CompactGraph(g *Graph) *Graph {
	n := len(g.Verts)
	if n == 0 {
		return g
	}

	remove := make([]bool, n)
	inlinedLocals := make(map[addrKey]cty.Value)
	var removedCount int

	for i := range g.Verts {
		v := &g.Verts[i]
		switch {
		case v.Kind == KindLocal && v.LocalVal != cty.NilVal:
			remove[i] = true
			removedCount++
			key := addrKey{unique.Make(v.Module.String()), unique.Make(v.Name)}
			inlinedLocals[key] = v.LocalVal

		case v.Kind == KindModuleClose:
			remove[i] = true
			removedCount++
		}
	}

	if removedCount == 0 {
		return g
	}

	// For close gates, rewire: any vertex depending on a close gate
	// instead depends on the close gate's dependencies (the child content).
	closeDeps := make(map[ID][]ID)
	for i := range g.Verts {
		if remove[i] && g.Verts[i].Kind == KindModuleClose {
			closeDeps[ID(i)] = g.Deps[i]
		}
	}

	// Build ID remapping.
	oldToNew := make([]ID, n)
	for i := range oldToNew {
		oldToNew[i] = -1
	}

	newVerts := make([]Vertex, 0, n-removedCount)
	for i := range g.Verts {
		if !remove[i] {
			oldToNew[i] = ID(len(newVerts))
			newVerts = append(newVerts, g.Verts[i])
		}
	}

	// Rebuild edges with close gate rewiring.
	newDeps := make([][]ID, len(newVerts))
	for oldID := range g.Verts {
		newID := oldToNew[oldID]
		if newID < 0 {
			continue
		}
		var deps []ID
		for _, oldDep := range g.Deps[oldID] {
			if !remove[oldDep] {
				deps = append(deps, oldToNew[oldDep])
			} else if g.Verts[oldDep].Kind == KindModuleClose {
				// Close gate removed — inherit its deps (rewire).
				for _, transitive := range closeDeps[oldDep] {
					if newDep := oldToNew[transitive]; newDep >= 0 {
						deps = append(deps, newDep)
					}
				}
			}
			// Cached local deps are simply dropped — value is pre-resolved.
		}
		newDeps[newID] = deps
	}

	ng := &Graph{
		Verts:         newVerts,
		Deps:          newDeps,
		InlinedLocals: inlinedLocals,
	}
	ng.initMaps(len(newVerts))
	ng.rebuildIndexes()
	ng.buildReverseEdges()

	log.Printf("[INFO] chofu/graph: compact: %d → %d vertices (%d cached locals inlined, %d close gates removed)",
		n, len(newVerts), len(inlinedLocals), removedCount-len(inlinedLocals))
	return ng
}

// remapGraph builds a new graph containing only vertices where keep[i] is true.
// Edges are remapped to the new dense IDs; edges to removed vertices are dropped.
func remapGraph(g *Graph, keep []bool) *Graph {
	n := len(g.Verts)
	oldToNew := make([]ID, n)
	for i := range oldToNew {
		oldToNew[i] = -1
	}

	var newVerts []Vertex
	for i := range g.Verts {
		if keep[i] {
			oldToNew[i] = ID(len(newVerts))
			newVerts = append(newVerts, g.Verts[i])
		}
	}

	ng := &Graph{
		Verts: newVerts,
		Deps:  make([][]ID, len(newVerts)),
	}
	ng.initMaps(len(newVerts))

	for oldID := range g.Verts {
		newID := oldToNew[oldID]
		if newID < 0 {
			continue
		}
		for _, oldDep := range g.Deps[oldID] {
			if newDep := oldToNew[oldDep]; newDep >= 0 {
				ng.Deps[newID] = append(ng.Deps[newID], newDep)
			}
		}
	}

	ng.buildReverseEdges()
	ng.rebuildIndexes()

	return ng
}

// providerLookupKey creates a stable key for provider config lookup.
func providerLookupKey(provider addrs.Provider, alias string) string {
	if alias == "" {
		return provider.String()
	}
	return provider.String() + "." + alias
}

// appendUnique appends id to slice if not already present.
func appendUnique(ids []ID, id ID) []ID {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}
