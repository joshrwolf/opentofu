// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package bench is the shared harness for synthetic build benchmarks and
// txtar-driven end-to-end tests. Runner.Run is the single entry point; the
// bench-specific scaffolding (topologies, generators, exports, report helpers)
// lives in bench_test.go, and the e2e scaffolding lives in txtar_test.go.
package bench

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build"
	"github.com/opentofu/opentofu/internal/build/catalog"
	buildhcl "github.com/opentofu/opentofu/internal/build/hcl"
	"github.com/opentofu/opentofu/internal/build/dice"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/build/selector"
	"github.com/opentofu/opentofu/internal/build/solve"
	commandtesting "github.com/opentofu/opentofu/internal/command/testing"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type Options struct {
	ProviderFactories map[addrs.Provider]providers.Factory
	ProviderInvoker   *buildprovider.Invoker
}

type Runner struct {
	options Options
}

func NewRunner(options Options) *Runner {
	return &Runner{options: options}
}

type Scenario struct {
	Name        string
	Selectors   []string
	InspectOnly bool
}

type Result struct {
	Scenario            string
	RootDir             string
	Selectors           []string
	Start               time.Time
	Total               time.Duration
	Spans               tracetest.SpanStubs
	Outputs             map[string]string
	Errors              []string
	Warnings            []string
	Diagnostics         int
	ErrorDiagnostics    int
	WarningDiagnostics  int
	DiagnosticSummaries map[string]int
	DiceStats           []dice.Stats
	HeapStats           HeapStats
}

type HeapStats struct {
	HeapAllocBytes uint64
	HeapObjects    uint64
	TotalAlloc     uint64
	Mallocs        uint64
	GCCycles       uint32
	GCPauseNs      uint64
}

func DefaultProviderFactories() map[addrs.Provider]providers.Factory {
	return map[addrs.Provider]providers.Factory{
		addrs.NewBuiltInProvider("test"): func() (providers.Interface, error) { return commandtesting.NewProvider(nil).Provider, nil },
		addrs.NewDefaultProvider("test"): func() (providers.Interface, error) { return commandtesting.NewProvider(nil).Provider, nil },
	}
}

func DefaultProviderInvoker() *buildprovider.Invoker {
	return buildprovider.NewInvoker(buildprovider.InvokerConfig{
		OCIGetResolver: syntheticOCIGetResolver{},
	})
}

type syntheticOCIGetResolver struct{}

func (syntheticOCIGetResolver) Resolve(_ context.Context, ref string) (string, error) {
	return "sha256:synthetic-" + ref, nil
}

func (r *Runner) Run(ctx context.Context, rootDir string, scenario Scenario) (Result, error) {
	if rootDir == "" {
		return Result{}, fmt.Errorf("root dir must not be empty")
	}

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	defer otel.SetTracerProvider(prev)

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	start := time.Now()
	var result Result
	var err error
	if scenario.InspectOnly {
		result, err = r.runInspector(ctx, rootDir, scenario)
	} else {
		result, err = r.runEngine(ctx, rootDir, scenario)
	}
	result.Start = start
	result.Total = time.Since(start)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	result.HeapStats = HeapStats{
		HeapAllocBytes: after.HeapAlloc,
		HeapObjects:    after.HeapObjects,
		TotalAlloc:     after.TotalAlloc - before.TotalAlloc,
		Mallocs:        after.Mallocs - before.Mallocs,
		GCCycles:       after.NumGC - before.NumGC,
		GCPauseNs:      after.PauseTotalNs - before.PauseTotalNs,
	}

	_ = tp.ForceFlush(ctx)
	result.Spans = exporter.GetSpans()

	return result, err
}

func (r *Runner) providerFactories() map[addrs.Provider]providers.Factory {
	if r.options.ProviderFactories != nil {
		return r.options.ProviderFactories
	}
	return DefaultProviderFactories()
}

func (r *Runner) providerInvoker() *buildprovider.Invoker {
	if r.options.ProviderInvoker != nil {
		return r.options.ProviderInvoker
	}
	return DefaultProviderInvoker()
}

