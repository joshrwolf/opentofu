// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"log"
	"sync"
	"time"
	"unique"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/instances"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
	"github.com/opentofu/opentofu/internal/tracing/traceattrs"
)

// buildPipeline decomposes the build into independent per-module pipelines.
// Each root-level module call gets its own expand → graph → walk cycle,
// running in a bounded worker pool. Root-scope content (vars, locals,
// providers) is evaluated once and shared across all pipelines.
func (bc *buildContext) buildPipeline(
	ctx context.Context,
	rootVars map[string]cty.Value,
	diags tfdiags.Diagnostics,
) (totalVerts int, expandDur, graphDur, walkDur time.Duration, outDiags tfdiags.Diagnostics) {
	outDiags = diags
	log.Printf("[INFO] chofu: using per-module pipeline (%d independent calls)", len(bc.config.Module.ModuleCalls))

	// Phase 1: Root expansion + walk (providers, vars, locals).
	bc.ui.Event(BuildEvent{PhaseStart: &PhaseStartEvent{Phase: "expand"}})
	expandStart := time.Now()

	e := bc.newExpCtx(nil) // no expander needed for root-only
	rootVerts, rootLocalVals, rootExpandDiags := e.expandRootOnly(ctx, rootVars)
	outDiags = outDiags.Append(rootExpandDiags)

	rootGraph, _, rootGraphDiags := BuildGraph(rootVerts, bc.pc.schemas)
	outDiags = outDiags.Append(rootGraphDiags)
	if rootGraphDiags.HasErrors() {
		return 0, 0, 0, 0, outDiags
	}

	rootResults := NewResults(len(rootGraph.Verts))
	for name, val := range rootVars {
		key := addrKey{unique.Make(addrs.RootModuleInstance.String()), unique.Make(name)}
		if id, ok := rootGraph.Vars[key]; ok {
			rootResults.Values[id] = val
		}
	}

	rootWC := bc.newWalkContext(rootGraph, rootResults)
	outDiags = outDiags.Append(Walk(ctx, rootGraph, rootResults, 1, rootWC.executeVertex))

	expandDur = time.Since(expandStart)
	bc.ui.Event(BuildEvent{PhaseComplete: &PhaseCompleteEvent{Phase: "expand", Duration: expandDur, ItemCount: len(rootVerts)}})

	// Phase 2: Per-module pipelines.
	bc.ui.Event(BuildEvent{PhaseStart: &PhaseStartEvent{Phase: "walk"}})
	walkStart := time.Now()

	e = bc.newExpCtx(instances.NewExpander())

	type moduleCallEntry struct {
		name string
		mc   *configs.ModuleCall
	}
	var calls []moduleCallEntry
	rootModule := addrs.RootModuleInstance.Module()
	for name, mc := range bc.config.Module.ModuleCalls {
		if !bc.filter.ModuleRelevant(rootModule, name) {
			continue
		}
		calls = append(calls, moduleCallEntry{name, mc})
	}

	pipelineWorkers := max(1, min(bc.workers, len(calls)))
	work := make(chan moduleCallEntry, len(calls))
	for _, entry := range calls {
		work <- entry
	}
	close(work)

	var diagsMu sync.Mutex
	var wg sync.WaitGroup
	for range pipelineWorkers {
		wg.Go(func() {
			for entry := range work {
				pipeDiags := bc.runModulePipeline(
					ctx, e, entry.name, entry.mc,
					rootVerts, rootResults, rootVars, rootLocalVals,
				)
				if len(pipeDiags) > 0 {
					diagsMu.Lock()
					outDiags = outDiags.Append(pipeDiags)
					diagsMu.Unlock()
				}
			}
		})
	}
	wg.Wait()

	walkDur = time.Since(walkStart)
	bc.ui.Event(BuildEvent{PhaseComplete: &PhaseCompleteEvent{Phase: "walk", Duration: walkDur}})

	return int(bc.stats.vertices.Load()), expandDur, 0, walkDur, outDiags
}

