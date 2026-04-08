// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/instances"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/plugins"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
	"github.com/opentofu/opentofu/internal/tracing/traceattrs"
)

// BuildHooks receives callbacks during the build walk. Implementations
// must be safe for concurrent use from multiple goroutines.
type BuildHooks interface {
	// PreBuildResource is called before a resource/data source vertex is
	// executed. addr is the fully-qualified instance address.
	PreBuildResource(addr addrs.AbsResourceInstance, action string)

	// PostBuildResource is called after a resource/data source vertex
	// completes. err is non-nil if the provider returned an error.
	PostBuildResource(addr addrs.AbsResourceInstance, action string, err error)
}

// BuildUI receives structured build events for rendering. Implementations
// must be safe for concurrent use. Unlike BuildHooks (which is a legacy
// bridge to tofu.Hook), BuildUI is the primary output interface for
// chofu builds.
//
// All output flows through Event — a single method carrying a tagged
// union. This makes it trivial to add new event types without breaking
// existing observers: unknown events are simply ignored.
type BuildUI interface {
	Event(BuildEvent)
}

// BuildEvent is a tagged union of all build lifecycle events. Exactly
// one field is non-nil.
type BuildEvent struct {
	PhaseStart       *PhaseStartEvent
	PhaseComplete    *PhaseCompleteEvent
	ResourceStart    *ResourceStartEvent
	ResourceComplete *ResourceCompleteEvent
	ProviderLog      *ProviderLogEvent
	BuildComplete    *BuildCompleteEvent
}

// PhaseStartEvent is emitted at the beginning of each build phase.
type PhaseStartEvent struct {
	Phase string // "schemas", "expand", "graph", "walk"
}

// PhaseCompleteEvent is emitted when a build phase finishes.
type PhaseCompleteEvent struct {
	Phase    string
	Duration time.Duration

	// Phase-specific counts, set when relevant.
	Vertices  int // graph phase: total vertices after target filtering
	Edges     int // graph phase: total edges
	ItemCount int // expand phase: total items produced
}

// ResourceStartEvent is emitted before a provider call for a resource.
type ResourceStartEvent struct {
	Addr     addrs.AbsResourceInstance
	Action   string        // "create" or "read"
	Provider addrs.Provider // provider type for log correlation
}

// ProviderLogEvent carries a structured log line from a provider binary.
// These arrive in real time during ApplyResourceChange via the hclog
// sink registered on the global InterceptLogger.
type ProviderLogEvent struct {
	// Source is the hclog logger name, e.g. "provider.terraform-provider-imagetest_v1.2.3".
	Source  string
	Level   string // "trace", "debug", "info", "warn", "error"
	Message string
	KVPairs []any // structured key-value pairs from hclog
}

// ResourceCompleteEvent is emitted after a resource provider call finishes.
type ResourceCompleteEvent struct {
	Addr        addrs.AbsResourceInstance
	Action      string
	Duration    time.Duration
	ContentHash string
	Err         error // nil on success
}

// BuildCompleteEvent is emitted once at the end of a build with summary stats.
type BuildCompleteEvent struct {
	Duration time.Duration
	DryRun   bool

	// Vertex counts by kind.
	Resources   int64
	DataSources int64
	Providers   int64
	Errors      int64
	Warnings    int64

	// Phase durations.
	SchemasDuration   time.Duration
	ExpansionDuration time.Duration
	GraphDuration     time.Duration
	WalkDuration      time.Duration

	// Diagnostics carries all errors and warnings from the build.
	// Observers are responsible for classifying (root cause vs cascade),
	// deduplicating, and rendering these appropriately.
	Diagnostics tfdiags.Diagnostics
}

// nilBuildUI is a no-op BuildUI used when no observer is configured.
type nilBuildUI struct{}

func (nilBuildUI) Event(BuildEvent) {}

// BuildOpts configures a build execution.
type BuildOpts struct {
	Config    *configs.Config
	State     *states.State
	Plugins   plugins.Library
	Variables map[string]cty.Value
	Targets   []addrs.Targetable
	Workspace string
	WorkDir   string
	Workers   int // 0 = GOMAXPROCS
	Hooks     BuildHooks
	UI        BuildUI
}

