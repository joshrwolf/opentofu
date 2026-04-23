# Build System Redesign: Suspension Model with `dice` Package

## Status

Design document. Not yet implemented.

## Problem

The build system uses a Salsa-style incremental computation framework (`memo`)
to memoize solver queries. Salsa was designed for edit-recompute cycles
(rust-analyzer): a user edits a file, inputs change, and the system figures out
which derived values to recompute. Our system has no edit-recompute cycle. It
runs once per session.

The "mutations" aren't external edits — they're action completions from our own
engine. The system evaluates HCL targets, some depend on runtime values that
require engine execution, and after those actions complete, previously-deferred
targets can resolve. This evaluation-execution co-dependency is solved today by
a polling loop in the executor that calls `WaitForAdvance` after every action
completion, broadcasts to all waiting goroutines, and re-queries the solver.

At real scale (~92k actions, ~1-2k deferred targets), every action completion
broadcasts to every waiting goroutine. Each woken goroutine re-queries the
solver, which re-verifies volatile memos. Most find nothing changed. The entire
volatile-dep / revision-tracking / `Advance` / `WaitForAdvance` machinery
exists to bridge the gap between Salsa's pull model and the engine's push model.

## Solution

Replace the `memo` package with a purpose-built `dice` package (named after
Buck2's DICE — Demand-driven Incremental Computation Engine). The core insight
from DICE: when a computation needs a value that requires execution, the
computation **suspends** (the goroutine blocks on the engine's result channel).
When the action completes, the goroutine resumes exactly where it left off. No
polling. No revision counter. No broadcast.

Go goroutines + channel blocking IS the suspension mechanism. The engine's
`getAction` already creates placeholder entries with `done` channels — if
`engine.Result(key)` is called before `engine.Submit(key)`, the goroutine
blocks on the placeholder and unblocks when the action is later submitted and
completes. The engine already supports this pattern; it just isn't being used.

## Architecture Overview

```
build.Load → *Loaded (immutable: Config, Catalog)
                ↓
build.NewEngine → *Engine
    creates:
      dice.Session (session-scoped cancellation + computation registry)
      engine.Engine (action executor with semaphore, locks, cache)
      Evaluator (wraps Loaded, holds HCL evaluation caches)
      Solver (12 dice.Computations, references Evaluator + Engine)
                ↓
build.Execute(selectors) → Result
    StreamSelections → discover targets/outputs
    per target: solver.TargetValue(ctx, addr)  → goroutine suspends until resolved
    per output: solver.OutputValue(ctx, addr)  → goroutine suspends until resolved
    collect outputs → Result
```

The executor is ~60 lines. No polling loop. No `InspectTarget`. No
`submitTransitiveClosure`. No `WaitForAdvance`. No dead-target detection.

## The `dice` Package

### What we take from DICE (Buck2)

- **Suspension via goroutine blocking.** DICE's async `.await` → Go's channel
  blocking. Compute functions call other `Computation.Get` methods which block
  transparently if the value isn't ready.
- **Submit-on-discover.** DICE's `ctx.compute` triggers evaluation. Our
  `registerSpec` triggers `engine.Submit` as a side effect of computing action
  specs.
- **Coalescing via shared execution.** DICE's `DicePromise` (multiple waiters
  on one task) → our `execution.done` channel (multiple goroutines block on one
  `close()`).
- **Session-scoped execution contexts.** DICE derives execution contexts from
  the transaction, not from individual callers. Our execution contexts derive
  from the session, so caller cancellation detaches without killing the
  computation.
- **Dep recording for future incremental support.** DICE records deps during
  computation for cross-build verification. We record deps even in v1 (cheap:
  ~6 bytes per dep) to preserve the option for watch mode / daemon mode.

### What we leave behind

- **Versioned graph with version ranges.** Cross-build state. We don't persist.
- **Single-threaded core state processor.** Graph mutation serialization. We
  don't mutate the graph within a session.
- **Series-parallel dep encoding.** Topology-aware dep verification. We don't
  verify deps in v1.
- **Transaction model.** Concurrent builds at different versions. One session.
- **Projection keys.** Cheap synchronous derived values. We don't have this
  pattern.
- **Epoch-based task cancellation.** Version supersession. No versions in v1.

### Public API

```go
package dice

// Session is the shared scope for a group of computations. All
// computations registered with a session share a cancellation
// boundary and a computation registry (for cycle detection and
// future cross-computation dep verification).
type Session struct { /* unexported */ }

func NewSession(ctx context.Context) *Session

// Computation is a concurrent, demand-driven, memoized computation.
//
// K is the comparable cache key. A is the argument passed to the
// compute function (may be richer than K — the key func extracts K
// from A). V is the result type.
//
// Compute functions may call Get on other Computations. The calling
// goroutine suspends until the dependency is available. This is the
// demand-driven model: computation pulls values as needed, blocking
// transparently when a value requires work (including blocking on
// engine action results).
type Computation[K comparable, A any, V any] struct { /* unexported */ }

// Register creates a Computation bound to the given session.
//
//   name:    diagnostic prefix for cycle errors and stats.
//   fn:      produces V from A. Runs at most once per distinct K
//            within the session. May call Get on other Computations.
//   key:     extracts the comparable cache key from A.
//   label:   human-readable name for A (cycle error messages).
//   opts:    future extension point (e.g., WithEqual for early cutoff).
func Register[K comparable, A any, V any](
    s     *Session,
    name  string,
    fn    func(ctx context.Context, arg A) (V, error),
    key   func(A) K,
    label func(A) string,
    opts  ...Option[V],
) *Computation[K, A, V]

// Get returns the value for arg, computing it if necessary.
//
// If another goroutine is already computing the same key, Get blocks
// until that computation finishes and returns the same result
// (coalescing).
//
// The compute function runs under a context derived from the Session
// (not from ctx). Caller cancellation detaches this caller without
// cancelling the computation. If ALL callers cancel, the computation
// is abandoned and its context is cancelled.
//
// Panics in the compute function propagate to all waiting callers.
// Cycles are detected and returned as errors.
func (c *Computation[K, A, V]) Get(ctx context.Context, arg A) (V, error)

// Option configures a Computation. No options are defined in v1.
// The variadic parameter exists so that WithEqual (for early cutoff
// in incremental mode) can be added without changing Register's
// signature.
type Option[V any] func(*config[V])
```

### Internal Data Structures

```go
// Session holds the session context, a revision counter (always 1
// in v1, used by future invalidation), and a registry of
// computations for cross-computation dep verification.
type Session struct {
    ctx        context.Context
    revision   atomic.Uint64    // v1: always 1. v2: bumped by Advance().
    queries    atomic.Pointer[[]queryRuntime]
    registerMu sync.Mutex
    mu         sync.Mutex       // v2: protects cond
    cond       *sync.Cond       // v2: for WaitForAdvance
}

// result holds a cached computation outcome.
type result[V any] struct {
    value V
    err   error
    deps  []depRef   // what this computation read during evaluation
}

// slot is the per-entry synchronization state.
type slot[V any] struct {
    snap atomic.Pointer[result[V]]  // nil → not computed; non-nil → done
    mu   sync.Mutex                 // protects exec
    exec *execution[V]              // non-nil → computation in flight
}

// execution tracks an in-flight computation with coalesced waiters.
type execution[V any] struct {
    ctx     context.Context
    cancel  context.CancelCauseFunc
    done    chan struct{}
    waiters int
    value   V
    err     error
}

// depRef identifies a dependency: which computation, which slot.
type depRef struct {
    query queryID   // computation's registration index
    slot  slotID    // entry index within the computation
}

// Sharded index and paged entry store are carried over from the
// current memo package. 64 shards with RWMutex each. Paged entry
// store with atomic page pointers for lock-free reads.
```

### The Get Algorithm

```
get(ctx, arg):
    key = c.key(arg)
    entry = getOrCreate(key, arg)     // sharded index lookup or insert

    // 1. Fast path: already computed (lock-free).
    if snap := entry.snap.Load(); snap != nil:
        recordDep(ctx, ref)
        return snap.value, snap.err

    // 2. Cycle detection.
    if detectCycle(ctx, ref):
        return zero, cycleError

    // 3. Caller cancellation.
    if ctx.Err() != nil:
        return zero, ctx.Err()

    // 4. Lock entry.
    entry.mu.Lock()

    // 5. Double-check under lock.
    if snap := entry.snap.Load(); snap != nil:
        entry.mu.Unlock()
        recordDep(ctx, ref)
        return snap.value, snap.err

    // 6. Coalesce: join active execution.
    if entry.exec != nil:
        exec = entry.exec
        exec.waiters++
        entry.mu.Unlock()
        return wait(ctx, exec)      // block on exec.done channel

    // 7. Caller cancellation before compute.
    if ctx.Err() != nil:
        entry.mu.Unlock()
        return zero, ctx.Err()

    // 8. Start execution. Context derived from session, not caller.
    exec = newExecution(session.ctx)
    exec.waiters = 1
    entry.exec = exec
    entry.mu.Unlock()

    // Detach caller on cancel (caller goes away, computation continues).
    handle = detachOnCancel(ctx, entry, exec)

    // Run compute function with cycle frame and dep recorder.
    recorder = newDepRecorder()
    computeCtx = withDepRecorder(pushFrame(exec.ctx, ref, name), recorder)
    value, err = fn(computeCtx, entry.arg)
    handle.stop()

    // Check execution cancellation (abandonment).
    if cause = context.Cause(exec.ctx); cause != nil:
        clearExec(entry, exec)
        exec.err = cause
        close(exec.done)
        if session.ctx.Err() != nil:
            return zero, cause           // session cancelled
        if handle.detached && ctx.Err() != nil:
            return zero, ctx.Err()       // caller cancelled
        return get(ctx, arg)             // abandonment → retry

    // Publish result.
    deps = recorder.collect()
    entry.snap.Store(&result{value, err, deps})
    clearExec(entry, exec)
    exec.value = value
    exec.err = err
    close(exec.done)                     // wakes all coalesced waiters

    recordDep(ctx, ref)
    return value, err
```

This is the current memo's `getSlot` with dep verification (steps 6-7 of the
original) removed. The coalescing, cancellation-detach, and abandonment logic
are preserved — they're correct and necessary.