// buildGlobal runs the original three-phase pipeline: expand all → build
// one graph → walk it with bounded concurrency.
func (bc *buildContext) buildGlobal(
	ctx context.Context,
	rootVars map[string]cty.Value,
	diags tfdiags.Diagnostics,
) (totalVerts int, expandDur, graphDur, walkDur time.Duration, outDiags tfdiags.Diagnostics) {
	outDiags = diags
	log.Printf("[INFO] chofu: using global pipeline")

	expander := instances.NewExpander()

	// Phase 1: Structural expansion.
	bc.ui.Event(BuildEvent{PhaseStart: &PhaseStartEvent{Phase: "expand"}})
	expandStart := time.Now()
	_, expandSpan := tracing.Tracer().Start(ctx, "chofu.Expand")
	items, expandDiags := Expand(ctx, bc.config, bc.pc.schemas, expander, bc.filter, rootVars, bc.workspace, bc.workDir)
	expandSpan.SetAttributes(
		traceattrs.Int64("chofu.items", int64(len(items))),
		traceattrs.Int64("chofu.expand_errors", int64(countErrors(expandDiags))),
	)
	expandSpan.End()
	expandDur = time.Since(expandStart)
	bc.ui.Event(BuildEvent{PhaseComplete: &PhaseCompleteEvent{Phase: "expand", Duration: expandDur, ItemCount: len(items)}})
	outDiags = outDiags.Append(expandDiags)

	var kindCounts [8]int64
	for _, item := range items {
		if int(item.Kind) < len(kindCounts) {
			kindCounts[item.Kind]++
		}
	}
	log.Printf("[INFO] chofu: expand produced %d items (vars=%d locals=%d outputs=%d resources=%d data=%d providers=%d)",
		len(items), kindCounts[KindVariable], kindCounts[KindLocal], kindCounts[KindOutput],
		kindCounts[KindResource], kindCounts[KindDataSource], kindCounts[KindProviderConfig])

	// Phase 2: DAG construction.
	bc.ui.Event(BuildEvent{PhaseStart: &PhaseStartEvent{Phase: "graph"}})
	graphStart := time.Now()
	_, graphSpan := tracing.Tracer().Start(ctx, "chofu.BuildGraph")
	graph, _, graphDiags := BuildGraph(items, bc.pc.schemas)
	outDiags = outDiags.Append(graphDiags)
	if graphDiags.HasErrors() {
		graphSpan.End()
		return 0, expandDur, 0, 0, outDiags
	}
	graph = FilterTargets(graph, bc.filter)
	graph = CompactGraph(graph)
	graphSpan.End()
	graphDur = time.Since(graphStart)
	totalVerts = len(graph.Verts)
	bc.ui.Event(BuildEvent{PhaseComplete: &PhaseCompleteEvent{Phase: "graph", Duration: graphDur, Vertices: totalVerts}})

	// Phase 3: Walk.
	bc.ui.Event(BuildEvent{PhaseStart: &PhaseStartEvent{Phase: "walk"}})
	walkStart := time.Now()
	walkCtx, walkSpan := tracing.Tracer().Start(ctx, "chofu.Walk")
	results := NewResults(len(graph.Verts))

	for name, val := range rootVars {
		key := addrKey{unique.Make(addrs.RootModuleInstance.String()), unique.Make(name)}
		if id, ok := graph.Vars[key]; ok {
			results.Values[id] = val
		}
	}

	wc := bc.newWalkContext(graph, results)
	outDiags = outDiags.Append(Walk(walkCtx, graph, results, bc.workers, wc.executeVertex))
	walkSpan.End()
	walkDur = time.Since(walkStart)
	bc.ui.Event(BuildEvent{PhaseComplete: &PhaseCompleteEvent{Phase: "walk", Duration: walkDur}})

	return totalVerts, expandDur, graphDur, walkDur, outDiags
}

