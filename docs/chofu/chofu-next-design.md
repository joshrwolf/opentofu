# chofu-next: Build Engine Design & Implementation

## Problem

The legacy OpenTofu graph walker is architecturally unsuited for use as a build engine. For the images-private workload (1,881 modules, ~66k resource instances), the legacy engine on the `chofu` branch takes **204s wall clock** — 14s for graph construction (806k vertices, 12x inflation) and 183s for the walk, dominated by an O(n^2) content hash bug in `CollectDependencyHashesByResource`.

A prior `dag2` experiment proved that a flat DAG + synchronous eval architecture could achieve 12.1s total, but was abandoned incomplete. The upstream "new evaluator" performed even worse at scale (196s, 5x legacy) due to unbounded goroutine spawning in `grapheval.Once`.

We started fresh on the `chofu-next` branch (clean fork of upstream OpenTofu `main`) to build a proper three-phase build engine.

## Architecture

### Three-Phase Pipeline

```
Config Load → Phase 1: Expand → Phase 2: Build DAG → Phase 3: Walk
```

All phases complete before the walk begins. No DynamicExpand during walk. No subgraph creation. No `interface{}` boxing. Flat integer-indexed arrays throughout.

### Package: `internal/chofu/`

Eight files, ~2,500 lines:

- **`types.go`** — `ID int32`, `VertexKind`, `Vertex`, `Graph` (flat arrays + lookup maps), `Results`
- **`expand.go`** — Phase 1: recursive config tree walk, evaluates for_each/count, emits `[]Vertex`
- **`graph.go`** — Phase 2: populates lookup maps, extracts HCL references for edges, cycle detection
- **`walk.go`** — Phase 3: Kahn's algorithm with bounded worker pool, cascading error skip
- **`eval.go`** — `evalData` implementing `lang.Data` (all 11 methods), backed by flat results array
- **`engine.go`** — `Build()` entry point orchestrating all phases with OTel tracing
- **`providers.go`** — Concurrent schema fetch, provider config/caching
- **`hash.go`** — O(1) content hash index
- **`target.go`** — `TargetFilter` with expansion pruning, graph subgraph extraction, and config walker filtering

### Key Design Decisions

**Reuse upstream APIs, don't reimplement them.** `lang.Scope` + `lang.Data` for expression evaluation, `instances.Expander` for module/resource expansion, `addrs.ParseRef` for reference parsing, `providers.Interface` for provider protocol.

**Flat integer-indexed DAG.** All vertex data in dense `[]Vertex` arrays indexed by `ID int32`. Edges in `[][]ID`. Lookup maps (`Vars`, `Locals`, `Outputs`, `ResInst`, `ChildOutputs`, `Providers`) for O(1) reference resolution. No pointer-based graph nodes. No `interface{}` boxing.

**No shared mutable state in expansion.** Variable values flow parent-to-child as parameters — each `expandModule` call receives its own `map[string]cty.Value`. This enables safe parallel expansion without locks.

**Targeting is first-class.** `TargetFilter` prunes at three levels: config loading (skip parsing irrelevant module directories), expansion (skip irrelevant module calls), and graph filtering (extract targeted subgraph with transitive dependencies).

## Performance Results

Benchmarked against images-private (1,881 modules, ~66k resources, 664k DAG vertices).

### Full Build (no targets)

| | Legacy (`chofu` branch) | chofu-next (initial) | chofu-next (optimized) |
|---|---|---|---|
| **Wall clock** | **204s** | **129s** | **44s** |
| Config load | ~5s (parallel) | 19s (serial+snapshot) | 15s (serial, no snapshot) |
| Expand | N/A (interleaved) | 71s | 7.2s |
| Graph build | 14s (806k vertices) | 6s | 2.3s |
| Walk | 183s (O(n^2) hash) | 31s | ~17s |
| GC % CPU | unknown | 33% | 31% |

### Targeted Build (`-target=module.nginx`)

| | Legacy | chofu-next |
|---|---|---|
| **Wall clock** | ~40s+ (no pruning) | **0.4s** |
| Config load | ~5s | 0.25s (target-filtered walker) |
| Expand | full | 12ms (2,092 vertices) |
| Graph + filter | full | 4ms |
| Walk | full | ~instant |

## Optimizations Applied

### 1. O(n^2) `collectChildOutputs` → O(1) Index

**Problem:** `collectChildOutputs` (eval.go) and `childModuleOutputIDs` (graph.go) scanned the entire `graph.Outputs` map for every module reference. With 39k outputs, this was 18s CPU.

**Fix:** Added `ChildOutputs map[addrKey][]ID` to Graph, built during Pass 1. Key = `(parent module, call name)`. Both call sites become direct map lookups.

**Impact:** 18s → 0s (eliminated from profile entirely).

### 2. Skip `maps.Copy` in `evalContext`

**Problem:** `lang.Scope.evalContext()` copied 270 function table entries into a new map for every expression evaluation. With millions of expression evals, this was 14s CPU + massive GC pressure.

**Fix:** Modified `internal/lang/eval.go` to assign the shared function map by reference. Copy-on-write only when a provider function needs injection (rare — our stub always errors). Added `funcsCopied` flag.