### Package Layout

```
internal/build/dice/
    session.go       Session, NewSession, queryRuntime registry      (~40 lines)
    computation.go   Computation, Register, Get, Stats               (~60 lines)
    slot.go          slot, result, entry, entryStore, indexShard      (~120 lines)
    eval.go          getSlot, compute, wait, detach, detachOnCancel   (~180 lines)
    dep.go           depRef, depRecorder, recordDep, withDepRecorder  (~60 lines)
    cycle.go         callFrame, detectCycle, cycleError, pushFrame    (~50 lines)
```

~510 lines total. The current `memo` package is ~1,400 lines.

### Future: Incremental Mode (v2)

When we add watch mode / daemon mode, the dice package gains:

```go
// Advance bumps the session revision. On next Get, computations
// re-verify their deps before returning cached values.
func (s *Session) Advance()

// WaitForAdvance blocks until the revision passes since.
func (s *Session) WaitForAdvance(ctx context.Context, since uint64) uint64

// WithEqual enables early cutoff: if a recomputed value equals the
// cached value, downstream dependents skip re-verification.
func WithEqual[V any](eq func(V, V) bool) Option[V]
```

The Get fast path gains a `verifiedAt` check:

```
// v2 fast path:
rev = session.Revision()
if entry.verifiedAt.Load() == rev:
    if snap := entry.snap.Load(); snap != nil:
        recordDep(ctx, ref)
        return snap.value, snap.err
// ... verification: walk snap.deps, check changedAfter for each ...
```