// BuildResult holds the outcome of a build execution.
type BuildResult struct {
	State     *states.State
	HashIndex *HashIndex
}

// buildContext holds shared state for an entire build execution. Both the
// global pipeline and per-module pipeline paths use this to construct
// walkContexts and coordinate shared resources.
type buildContext struct {
	config      *configs.Config
	pc          *providerCache
	hashIndex   *HashIndex
	filter      *TargetFilter
	sharedFuncs map[string]function.Function
	workDir     string
	workspace   string
	dryRun      bool
	workers     int
	hooks       BuildHooks
	ui          BuildUI
	stats       *walkStats
}

// newWalkContext creates a walkContext for a specific graph + results pair,
// inheriting all shared state from the buildContext.
func (bc *buildContext) newWalkContext(g *Graph, r *Results) *walkContext {
	return &walkContext{
		graph:       g,
		results:     r,
		pc:          bc.pc,
		config:      bc.config,
		hashIndex:   bc.hashIndex,
		workDir:     bc.workDir,
		workspace:   bc.workspace,
		dryRun:      bc.dryRun,
		sharedFuncs: bc.sharedFuncs,
		hooks:       bc.hooks,
		ui:          bc.ui,
		stats:       bc.stats,
	}
}

// newExpCtx creates an expCtx for structural expansion, sharing the
// buildContext's config, schemas, and function table.
func (bc *buildContext) newExpCtx(expander *instances.Expander) *expCtx {
	return &expCtx{
		config:      bc.config,
		schemas:     bc.pc.schemas,
		expander:    expander,
		filter:      bc.filter,
		workspace:   bc.workspace,
		workDir:     bc.workDir,
		rootDir:     bc.config.Module.SourceDir,
		sharedFuncs: bc.sharedFuncs,
	}
}