// runModulePipeline executes expand → graph → walk for a single root-level
// module call. Each pipeline produces its own mini-graph and results array
// but shares the buildContext's providerCache, hashIndex, config, and stats.
func (bc *buildContext) runModulePipeline(
	ctx context.Context,
	e *expCtx,
	callName string,
	mc *configs.ModuleCall,
	rootVerts []Vertex,
	rootResults *Results,
	rootVarValues map[string]cty.Value,
	rootLocalVals map[string]cty.Value,
) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	// Expand this module call's children from the root scope.
	childVerts, expandDiags := e.expandModuleCall(
		ctx, bc.config, addrs.RootModuleInstance, callName, mc,
		instances.RepetitionData{}, rootVarValues, rootLocalVals,
	)
	diags = diags.Append(expandDiags)
	if len(childVerts) == 0 {
		return diags
	}

	// Build the mini-graph: root vertices (vars, locals, providers) +
	// child vertices. Root vertices occupy indices 0..len(rootVerts)-1.
	nRoot := len(rootVerts)
	allVerts := make([]Vertex, nRoot+len(childVerts))
	copy(allVerts, rootVerts)
	copy(allVerts[nRoot:], childVerts)

	miniGraph, _, graphDiags := BuildGraph(allVerts, bc.pc.schemas)
	diags = diags.Append(graphDiags)
	if graphDiags.HasErrors() {
		return diags
	}

	miniGraph = FilterTargets(miniGraph, bc.filter)

	results := NewResults(len(miniGraph.Verts))

	// Seed root vertex values from the root walk results. After target
	// filtering, root vertices may have been remapped to new IDs, so
	// we look them up by address rather than assuming positional indices.
	for i, rv := range rootVerts {
		srcVal := rootResults.Values[ID(i)]
		if srcVal == cty.NilVal {
			continue
		}
		switch rv.Kind {
		case KindVariable:
			key := addrKey{unique.Make(rv.Module.String()), unique.Make(rv.Name)}
			if id, ok := miniGraph.Vars[key]; ok {
				results.Values[id] = srcVal
			}
		case KindLocal:
			key := addrKey{unique.Make(rv.Module.String()), unique.Make(rv.Name)}
			if id, ok := miniGraph.Locals[key]; ok {
				results.Values[id] = srcVal
			}
		case KindProviderConfig:
			provKey := providerLookupKey(rv.ProviderAddr, rv.ProviderAlias)
			if id, ok := miniGraph.Providers[provKey]; ok {
				results.Values[id] = srcVal
			}
		}
	}

	bc.stats.vertices.Add(int64(len(miniGraph.Verts)))

	wc := bc.newWalkContext(miniGraph, results)
	diags = diags.Append(Walk(ctx, miniGraph, results, 1, wc.executeVertex))

	return diags
}

// isIndependentCall checks whether a root-level module call's arguments
// reference only root-scope values (variables, locals, path/terraform attrs).
// Returns false if any expression references a resource, module call,
// module output, or output — which would require cross-module evaluation.
func isIndependentCall(mc *configs.ModuleCall) bool {
	var exprs []hcl.Expression

	// Collect all expressions from the module call body (input variables).
	if mc.Config != nil {
		attrs, _ := mc.Config.JustAttributes()
		for _, attr := range attrs {
			exprs = append(exprs, attr.Expr)
		}
	}

	// Meta-argument expressions.
	if mc.ForEach != nil {
		exprs = append(exprs, mc.ForEach)
	}
	if mc.Count != nil {
		exprs = append(exprs, mc.Count)
	}
	if mc.Enabled != nil {
		exprs = append(exprs, mc.Enabled)
	}

	for _, expr := range exprs {
		refs, _ := lang.ReferencesInExpr(addrs.ParseRef, expr)
		for _, ref := range refs {
			switch ref.Subject.(type) {
			case addrs.Resource, addrs.ResourceInstance,
				addrs.ModuleCall, addrs.ModuleCallInstance,
				addrs.ModuleCallInstanceOutput, addrs.OutputValue:
				return false
			}
		}
	}

	for _, trav := range mc.DependsOn {
		ref, _ := addrs.ParseRef(trav)
		if ref == nil {
			continue
		}
		switch ref.Subject.(type) {
		case addrs.Resource, addrs.ResourceInstance,
			addrs.ModuleCall, addrs.ModuleCallInstance,
			addrs.ModuleCallInstanceOutput, addrs.OutputValue:
			return false
		}
	}

	return true
}