The dep data recorded in v1 is exactly what v2 needs for verification. No
re-architecture required — the verification path is an insertion into the
existing Get algorithm, and `changedAfter` is a new method on each
Computation's slot that checks whether a cached value changed after a given
revision.

## Solver Changes

### Computation Map (14 → 13 queries)

```
Layer 0 — Static facts
    moduleScopes      Computation[ModulePathKey, ModulePath, *PreparedScope]
    targetEvals       Computation[AddrKey, Addr, EvalResult]
    outputEvals       Computation[AddrKey, Addr, EvalResult]
    bindings          Computation[ProviderKey, bindingArg, Binding]

Layer 1 — Instance resolution
    packages          Computation[ModulePathKey, ModulePath, Packages]
    instances         Computation[AddrKey, Addr, Instances]

Layer 2 — Configuration + action specification
    targets           Computation[AddrKey, Addr, EvalResult]
    actions           Computation[AddrKey, Addr, engine.Spec]
    actionSets        Computation[AddrKey, Addr, ActionSet]

Layer 3 — Runtime values (block on engine execution)
    outputs           Computation[AddrKey, Addr, EvalResult]
    targetValues      Computation[AddrKey, Addr, EvalResult]
    targetCollections Computation[AddrKey, Addr, EvalResult]
    moduleCollections Computation[ModulePathKey, ModulePath, EvalResult]
```

**Added:** `moduleScopes` — pre-builds the HCL `PreparedScope` per module path.
Addresses the 68% cumulative allocation hotspot (`lang.Scope.evalContext`).
See the "Module Scope as Computation" section below.

**Deleted as memo queries:** `addrValueDeps`, `modValueDeps`. The ValueDeps
memo queries are deleted, but the **dep collection logic is preserved** as a
non-memoized helper called inside `computeActionSpec` (see below).

### Solver Construction Modes

A solver is either **execution-capable** (engine != nil, submits actions) or
**inspection-only** (engine == nil, query command). This is fixed at
construction and immutable.

```go
type Config struct {
    Providers *buildprovider.Session
    Engine    *engine.Engine   // nil for inspection-only solvers
}
```

The solver needs `engine.Submit` (to submit specs on discovery) and
`engine.Result` (to block on action completion). The read-only `engine.Reader`
interface is deleted. Inspection-only solvers (engine=nil) never submit or
block on engine results.

Layer 3 computations (`targetValues`, `outputs`, `targetCollections`,
`moduleCollections`) require an execution-capable solver. Their compute
functions guard on `s.engine != nil` and return a clear error if called on an
inspection-only solver:

```go
func (s *Solver) computeTargetValue(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
    if s.engine == nil {
        return EvalResult{}, fmt.Errorf("target %s requires engine execution", addr)
    }
    // ...
}
```

The query command never calls Layer 3 methods — it uses `TargetConfig`,
`ActionSpec`, `OutputEval` for display.

### Action Submission via `registerSpec`

Specs are submitted to the engine as they're computed:

```go
func (s *Solver) registerSpec(ctx context.Context, spec engine.Spec) error {
    if spec.Key == (digest.Digest{}) {
        return fmt.Errorf("cannot register spec with zero key")
    }
    s.specMu.Lock()
    if _, ok := s.specIndex[spec.Key]; ok {
        s.specMu.Unlock()
        return nil
    }
    s.specIndex[spec.Key] = spec
    s.specMu.Unlock()
    if s.engine != nil {
        if _, err := s.engine.Submit(ctx, spec); err != nil {
            return err
        }
    }
    return nil
}
```

`registerSpec` returns error (not fire-and-forget). Zero-key specs are
rejected, not silently skipped. `engine.Submit` failures propagate to the
caller.