func (r *Runner) runEngine(ctx context.Context, rootDir string, scenario Scenario) (Result, error) {
	result := Result{
		Scenario:            scenario.Name,
		RootDir:             rootDir,
		Selectors:           slices.Clone(scenario.Selectors),
		DiagnosticSummaries: map[string]int{},
	}
	diagCounter := &diagnosticCounter{summaries: result.DiagnosticSummaries}

	loader, err := configload.NewLoader(&configload.Config{
		ModulesDir: filepath.Join(rootDir, ".terraform", "modules"),
	})
	if err != nil {
		return Result{}, err
	}

	rootCall := staticRootCall(rootDir)
	selectors, selectorDiags := build.ParseSelectors(scenario.Selectors)
	diagCounter.Append(selectorDiags)
	if selectorDiags.HasErrors() {
		diagCounter.applyTo(&result)
		return result, selectorDiags.Err()
	}

	loaded, loadDiags := build.Load(ctx, loader, buildhcl.LoadRequest{RootDir: rootDir}, rootCall)
	diagCounter.Append(loadDiags)
	if loaded == nil {
		diagCounter.applyTo(&result)
		return result, loadDiags.Err()
	}

	engine, engineDiags := build.NewEngine(ctx, loaded, build.EngineConfig{
		ProviderFactories: r.providerFactories(),
		ProviderInvoker:   r.providerInvoker(),
	})
	diagCounter.Append(engineDiags)
	if engine == nil {
		diagCounter.applyTo(&result)
		return result, engineDiags.Err()
	}
	defer func() { diagCounter.Append(engine.Close(ctx)) }()

	buildResult, executeDiags := engine.Execute(ctx, selectors)
	diagCounter.Append(executeDiags)

	if !diagCounter.hasErrors() {
		result.Outputs = formatOutputValues(buildResult.Outputs)
	}

	result.DiceStats = engine.Stats()
	diagCounter.applyTo(&result)
	result.Errors = diagCounter.errorDetails()
	result.Warnings = diagCounter.warningDetails()
	return result, nil
}

func (r *Runner) runInspector(ctx context.Context, rootDir string, scenario Scenario) (Result, error) {
	result := Result{
		Scenario:            scenario.Name,
		RootDir:             rootDir,
		Selectors:           slices.Clone(scenario.Selectors),
		DiagnosticSummaries: map[string]int{},
	}
	diagCounter := &diagnosticCounter{summaries: result.DiagnosticSummaries}

	loader, err := configload.NewLoader(&configload.Config{
		ModulesDir: filepath.Join(rootDir, ".terraform", "modules"),
	})
	if err != nil {
		return Result{}, err
	}

	rootCall := staticRootCall(rootDir)
	selectors, selectorDiags := build.ParseSelectors(scenario.Selectors)
	diagCounter.Append(selectorDiags)
	if selectorDiags.HasErrors() {
		diagCounter.applyTo(&result)
		return result, selectorDiags.Err()
	}

	loaded, loadDiags := build.Load(ctx, loader, buildhcl.LoadRequest{RootDir: rootDir}, rootCall)
	diagCounter.Append(loadDiags)
	if loaded == nil {
		diagCounter.applyTo(&result)
		return result, loadDiags.Err()
	}

	inspector, inspectorDiags := build.NewInspector(ctx, loaded, build.InspectorConfig{
		ProviderFactories: r.providerFactories(),
	})
	diagCounter.Append(inspectorDiags)
	if inspector == nil {
		diagCounter.applyTo(&result)
		return result, inspectorDiags.Err()
	}
	defer func() { diagCounter.Append(inspector.Close(ctx)) }()

	inspectSelections(ctx, inspector.Catalog(), inspector.Solver(), selectors)

	result.DiceStats = inspector.Stats()
	diagCounter.applyTo(&result)
	result.Errors = diagCounter.errorDetails()
	result.Warnings = diagCounter.warningDetails()
	return result, nil
}

func inspectSelections(ctx context.Context, cat *catalog.Catalog, solver *solve.Solver, selectors selector.Set) {
	build.StreamCandidateMatches(cat, selectors, func(addr catalog.Addr) bool {
		inspectCandidate(ctx, solver, selectors, addr)
		return ctx.Err() == nil
	})
}

func inspectCandidate(ctx context.Context, solver *solve.Solver, selectors selector.Set, addr catalog.Addr) {
	modules := expandModules(ctx, solver, addr.Module)

	switch addr.Kind {
	case catalog.TargetKindModule:
	case catalog.TargetKindOutput:
		for _, module := range modules {
			concrete := addr
			concrete.Module = module
			if !selectors.Match(concrete).Exact() {
				continue
			}
			solver.OutputEval(ctx, concrete)
		}
	case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
		for _, module := range modules {
			concreteDecl := addr
			concreteDecl.Module = module
			concreteDecl.Key = catalog.NoKey()

			instances, instanceDiags := solver.TargetInstances(ctx, concreteDecl)
			if instanceDiags.HasErrors() {
				continue
			}
			for _, instance := range instances.Addrs {
				if !selectors.Match(instance).Exact() {
					continue
				}
				solver.ActionSpec(ctx, instance)
			}
		}
	}
}

