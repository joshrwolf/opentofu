// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/engine"
	"github.com/opentofu/opentofu/internal/build/selector"
	"github.com/opentofu/opentofu/internal/build/solve"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
	"github.com/zclconf/go-cty/cty"
)

type OutputValue struct {
	Addr  catalog.Addr
	Value cty.Value
}

type Result struct {
	Outputs []OutputValue
}

type executor struct {
	solver  *solve.Solver
	catalog *catalog.Catalog
	engine  *engine.Engine
	runner  *Runner
	logf    func(string, ...any)
}

func (e *executor) Execute(ctx context.Context, selectors selector.Set) (Result, tfdiags.Diagnostics) {
	ctx, span := tracing.Tracer().Start(ctx, "build.Execute")
	defer span.End()

	var (
		wg          sync.WaitGroup
		diags       diagCollector
		targetSeen  sync.Map
		outputMu    sync.Mutex
		outputSeen  = map[catalog.AddrKey]struct{}{}
		outputAddrs []catalog.Addr
		targetCount atomic.Int64
	)

	sem := make(chan struct{}, runtime.GOMAXPROCS(0)*4)

	launch := func(fn func()) {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			fn()
		})
	}

	selectionDiags := StreamSelections(ctx, e.catalog, e.solver, selectors, func(addr catalog.Addr) bool {
		switch addr.Kind {
		case catalog.TargetKindModule:
			launch(func() {
				e.discoverModuleTargets(ctx, addr, &diags, func(a catalog.Addr) {
					if _, loaded := targetSeen.LoadOrStore(a.Identity(), struct{}{}); loaded {
						return
					}
					targetCount.Add(1)
					launch(func() {
						_, d := e.solver.TargetValue(ctx, a)
						diags.append(d)
					})
				})
			})
		case catalog.TargetKindOutput:
			outputMu.Lock()
			key := addr.Identity()
			if _, ok := outputSeen[key]; !ok {
				outputSeen[key] = struct{}{}
				outputAddrs = append(outputAddrs, addr)
			}
			outputMu.Unlock()
			launch(func() {
				_, d := e.solver.OutputValue(ctx, addr)
				diags.append(d)
			})
		case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
			if _, loaded := targetSeen.LoadOrStore(addr.Identity(), struct{}{}); loaded {
				return ctx.Err() == nil
			}
			targetCount.Add(1)
			launch(func() {
				_, d := e.solver.TargetValue(ctx, addr)
				diags.append(d)
			})
		}
		return ctx.Err() == nil
	})
	diags.append(selectionDiags)

	wg.Wait()

	result := diags.result()
	result = result.Append(e.runner.Warnings())

	var ret Result
	if !result.HasErrors() {
		var outputDiags tfdiags.Diagnostics
		ret.Outputs, outputDiags = e.collectOutputs(ctx, outputAddrs)
		result = result.Append(outputDiags)
	}

	span.SetAttributes(
		attribute.Int("build.selected_targets", int(targetCount.Load())),
		attribute.Int("build.selected_outputs", len(outputAddrs)),
	)
	for _, qs := range e.solver.Stats() {
		span.SetAttributes(
			attribute.Int(qs.Name+"entries", qs.Entries),
			attribute.Int64(qs.Name+"gets", qs.Gets()),
			attribute.Int64(qs.Name+"fast_path", qs.FastPath),
			attribute.Int64(qs.Name+"computed", qs.Computed),
			attribute.Int64(qs.Name+"coalesced", qs.Coalesced),
		)
	}
	tracing.SetSpanError(span, result)
	return ret, result
}

func (e *executor) discoverModuleTargets(ctx context.Context, addr catalog.Addr, diags *diagCollector, launch func(catalog.Addr)) {
	pkg, ok := e.catalog.Package(addr.Module.Declaration())
	if !ok {
		return
	}
	for _, target := range pkg.Targets {
		if !target.Addr.Actionable() {
			continue
		}
		concreteDecl := target.Addr
		concreteDecl.Module = addr.Module
		concreteDecl.Key = catalog.NoKey()

		instances, instanceDiags := e.solver.TargetInstances(ctx, concreteDecl)
		diags.append(instanceDiags)
		if instanceDiags.HasErrors() {
			continue
		}
		for _, instance := range instances.Addrs {
			launch(instance)
		}
	}
}

func (e *executor) collectOutputs(ctx context.Context, addrs []catalog.Addr) ([]OutputValue, tfdiags.Diagnostics) {
	if len(addrs) == 0 {
		return nil, nil
	}
	var diags tfdiags.Diagnostics
	outputs := make([]OutputValue, 0, len(addrs))
	for _, addr := range addrs {
		eval, outputDiags := e.solver.OutputValue(ctx, addr)
		diags = diags.Append(outputDiags)
		if outputDiags.HasErrors() {
			continue
		}
		if !eval.Known || eval.Value == cty.NilVal {
			continue
		}
		outputs = append(outputs, OutputValue{Addr: addr, Value: eval.Value})
	}
	return outputs, diags
}

type diagCollector struct {
	mu    sync.Mutex
	diags tfdiags.Diagnostics
}

func (c *diagCollector) append(d tfdiags.Diagnostics) {
	if len(d) == 0 {
		return
	}
	c.mu.Lock()
	c.diags = c.diags.Append(d)
	c.mu.Unlock()
}

func (c *diagCollector) result() tfdiags.Diagnostics {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.diags
}