### Transitive Dep Collection (replaces ValueDeps memo queries)

The recursive ref-walking logic from `value_deps.go` is preserved as a
non-memoized helper `collectExecDeps`. This function walks a target's refs,
computes action specs for each (triggering `registerSpec` → Submit), and
returns the dep keys.

**Invariant:** No key is appended to `ExecDeps` or `After` unless the exact
`engine.Spec` for that key has been successfully registered and submitted.

`computeActionSpec` calls `collectExecDeps` to compute its `ExecDeps` and
`After` fields. By the time `computeActionSpec` returns, all transitive dep
specs have been computed, registered, and submitted to the engine.

The helper preserves the current `value_deps.go` semantics: target refs,
declaration refs, output refs, module refs, module input deps, deferred
module/package deps, provider function calls, and explicit `depends_on`.

This is NOT a memo query — it runs every time `computeActionSpec` runs. This is
acceptable because `computeActionSpec` itself is memoized by the dice
`Computation`, so the dep collection runs at most once per action spec per
session. The dep collection calls other dice computations (like `actions.Get`)
which are themselves memoized, so the recursive walk hits caches for previously
computed specs.

### `computeTargetValue` — The Core Suspension Point

Current (peek + return Deferred):
```go
func (s *Solver) computeTargetValue(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
    spec, err := s.actions.Get(ctx, addr)
    if err != nil {
        targetConfig, configErr := s.targets.Get(ctx, addr)
        if configErr == nil && targetConfig.Deferred {
            return EvalResult{Deferred: true}, nil
        }
        return EvalResult{}, err
    }
    memo.RecordVolatile(ctx)
    record, ok, _ := s.runtime.LookupRecord(ctx, spec.Key)
    if !ok {
        return EvalResult{Deferred: true}, nil
    }
    // decode record ...
}
```

New (block until available):
```go
func (s *Solver) computeTargetValue(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
    spec, err := s.actions.Get(ctx, addr)
    if err != nil {
        return EvalResult{}, err
    }
    result, err := s.engine.Result(ctx, spec.Key)
    if err != nil {
        return EvalResult{}, err
    }
    // decode result.Record — always concrete, never Deferred
}
```

### Deferred Instance Resolution

Instances may defer when `for_each` or `count` depends on a value that isn't
statically resolvable. The two cases:

**Case 1: Module input depends on a runtime target value.** For example,
`for_each` references a variable whose value is passed from a parent module
that depends on a runtime target. The RuntimeResolver can resolve this — the
goroutine suspends while the upstream target's action executes, then the
RuntimeResolver returns the concrete value.

```go
func (s *Solver) computeDeclaredTargetInstances(ctx context.Context, addr catalog.Addr) (Instances, error) {
    // ... module resolution (may suspend via packages.Get) ...

    // Pass 1: static evaluation (no runtime resolver)
    result, diags := s.source.EvalTargetInstances(ctx, EvalTargetInstancesRequest{
        DeclAddr: concreteAddr,
    })
    if diags.HasErrors() { return Instances{}, wrapDiags(diags) }
    if !result.Deferred {
        return expandInstances(concreteAddr, result), nil
    }

    // Pass 2: re-evaluate with runtime resolver — goroutine may suspend
    // while RuntimeResolver.TargetValue/OutputValue/ModuleValue block on
    // upstream engine results via solver Layer 3 queries.
    runtimeResolver := solverRuntimeResolver{solver: s}
    result, diags = s.source.EvalTargetInstances(ctx, EvalTargetInstancesRequest{
        DeclAddr: concreteAddr,
        Runtime:  runtimeResolver,
    })
    if diags.HasErrors() { return Instances{}, wrapDiags(diags) }
    if result.Deferred {
        // Still deferred after runtime resolution — this is a genuine
        // unresolvable deferral (e.g., non-query-safe provider function
        // in for_each). Return error rather than caching a permanently
        // deferred value.
        return Instances{}, fmt.Errorf("target %s instances could not be resolved: for_each depends on values not available at build time", concreteAddr)
    }
    return expandInstances(concreteAddr, result), nil
}
```

**Case 2: Non-query-safe provider function in `for_each`.** This is a
pre-existing limitation. The HCL evaluator marks non-query-safe provider
functions as deferred before evaluation (`eval_instances.go:449`). The
RuntimeResolver resolves target/output/module values but NOT provider function
results. Neither the current system nor the redesign can resolve this case. The
compute function returns an error after the second pass still defers.

If this becomes a real requirement, the fix is a runtime-backed provider
function resolver that decodes action records into provider function return
values and injects them into the HCL evaluation context. This is out of scope
for the initial redesign.

The two-pass pattern applies to `computeTargetConfig`, `computeOutputValue`,
and `computeOutputEval` where the HCL evaluator may return deferred results
that the RuntimeResolver can resolve.

**Why two passes is always sufficient.** The repetition expression (`for_each`,
`count`, `enabled`) references the same variables on both passes. Pass 1
evaluates without RuntimeResolver and defers because some variables can't be
resolved statically. Pass 2 provides RuntimeResolver, which resolves those
variables to concrete values by blocking on upstream engine results. If the
expression still defers after RuntimeResolver is available, it's a genuine
limitation (e.g., provider function in `for_each`), not a fixable deferral.