func expandModules(ctx context.Context, solver *solve.Solver, module catalog.ModulePath) []catalog.ModulePath {
	if module.Len() == 0 {
		return []catalog.ModulePath{catalog.RootModule()}
	}
	packages, diags := solver.PackageInstances(ctx, module)
	if diags.HasErrors() {
		return nil
	}
	return packages.Modules
}

func staticRootCall(rootDir string) configs.StaticModuleCall {
	return configs.NewStaticModuleCall(addrs.RootModule, hcl.Range{}, func(_ context.Context, v *configs.Variable, _ configs.EvalOverlay) (cty.Value, hcl.Diagnostics) {
		if v == nil {
			return cty.NilVal, nil
		}
		if v.Default != cty.NilVal {
			return v.Default, nil
		}
		return cty.DynamicVal, hcl.Diagnostics{&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Input variable not configured in build benchmark",
			Detail:   fmt.Sprintf("Synthetic build benchmark root call does not provide input variable %q.", v.Name),
			Subject:  v.DeclRange.Ptr(),
		}}
	}, rootDir, "")
}

func formatOutputValues(values []build.OutputValue) map[string]string {
	if len(values) == 0 {
		return nil
	}
	outputs := make(map[string]string, len(values))
	for _, ov := range values {
		outputs[ov.Addr.String()] = formatOutputValue(ov.Value)
	}
	return outputs
}

func formatOutputValue(v cty.Value) string {
	if !v.IsKnown() {
		return "(unknown)"
	}
	if v.IsNull() {
		return "null"
	}
	switch v.Type() {
	case cty.String:
		return v.AsString()
	case cty.Bool:
		if v.True() {
			return "true"
		}
		return "false"
	case cty.Number:
		return v.AsBigFloat().Text('f', -1)
	default:
		return v.GoString()
	}
}

// SpanAttrInt and ActionCounts are the only span-query helpers exposed beyond
// this file — both used by the txtar e2e harness in txtar_test.go. All other
// span querying lives in bench_test.go alongside the benchmark report helpers.

func SpanAttrInt(spans tracetest.SpanStubs, spanName, key string) int {
	for _, s := range spans {
		if s.Name != spanName {
			continue
		}
		for _, attr := range s.Attributes {
			if string(attr.Key) == key {
				return int(attr.Value.AsInt64())
			}
		}
	}
	return 0
}

func ActionCounts(spans tracetest.SpanStubs) (submitted, executed, cached, failed int) {
	for _, s := range spans {
		if s.Name != "dispatch.execute" {
			continue
		}
		submitted++
		if s.Status.Code == codes.Error {
			failed++
			continue
		}
		wasCached := false
		for _, attr := range s.Attributes {
			if string(attr.Key) == "build.action.cached" && attr.Value.AsBool() {
				wasCached = true
				break
			}
		}
		if wasCached {
			cached++
		} else {
			executed++
		}
	}
	return
}

// diagnosticCounter tracks diagnostics across a single Runner.Run, since
// diagnostic data isn't carried in spans — it's harness-level bookkeeping.

type diagnosticCounter struct {
	total     int
	errors    int
	warnings  int
	summaries map[string]int
	collected tfdiags.Diagnostics
}

func (c *diagnosticCounter) Append(diags tfdiags.Diagnostics) {
	for _, diag := range diags {
		c.total++
		c.collected = append(c.collected, diag)
		switch diag.Severity() {
		case tfdiags.Error:
			c.errors++
		case tfdiags.Warning:
			c.warnings++
		}
		if c.summaries != nil {
			c.summaries[diag.Description().Summary]++
		}
	}
}

func (c *diagnosticCounter) applyTo(result *Result) {
	result.Diagnostics = c.total
	result.ErrorDiagnostics = c.errors
	result.WarningDiagnostics = c.warnings
}

func (c *diagnosticCounter) hasErrors() bool {
	return c.errors > 0
}

func (c *diagnosticCounter) errorDetails() []string {
	return c.detailsBySeverity(tfdiags.Error)
}

func (c *diagnosticCounter) warningDetails() []string {
	return c.detailsBySeverity(tfdiags.Warning)
}

func (c *diagnosticCounter) detailsBySeverity(severity tfdiags.Severity) []string {
	if len(c.collected) == 0 {
		return nil
	}
	var ret []string
	for _, diag := range c.collected {
		if diag.Severity() != severity {
			continue
		}
		desc := diag.Description()
		ret = append(ret, desc.Summary+": "+desc.Detail)
	}
	slices.Sort(ret)
	return ret
}