// Build executes a three-phase build pipeline:
//  1. Structural expansion — determine all module/resource instances
//  2. DAG construction — flat integer-indexed dependency graph
//  3. Walk — bounded concurrent evaluation with synchronous lang.Scope
//
// When all root-level module calls are independent (reference only variables
// and locals), Build uses a per-module pipeline that decomposes the global
// graph into independent mini-graphs processed in parallel. Otherwise it
// falls back to the global pipeline.
func Build(ctx context.Context, opts *BuildOpts) (*BuildResult, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	ctx, buildSpan := tracing.Tracer().Start(ctx, "chofu.Build")
	defer buildSpan.End()

	ui := opts.UI
	if ui == nil {
		ui = nilBuildUI{}
	}

	workers := opts.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	buildSpan.SetAttributes(traceattrs.Int64("chofu.workers", int64(workers)))

	workDir := opts.WorkDir
	if workDir == "" {
		workDir, _ = os.Getwd()
	}

	pc := newProviderCache(opts.Plugins.NewProviderManager())
	defer pc.Close(ctx)

	buildStart := time.Now()

	// ========== Phase 0: Fetch provider schemas ==========
	ui.Event(BuildEvent{PhaseStart: &PhaseStartEvent{Phase: "schemas"}})
	schemaStart := time.Now()
	_, schemaSpan := tracing.Tracer().Start(ctx, "chofu.FetchSchemas")
	providerTypes := collectProviderTypes(opts.Config)
	schemaSpan.SetAttributes(traceattrs.Int64("chofu.provider_count", int64(len(providerTypes))))
	schemaDiags := pc.FetchSchemas(ctx, providerTypes)
	schemaSpan.End()
	schemaDur := time.Since(schemaStart)
	ui.Event(BuildEvent{PhaseComplete: &PhaseCompleteEvent{Phase: "schemas", Duration: schemaDur}})
	diags = diags.Append(schemaDiags)
	if diags.HasErrors() {
		return nil, diags
	}

	// Pre-build a shared function table and propagate it to every module's
	// StaticEvaluator. Without this, each StaticEvaluator.Evaluate call
	// rebuilds the ~270-entry table from scratch — 56s of CPU on a full build.
	sharedFuncs := (&lang.Scope{
		BaseDir:           workDir,
		ProviderFunctions: stubProviderFunction,
	}).Functions()
	opts.Config.DeepEach(func(c *configs.Config) {
		if c.Module.StaticEvaluator != nil {
			c.Module.StaticEvaluator.SharedFuncs = sharedFuncs
		}
	})

	bc := &buildContext{
		config:      opts.Config,
		pc:          pc,
		hashIndex:   NewHashIndex(),
		filter:      NewTargetFilter(opts.Targets),
		sharedFuncs: sharedFuncs,
		workDir:     workDir,
		workspace:   opts.Workspace,
		dryRun:      os.Getenv("CHOFU_DRY_RUN") == "1",
		workers:     workers,
		hooks:       opts.Hooks,
		ui:          ui,
		stats:       &walkStats{},
	}

	// Determine whether the per-module pipeline is eligible.
	// Requirements: root module has no resources/data sources, and all
	// root-level module calls are independent (reference only vars/locals).
	usePipeline := len(opts.Config.Module.ManagedResources) == 0 &&
		len(opts.Config.Module.DataResources) == 0
	if usePipeline {
		for _, mc := range opts.Config.Module.ModuleCalls {
			if !isIndependentCall(mc) {
				usePipeline = false
				break
			}
		}
	}

	var expandDur, graphDur, walkDur time.Duration
	var totalVerts int

	if usePipeline {
		buildSpan.SetAttributes(traceattrs.Bool("chofu.pipeline", true))
		totalVerts, expandDur, graphDur, walkDur, diags = bc.buildPipeline(ctx, opts.Variables, diags)
	} else {
		buildSpan.SetAttributes(traceattrs.Bool("chofu.pipeline", false))
		totalVerts, expandDur, graphDur, walkDur, diags = bc.buildGlobal(ctx, opts.Variables, diags)
	}

	buildSpan.SetAttributes(traceattrs.Int64("chofu.total_diags", int64(len(diags))))

	// ========== Summary ==========
	totalDur := time.Since(buildStart)
	stats := bc.stats
	errors := stats.errors.Load()
	warnings := countWarnings(diags)

	ui.Event(BuildEvent{BuildComplete: &BuildCompleteEvent{
		Duration:          totalDur,
		DryRun:            bc.dryRun,
		Resources:         stats.resources.Load(),
		DataSources:       stats.dataSources.Load(),
		Providers:         stats.providers.Load(),
		Errors:            errors,
		Warnings:          int64(warnings),
		SchemasDuration:   schemaDur,
		ExpansionDuration: expandDur,
		GraphDuration:     graphDur,
		WalkDuration:      walkDur,
		Diagnostics:       diags,
	}})

	if opts.UI == nil {
		fmt.Fprintf(os.Stderr, "\n")
		if bc.dryRun {
			fmt.Fprintf(os.Stderr, "Build complete (dry-run)\n")
		} else {
			fmt.Fprintf(os.Stderr, "Build complete\n")
		}
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  Vertices:     %d (%d resources, %d data sources)\n",
			totalVerts, stats.resources.Load(), stats.dataSources.Load())
		fmt.Fprintf(os.Stderr, "  Providers:    %d configured\n", stats.providers.Load())
		if errors > 0 {
			fmt.Fprintf(os.Stderr, "  Errors:       %d\n", errors)
		}
		if warnings > 0 {
			fmt.Fprintf(os.Stderr, "  Warnings:     %d\n", warnings)
		}
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  Schemas:      %s\n", schemaDur.Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "  Expansion:    %s\n", expandDur.Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "  Graph:        %s\n", graphDur.Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "  Walk:         %s\n", walkDur.Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "  Total:        %s\n", totalDur.Round(time.Millisecond))
		fmt.Fprintf(os.Stderr, "\n")
	}

	return &BuildResult{
		State:     opts.State,
		HashIndex: bc.hashIndex,
	}, diags
}

// countErrors counts the number of error diagnostics in a set.
func countErrors(diags tfdiags.Diagnostics) int64 {
	var n int64
	for _, d := range diags {
		if d.Severity() == tfdiags.Error {
			n++
		}
	}
	return n
}

// countWarnings counts the number of warning diagnostics in a set.
func countWarnings(diags tfdiags.Diagnostics) int64 {
	var n int64
	for _, d := range diags {
		if d.Severity() == tfdiags.Warning {
			n++
		}
	}
	return n
}