A third pass would only be needed if pass 2's resolution revealed new instance
keys whose configs required different runtime values that were themselves
deferred. But this can't happen: `computeDeclaredTargetInstances` only
evaluates the repetition expression, not the config body. The config body is
evaluated by `computeTargetConfig`, which is a separate dice `Computation` with
its own deferred resolution.

### Bounded Fan-Out for Collection Evaluation

`targetCollectionElements` (`runtime.go:242`) and `moduleCollectionElements`
(`runtime.go:261`) loop through instances and call `targetValues.Get` /
`outputs.Get` one at a time. In the suspension model, each Get may block on
`engine.Result`. Without mitigation, a target with 16 instances would submit
and wait for actions serially — a significant concurrency regression.

**Fix:** Two-phase pattern with bounded parallel pre-submission.

```go
func (s *Solver) targetCollectionElements(ctx context.Context, addrs []catalog.Addr) ([]cty.Value, []digest.Digest, error) {
    // Phase 1: pre-submit all instance action specs in parallel.
    // This triggers computeActionSpec → registerSpec → engine.Submit
    // for all instances concurrently, so the engine can execute them
    // in parallel.
    var wg sync.WaitGroup
    errs := make([]error, len(addrs))
    for i, addr := range addrs {
        wg.Go(func() {
            _, errs[i] = s.actions.Get(ctx, addr)
        })
    }
    wg.Wait()
    // Errors are not fatal here — targetValues.Get handles them below.

    // Phase 2: collect values in stable order. Engine is already
    // executing all actions. Each Get either returns immediately
    // (cache hit) or blocks briefly (action in flight).
    values := make([]cty.Value, 0, len(addrs))
    digests := make([]digest.Digest, 0, len(addrs))
    for _, addr := range addrs {
        result, err := s.targetValues.Get(ctx, addr)
        if err != nil { return nil, nil, err }
        if !result.Known { return nil, nil, nil }
        values = append(values, result.Value)
        digests = append(digests, result.Digest)
    }
    return values, digests, nil
}
```

The same pattern applies to `moduleCollectionElements` (fan out
`outputs.Get` for all outputs) and `moduleInstanceValue` (fan out per-output
resolution).

### `Deferred` Field Changes

The `Deferred` field stays on `EvalResult` and `InstanceResult` as an internal
signal from the HCL evaluator — it means "the HCL expression couldn't be
fully evaluated." But it no longer propagates through the solver's return types.
Solver compute functions handle deferred resolution internally (two-pass or
suspension). The solver's public methods always return concrete values or
errors.

The `Deferred` field is removed from `Instances`, `Packages`, and `ActionSet`
— these solver-internal types always carry concrete data.

## Module Scope as Computation

### Problem

Scope construction dominates evaluator cost. Every `EvalTarget`, `EvalOutput`,
`EvalTargetInstances`, and `EvalProvider` call goes through
`moduleEvalOptions` (`eval_options.go:21`), which recursively walks the module
parent chain. For a leaf module at depth 3, that's 3 levels of:

1. Finding the parent's `*configs.Config`
2. Building a `Variables` closure that calls `StaticEvaluator.EvaluateWithOptions`
   for each variable attribute — which calls `scopeWithOptions(...).EvalExpr(...)`,
   rebuilding a full `lang.Scope` from scratch (**the 68% allocation hotspot**)
3. Evaluating the module call's `count`/`for_each` for repetition options
4. Constructing a `PreparedScope`