**Impact:** 14s → 0s (eliminated from profile entirely). Major GC reduction.

### 3. `SharedFuncs` on `lang.Scope`

**Problem:** `makeBaseFunctionTable` allocates 120 function objects, wraps each with `WithDescription`, duplicates all into `core::` namespace (~270 entries). Called once per Scope instance. With 657k vertices, that's 657k function tables.

**Fix:** Added `SharedFuncs` field to `lang.Scope`. When set, `Functions()` returns it directly — no lock, no allocation. Chofu pre-builds one table per unique `baseDir` (expansion: 1 table for workDir; walk: ~1,881 tables for module source dirs) and shares across all Scopes.

**Impact:** 4s function table construction + major GC reduction.

### 4. Scope Reuse in `evaluateLocals`

**Problem:** Created a new `lang.Scope` (and `expandData`) per local evaluation. The `vals` map is shared by reference — the new Scope per local was redundant.

**Fix:** Create one Scope before the locals loop. The `expandData.locals` field holds the `vals` map by reference, so each iteration sees previously evaluated locals.

**Impact:** Reduced Scope creation from 363k to ~1.9k during expansion.

### 5. Parallel Expansion of Sibling Module Calls

**Problem:** Expansion was serial — DFS walk processing 1,881 root module calls one at a time. Each module reads a JSON lock file and evaluates locals.

**Fix:** Refactored `expCtx` to eliminate shared mutable state (`varValues` map removed, variable values passed down as parameters). Sibling module calls dispatched via `sync.WaitGroup.Go`. The `instances.Expander` is already thread-safe.

**Impact:** Expansion 32s → 7.2s (~4.4x speedup).

### 6. Target-Filtered Config Loading

**Problem:** `localRun` parses all 1,881 modules before the engine starts — 16s even for a targeted build of one image. Also creates an unnecessary config snapshot.

**Fix:** Bypassed `localRun` in `opBuild`. Added `LoadConfigWithWalker` to `configload.Loader` that accepts a walker wrapper. `TargetFilter.WrapWalker` skips irrelevant module subtrees during `configs.BuildConfig` — the walker returns nil for non-targeted modules, so `buildChildModules` skips them entirely.

**Impact:** Targeted config load 16s → 0.25s. Full build: 19s → 15s (no snapshot overhead).

## Notable Discoveries

### Lazy Local Evaluation Hypothesis — Refuted

We hypothesized that most `jsondecode(file(...))` locals were consumed only by resources (walk-time), not by `for_each` (expansion-time), and could be deferred. Analysis of 1,413 image modules showed **91.8% use these locals directly in `for_each`**. The dominant pattern:

```hcl
locals { locked_configs = jsondecode(file("${path.module}/art.image.locks.json")).imageLocks }
module "build" { for_each = local.locked_configs }
```

Deferred evaluation is not viable. The optimization path is parallel expansion (spreading file I/O across cores), not lazy evaluation.

### GC is the Dominant Overhead

After eliminating algorithmic inefficiencies, GC accounts for 31% of CPU (38s of 122s total). The remaining application code in `evaluateLocals` is only ~2.3s — the 42s cumulative time was almost entirely GC thrashing from per-scope allocations. The `maps.Copy` elimination and `SharedFuncs` changes addressed the largest sources, but 664k vertices still create significant allocation pressure from `hcl.EvalContext` and `cty.Value` intermediates.

### Profile Attribution is Misleading

The 27s `syscall.rawsyscalln` under expansion initially appeared to be file I/O. Decomposition showed only ~9s was actual file reads (expansion) and ~12s was config loading. The rest was goroutine scheduling and memory management syscalls. Always decompose runtime overhead before optimizing.

### `addrs.ModuleInstance.String()` is Expensive at Scale

Module address string formatting appears throughout the hot path — map key construction, reference resolution, logging. The `unique.Handle[string]` interning on `addrKey` was added to amortize this, making map lookups compare pointers instead of hashing full strings.

## What We Reuse from Upstream (unmodified)

- `lang.Scope` + `lang.Data` — expression evaluation (modified: `SharedFuncs`, `maps.Copy` optimization)
- `lang.ReferencesInBlock/Expr` — static reference extraction
- `addrs.ParseRef` — reference parser
- `addrs.Targetable` — target containment semantics
- `instances.Expander` — module/resource expansion
- `configs.Config/Module/Resource` — config tree
- `configs.BuildConfig` + `ModuleWalker` — config tree construction
- `providers.Interface` — provider protocol
- `plugins.Library` — plugin lifecycle
- `states.*` — state types

## Remaining Work

- **Provider function support** — currently stubbed (`stubProviderFunction` returns error for `provider::oci::parse`)
- **Actual provider execution** — Plan/Apply calls, cache checking against prior state
- **State management** — write results to state, content hash persistence
- **`-exclude` support** — `TargetFilter` only handles `-target`, not `-exclude`
- **Upstream GC optimization** — lazy `evalVarBuilder` map initialization, pre-allocated Variables capacity
- **Config load parallelization** — parallel HCL parsing in `BuildConfig` for full builds