At real scale: ~1,920 leaf modules × ~8 targets each = ~15k EvalTarget calls.
Each walks 3 levels and reconstructs the scope. That's ~45k scope
constructions. The `moduleEvalCache` catches static-path duplicates, but the
runtime path bypasses it (the `Variables` closure captures the `runtime`
parameter, so caching a static-only scope and serving it for runtime calls
produces a scope that can't resolve runtime references).

The dice model's single-evaluation-per-key guarantee doesn't help here:
each `computeTargetConfig(addr)` is a distinct key, so each independently
calls `moduleEvalOptions(module, runtime)`.

### Solution: `moduleScopes` dice computation

Scope construction becomes a dice computation keyed by module path.

```go
func (s *Solver) computeModuleScope(ctx context.Context, module catalog.ModulePath) (*configs.PreparedScope, error) {
    if module.Len() == 0 {
        return s.source.PrepareRootScope(ctx)
    }
    parentScope, err := s.moduleScopes.Get(ctx, module.Parent())
    if err != nil {
        return nil, err
    }
    return s.source.PrepareChildScope(ctx, module, parentScope)
}
```

The recursive structure maps naturally to dice: child scope depends on parent
scope, dice handles memoization and coalescing. The first target in a module
triggers scope construction; all subsequent targets in the same module get the
cached `*PreparedScope` from a fast-path atomic load.

**Scale impact:** Scope constructions drop from ~45k to ~3,840 (1 root + 1,920
parents + 1,920 leaves). Each remaining construction is cheaper because
`PrepareChildScope` evaluates variable expressions using the parent's
`PreparedScope.Evaluate` (the parent's pre-built `hcl.EvalContext`) instead of
calling `EvaluateWithOptions` (which builds a fresh `lang.Scope`). That's the
fast path: one `expr.Value(hclCtx)` call with zero context-building overhead.

### Evaluator interface additions

```go
type Evaluator interface {
    // Existing 5 methods...

    PrepareRootScope(ctx context.Context) (*configs.PreparedScope, error)

    // PrepareChildScope builds a child module's PreparedScope from
    // the parent's. Evaluates variable expressions from the module
    // call using parent.Evaluate (fast path). Resolves eligible
    // locals from the eval cache.
    PrepareChildScope(ctx context.Context, module catalog.ModulePath, parent *configs.PreparedScope) (*configs.PreparedScope, error)
}
```

### Runtime resolution via WithOverlay

`moduleScopes` is a Layer 0 computation — scopes are built without runtime
resolver. Variables whose expressions reference runtime values get
`cty.DynamicVal` during preparation (they're recorded in `skippedVars`).

At evaluation time, compute functions overlay runtime resolution onto the
pre-built scope:

```go
func (s *Solver) computeOutputValue(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
    scope, err := s.moduleScopes.Get(ctx, addr.Module)
    if err != nil { return EvalResult{}, err }

    // Pass 1: evaluate with static scope
    result, diags := s.source.EvalOutputWithScope(ctx, EvalOutputRequest{
        Addr: addr, Scope: scope,
    })
    if !result.Deferred { return result, wrapDiags(diags) }

    // Pass 2: overlay runtime resolver
    runtimeScope := scope.WithOverlay(configs.StaticEvalOptions{
        Runtime: runtimeLookupForModule(addr.Module, solverRuntimeResolver{solver: s}),
    })
    result, diags = s.source.EvalOutputWithScope(ctx, EvalOutputRequest{
        Addr: addr, Scope: runtimeScope,
    })
    return result, wrapDiags(diags)
}
```

`PreparedScope.WithOverlay` already exists (`prepared_scope.go:198`). It
creates a child scope sharing the base `hcl.EvalContext` and adds runtime
capability. Cost: one struct allocation. The base context — the expensive
part — is shared.

### contextForRefs extension (cross-package change)

Currently, `PreparedScope.contextForRefs` (`prepared_scope.go:312-319`) ALWAYS
falls back for runtime reference types, even when `hasRuntime` is true:

```go
case addrs.Resource, ..., addrs.OutputValue, ...:
    if !p.hasRuntime {
        return nil, true
    }
    return nil, true  // ← always falls back
```

This must change: when `hasRuntime` is true AND `opts.Runtime` is set (from the
overlay), `contextForRefs` should eagerly resolve runtime refs via
`opts.Runtime` and build a child `hcl.EvalContext` with the resolved values
alongside the pre-resolved static values. This keeps the fast path (one
`expr.Value(hclCtx)` call) for expressions referencing runtime values.

This is a change to `internal/configs/prepared_scope.go` — shared
infrastructure, not build-specific. The change is safe because the only current
user of `PreparedScope` is the build pipeline, and the behavior only activates
when `opts.Runtime` is non-nil (which only the build pipeline sets).

### Impact on Loaded

With `moduleScopes` as a dice computation, `Loaded` simplifies:

**Deleted from Loaded:**
- `moduleEvalCache` (map + RWMutex) — subsumed by `moduleScopes`
- `targetRepetitionCache` (map + RWMutex) — subsumed by `instances` computation
- `importRepetitionCache` (map + RWMutex) — subsumed by `packages` computation
- `preparedScopeCache` (map + RWMutex) — subsumed by `moduleScopes`
- `moduleEvalOptions` function (~130 lines) — logic moves to `PrepareChildScope`

**Retained on Loaded:**
- `evalCacheValues` + `evalCacheMu` — pure local variable cache, shared across
  sibling module instances within a `PreparedScope.Prepare` call. An
  optimization internal to scope construction, not a structural cache.
- `purityAnalysis` + `purityMu` — one-time analysis, effectively immutable.

4 caches and 3 mutexes deleted. Loaded goes from 6 caches / 5 mutexes to
2 caches / 2 mutexes.

## Executor Changes

The executor shrinks from ~400 lines to ~60 lines:

```go
func (e *executor) Execute(ctx context.Context, selectors selector.Set) (Result, tfdiags.Diagnostics) {
    var (
        wg         sync.WaitGroup
        diags      diagCollector
        outputMu   sync.Mutex
        outputAddrs []catalog.Addr
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
            // Launch in a goroutine — discoverModuleTargets calls
            // solver.TargetInstances which may suspend on deferred
            // instance resolution. Running inline would block
            // StreamSelections from discovering subsequent addresses.
            launch(func() {
                e.discoverModuleTargets(ctx, addr, &diags, func(a catalog.Addr) {
                    launch(func() {
                        if _, err := e.solver.TargetValue(ctx, a); err != nil {
                            diags.append(unwrapDiags(err))
                        }
                    })
                })
            })
        case catalog.TargetKindOutput:
            outputMu.Lock()
            outputAddrs = append(outputAddrs, addr)
            outputMu.Unlock()
            launch(func() {
                if _, err := e.solver.OutputValue(ctx, addr); err != nil {
                    diags.append(unwrapDiags(err))
                }
            })
        case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
            launch(func() {
                if _, err := e.solver.TargetValue(ctx, addr); err != nil {
                    diags.append(unwrapDiags(err))
                }
            })
        }
        return ctx.Err() == nil
    })
    diags.append(selectionDiags)

    wg.Wait()
    // ... collect outputs ...
}
```

**Key detail:** `discoverModuleTargets` runs in a goroutine, not inline in
the `StreamSelections` callback. In the dice model, `solver.TargetInstances`
may suspend on deferred instance resolution (two-pass with RuntimeResolver).
Running it inline would block `StreamSelections` from discovering subsequent
addresses, stalling the entire submission pipeline behind one deferred module.
By launching it in a goroutine, discovery proceeds concurrently with deferred
resolution.

`StreamSelections` itself also calls `solver.TargetInstances` and
`solver.PackageInstances` to expand instances during the tree walk. These Layer
1 calls may now suspend. At images-private scale, most modules have static
`for_each` (from JSON lockfiles), so Layer 1 resolves without suspension. The
root module's children are all static. Suspension only occurs for the rare
module with runtime-dependent instance counts, and those deferred modules are
yielded as declaration addresses (not expanded inline) — their expansion
happens in the launched goroutine.

**Deleted from executor:** `resolveTarget` (100 lines), `resolveOutput` (55
lines), `submitAndAwait` / `submitAndAwaitAll` / `submitTransitiveClosure` (60
lines), `needsInstanceExpansion`, `resolveTargetInstances`,
`activeResolvers` counter, dead-target detection logic.

## Engine Changes

### Deleted

- `LookupRecord` — replaced by `Result` (blocking).
- `HasPending` — only used by dead-target detection.
- `OnComplete` callback — no revision to advance.
- The `Reader` interface type — solver gets full `*Engine`.

### Unchanged

- `Submit` — fire-and-forget action submission.
- `Result` — blocking wait for action completion.
- `Run` — Submit + Result convenience method.
- `getAction` — creates placeholder entries (this is what makes suspension
  work — `Result` before `Submit` blocks on the placeholder's `done` channel).
- `execute` — cache check, dep wait, semaphore, locks, runner, cache store.
- `waitForDependencies` — waits for ExecDeps and After by calling `Result`.
- Lock coordination, panic recovery, tracing — all unchanged.

## Evaluator Interface Changes

### Deleted

- `EvalModuleInputs` — only used by `moduleInputValueDeps` in the deleted
  ValueDeps path. Module input evaluation is still handled by
  `moduleEvalOptions` in the HCL package.

### Simplified

The `Evaluator` interface drops from 6 methods to 5:
```go
type Evaluator interface {
    EvalImportInstances(context.Context, EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics)
    EvalTargetInstances(context.Context, EvalTargetInstancesRequest) (InstanceResult, tfdiags.Diagnostics)
    EvalProvider(context.Context, EvalProviderRequest) (EvalResult, tfdiags.Diagnostics)
    EvalTarget(context.Context, EvalTargetRequest) (EvalResult, tfdiags.Diagnostics)
    EvalOutput(context.Context, EvalOutputRequest) (EvalResult, tfdiags.Diagnostics)
}
```

The HCL evaluator implementation (`*Loaded`) is unchanged. It still returns
`Deferred: true` when expressions can't be fully evaluated. The solver handles
deferred resolution internally.

## Solver Public API Changes

### Deleted

- `InspectTarget` / `InspectOutput` — existed for the polling loop. For the
  query command's display needs, equivalent logic moves to the command layer
  as local helper functions that call `ActionSpec`, `TargetConfig`, and
  `OutputValue` directly.
- `ValueDeps` / `ModuleValueDeps` — existed to tell the executor which actions
  to submit. The solver now submits directly.
- `CollectTransitiveSpecs` — may still be useful as a diagnostic/debug tool,
  but is no longer called in the execution path.
- `InvalidateRuntimeState` — no revision system.

### Changed

- `ActionSpec` calls `registerSpec` which now submits to the engine.
- Public methods may change from returning `tfdiags.Diagnostics` to returning
  `error` (see below).

### Consider: Solver returns `error` instead of `tfdiags.Diagnostics`

The solver's public methods currently return `(V, tfdiags.Diagnostics)`. The
memo compute functions return `(V, error)`. Every public method does
`unwrapDiags(err)`. Every compute function that calls the Evaluator does
`wrapDiags(diags)`.

With the redesign, consider making the solver's public API return `(V, error)`
to match the dice package's contract. The `diagsError` wrapper preserves source
ranges. The executor converts to `tfdiags.Diagnostics` at its boundary (it
already has `unwrapRunnerDiags` for engine errors). This removes ~15
`unwrapDiags` calls from solver methods.

## What Gets Deleted

| Component | Lines | Reason |
|---|---|---|
| `internal/build/memo/` (entire package) | ~1,400 | Replaced by `dice/` |
| `solve/value_deps.go` (as memo queries) | ~520 | Queries deleted; ~200 lines of dep collection logic preserved as non-memoized helper in `computeActionSpec` path |
| `solve/inspect.go` | ~180 | No polling loop to drive |
| Executor polling loops | ~250 | Direct solver calls replace them |
| `engine.LookupRecord` | ~20 | Replaced by `engine.Result` |
| `engine.HasPending` | ~5 | Dead-target detection removed |
| `engine.OnComplete` | ~10 | No revision to advance |
| `engine.Reader` interface | ~10 | Solver gets full `*Engine` |
| `hcl/eval_options.go:moduleEvalOptions` | ~130 | Logic moves to `PrepareChildScope` |
| Loaded caches (4 maps + 3 mutexes) | ~80 | Subsumed by `moduleScopes` computation |
| **Total deleted** | **~2,600** | |
| **Total added** (`dice/` + moduleScope) | **~610** | |
| **Net reduction** | **~2,000 lines** | |

## Migration Plan

This is a greenfield redesign with no backwards compatibility stage. All code
is internal to `internal/build/` with no external consumers. The txtar tests in
`bench/testdata/` and the benchmarks in `bench/bench_test.go` are the
acceptance criteria.

### Phase 1: `dice` Package

Write `internal/build/dice/` from scratch. The package has no dependency on the
current `memo` package. The sharded index and paged entry store designs are
carried over (they're well-optimized), but the implementation is fresh.

Test with unit tests that verify: memoization, coalescing, cycle detection,
caller cancellation/detachment, abandonment, panic propagation, dep recording.

### Phase 2: Solver on `dice`

Rewrite the solver to use `dice.Computation` instead of `memo.Query`.

- Replace `*memo.Database` with `*dice.Session`.
- Replace each `memo.New(...)` with `dice.Register(...)`.
- Replace each `memo.Query.Get` with `dice.Computation.Get`.
- Delete equality functions (`evalResultDigestEqual`, etc.).
- Delete `InvalidateRuntimeState`.
- Give the solver `*engine.Engine` (nil for inspection-only).
- Change `registerSpec` to submit to the engine and return error.
- Change `computeTargetValue` to block on `engine.Result`.
- Change `computeOutputValue` to use the two-pass suspension pattern.
- Add two-pass deferred resolution in `computeDeclaredTargetInstances`,
  `computeTargetConfig`, and `computeOutputValue` where needed.
- Extract dep collection logic from `value_deps.go` into a non-memoized
  helper `collectExecDeps`. Delete the `addrValueDeps` and `modValueDeps`
  memo queries but preserve the recursive ref-walking logic. The invariant:
  no key in ExecDeps/After unless its spec is registered and submitted.
- Delete `inspect.go` (move display helpers to query command).
- Remove `Deferred` from `Instances`, `Packages`, `ActionSet`.
- Add bounded fan-out in `targetCollectionElements` and
  `moduleCollectionElements` (pre-submit phase + collect phase).
- Add `moduleScopes` dice computation. Add `PrepareRootScope` and
  `PrepareChildScope` to the Evaluator interface. Implement in HCL package.
  Change compute functions (`computeTargetConfig`, `computeOutputValue`,
  `computeTargetEval`, `computeOutputEval`) to accept scope from
  `moduleScopes.Get`. Add `Scope` field to Eval* request types (or add
  `EvalTargetWithScope`/`EvalOutputWithScope` methods).
- Extend `PreparedScope.contextForRefs` to eagerly resolve runtime refs
  via `opts.Runtime` instead of falling back to `EvaluateWithOptions`.
  This is a cross-package change in `internal/configs/prepared_scope.go`.
- Delete `moduleEvalOptions` (~130 lines), `moduleEvalCache`,
  `targetRepetitionCache`, `importRepetitionCache`, `preparedScopeCache`
  and their mutexes from Loaded.

The solver's 14 queries become 13. The public API shrinks.

**Loaded cache simplification.** The `moduleScopes` dice computation subsumes
`moduleEvalCache`, `preparedScopeCache`, `targetRepetitionCache`, and
`importRepetitionCache`. See "Module Scope as Computation" section for details.
The remaining `evalCacheValues` and `purityAnalysis` caches stay on Loaded.

### Phase 3: Executor Simplification

Replace the executor with the ~60-line version that calls solver methods
directly. Delete `resolveTarget`, `resolveOutput`, `submitTransitiveClosure`,
`submitAndAwait*`, `activeResolvers`, dead-target detection.

### Phase 4: Engine Cleanup

Delete `LookupRecord`, `HasPending`, `OnComplete`, the `Reader` interface.
Remove the `onComplete` callback from `engine.Config`.

### Phase 5: Wire Up and Validate

Update `build.go:NewEngine` to create `dice.Session` and wire the solver with
the engine. Update `command/query.go` to use solver methods directly instead of
`InspectTarget`/`InspectOutput`. Run the txtar tests and benchmarks.

### Phase 6: Delete `memo`

Once all references are removed, delete `internal/build/memo/`.

### Phase 7: Cleanup Items

Address items from `CLEANUP.md` that are still relevant after the redesign
(module-prefix dedup, singleton registries, etc.). Some items (like
"consolidate caching on Loaded") may be superseded by the Evaluator split
if we pursue that separately.
