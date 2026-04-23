# Build Engine Isolation

This document describes the refactoring plan to establish a clean boundary
between the Terraform/HCL frontend and the source-independent execution engine
in `internal/build/`.

## Problem

The architecture doc (`docs/build-native-architecture.md`) calls for a
source-neutral core that HCL and CUE can both target. In practice, the boundary
is not enforced:

- `solve/` is described as source-neutral but implements Terraform module system
  semantics (instance expansion via count/for_each, provider alias resolution,
  module path walking). It imports `cty` for runtime value assembly and `hcl/v2`
  for diagnostics.
- `action/` is the boundary type between frontend and engine, but it imports
  `catalog/`, `provider/`, and `run/`, pulling in `cty`, `hcl`, `addrs`,
  `configs`, `providers`, and the full Terraform dependency tree transitively.
- `dispatch/` (the execution engine) looks clean at the direct-import level but
  inherits the entire Terraform universe through `action/`.
- `source/source.go` defines an `Evaluator` interface so the solver can call the
  HCL evaluator without importing it, but the solver and evaluator are so
  tightly coupled that the interface requires 6 methods, 6 request types, and
  10+ shared types with `any` escape hatches.

The result: there is no package in the build subsystem that can be compiled,
tested, or reasoned about without the Terraform/HCL dependency tree. The
"source-neutral" label is aspirational, not actual.

## Design

### Principle

The build subsystem has two fundamentally different layers:

1. **The execution engine.** Takes a DAG of action specs (digests, dependency
   edges, lock domains, opaque runner payloads), executes them in dependency
   order, manages locks and caching, and produces result records. This layer has
   no reason to know about Terraform, HCL, cty, providers, modules, or
   instances.

2. **The HCL/Terraform frontend.** Parses HCL, loads configs, expands instances
   via count/for_each, resolves provider bindings across module boundaries,
   evaluates expressions, discovers dependencies, lowers targets to action specs,
   and materializes runtime values from execution results back into cty. This
   layer is deeply and correctly coupled to Terraform semantics.

The refactoring goal is to make this boundary real: the engine core compiles with
zero Terraform dependencies, and the frontend is free to use Terraform types
everywhere without pretending otherwise.

### End-state package layout

```text
internal/build/
    # Engine core — zero Terraform knowledge
    action/          Spec, RunnerSpec, LockKey, SourceRef
    engine/          execution, dependency waiting, locks, caching, runner dispatch
    runtime/         Record, Reader interface
    digest/          content-addressed hashing
    task/            keyed lazy computation primitive

    # Shared declarations
    catalog/         Package, TargetDecl, Addr, ModulePath, Key, etc.
    selector/        pattern matching over catalog

    # Terraform provider protocol — shared by all TF-targeting frontends
    provider/        sessions, schemas, request/result encoding, capabilities
    run/             subprocess request building, result value decoding

    # HCL/Terraform frontend — solver + evaluator merged
    hcl/             everything Terraform: parsing, evaluation, solving,
                     runtime value resolution, action lowering

    # Application wiring
    build.go         constructs engine + frontend, top-level API
    session.go       session type backed by hcl/ frontend
    executor.go      selection + build orchestration loop
    runner.go        runner implementations (provider target, provider function, run)
```

### What changes

#### 1. `action/` loses its payload types and external imports

Today `action/` defines `Spec`, `RunnerSpec`, and three payload types
(`ProviderTargetPayload`, `ProviderFunctionPayload`, `RunPayload`). The payload
types import `provider/` and `run/`, which pull in the Terraform dependency
tree.

After:

- `action/` defines only `Spec`, `RunnerSpec`, `LockKey`, and `SourceRef`.
- `RunnerSpec.Kind` becomes `string` (not `catalog.RunnerKind`).
- `RunnerSpec.Payload` remains `any`, but no concrete payload types are defined
  in `action/`.
- `RunnerSpec.Provider` and `RunnerSpec.TypeName` are removed from the struct.
  That information is part of the opaque payload.
- `action.SourceRef` is a local type (filename + two positions), not imported
  from `catalog/`.
- `action/` imports only `digest/`.

The payload types move to the protocol packages where they naturally belong:

- `ProviderTargetPayload` and `ProviderFunctionPayload` → `provider/`
- `RunPayload` → `run/`

Both `hcl/` (which constructs payloads) and `runner.go` (which decodes them)
already import `provider/` and `run/`.

#### 2. `dispatch/` renames to `engine/`

The architecture doc calls this package `engine`. The current name `dispatch`
describes one of its responsibilities (runner dispatch) but not the others
(dependency waiting, lock management, caching, result tracking).

After the `action/` split, `engine/` imports only:

- `action/` → `digest/`
- `runtime/` → `digest/`
- `task/`
- `digest/`

Zero transitive Terraform dependencies.

#### 3. `solve/` and `source/hcl/` merge into `hcl/`

The solver and the HCL evaluator are tightly coupled. The solver calls the
evaluator (via `source.Evaluator`) and the evaluator calls back into the solver
(via `source.RuntimeResolver`). The interface between them exists only because
they are in separate packages, and it requires extensive `any` escape hatches
to avoid cty imports in `solve/`.

After:

- `source/hcl/` and `solve/` merge into `internal/build/hcl/`.
- The `source.Evaluator` interface (6 methods, 6 request types) is eliminated.
  The solver calls evaluation functions directly.
- The `source.RuntimeResolver` interface (3 methods) is eliminated. The solver
  creates the runtime resolver directly.
- `source.EvalResult`, `source.InstanceResult`, `source.InstanceShape`,
  `source.ProviderFunctionCall`, and `source.ProviderFunctionRef` become
  package-internal types in `hcl/`.
- `solve.Solver`, `solve.Instances`, `solve.Packages`, `solve.ValueDeps`,
  `solve.TargetInspection`, `solve.OutputInspection`, etc. become
  package-internal types in `hcl/`.
- The merged package uses `cty`, `hcl/v2`, `addrs`, `configs`, and `providers`
  directly, without apology.

The `source/` directory and `source/source.go` are deleted.

#### 4. `source/hcl/` drops the `source/` prefix

The package path changes from `internal/build/source/hcl` to
`internal/build/hcl`. It is not just a "source adapter" — it is the full
HCL/Terraform frontend: parsing, catalog lowering, evaluation, solving, runtime
value resolution, action lowering, and compatibility shims.

#### 5. `session.go` and `executor.go` use simplified types

The executor currently depends on `solve.TargetInspection`,
`solve.OutputInspection`, `solve.Instances`, and `solve.ValueDeps`. It only
reads a small subset of their fields.

After: the session exposes executor-oriented types defined in the top-level
`build` package:

```go
type TargetStatus string

const (
    TargetStatusLowerable TargetStatus = "lowerable"
    TargetStatusDeferred  TargetStatus = "deferred"
    TargetStatusBlocked   TargetStatus = "blocked"
)

type TargetInspection struct {
    Status   TargetStatus
    Spec     action.Spec
    DepSpecs []action.Spec
    Reason   tfdiags.Diagnostics
}

type OutputStatus string

const (
    OutputStatusKnown    OutputStatus = "known"
    OutputStatusDeferred OutputStatus = "deferred"
    OutputStatusBlocked  OutputStatus = "blocked"
)

type OutputInspection struct {
    Status   OutputStatus
    DepSpecs []action.Spec
    Reason   tfdiags.Diagnostics
}

type TargetInstances struct {
    Addrs    []catalog.Addr
    Deferred bool
}
```

These types reference only `action.Spec`, `catalog.Addr`, and
`tfdiags.Diagnostics`. The rich solver types (`ValueDeps`, `Packages`,
`ReverseDepsResult`, `Instances` with Shape/Refs/ProviderFunctionCalls) stay
internal to `hcl/`.

The `hcl/` package constructs these types when the session methods are called.
The `build` package imports `hcl/` and uses a concrete session type — no
interface. The architecture doc advises against introducing broad interfaces
before there are multiple implementations. When a CUE frontend arrives, we
introduce the session interface then, informed by what CUE actually needs.

#### 6. `catalog/` keeps its `addrs` dependency for now

`catalog/` uses `addrs.Provider` and `addrs.LocalProviderConfig` for provider
identity. These are Terraform provider ecosystem types, not HCL syntax types.
Both HCL and CUE frontends targeting Terraform providers would use them.

The fix (defining `catalog.ProviderAddr` and `catalog.ProviderRef` as
catalog-owned types, converting at boundaries) is mechanical but touches dozens
of files and every test. It is a separate follow-up pass, not part of this
refactoring.

### What does not change

- `digest/` — shared hashing substrate. No changes.
- `task/` — shared lazy computation. No changes.
- `runtime/` — record type and reader interface. No changes.
- `selector/` — pattern matching over catalog. No changes.
- `catalog/` — structural declarations. No changes beyond `addrs` follow-up.
- `provider/` — Terraform provider protocol. Gains payload types from `action/`.
  Otherwise unchanged.
- `run/` — subprocess execution. Gains `RunPayload` from `action/`. Otherwise
  unchanged.
- `runner.go` — runner implementations. Imports change (payload types from
  `provider/` and `run/` instead of `action/`), dispatch logic unchanged.

### Dependency graph after refactoring

Engine core (zero Terraform dependencies):

```text
engine/ → action/ → digest/
engine/ → runtime/ → digest/
engine/ → task/
engine/ → digest/
```

HCL frontend:

```text
hcl/ → catalog/, action/, provider/, run/, digest/, runtime/
hcl/ → cty, hcl/v2, addrs, configs, providers
```

Application wiring:

```text
build (top-level) → hcl/, engine/, catalog/, action/, provider/, selector/
runner.go → action/, provider/, run/, engine/
```

The engine is testable with synthetic action specs. No HCL, no providers, no
cty. The HCL frontend uses Terraform types freely. The runner implementations
are the only code that bridges both worlds (type-asserting opaque payloads to
concrete types).

## Implementation order

The refactoring is ordered to keep tests passing at every step.

### Phase 1: Split `action/` payload types

Move `ProviderTargetPayload` and `ProviderFunctionPayload` to `provider/`.
Move `RunPayload` to `run/`. Move `action.RunnerSpec.Provider` and
`action.RunnerSpec.TypeName` into the payload types. Make `RunnerSpec.Kind` a
plain `string`. Define `action.SourceRef` locally instead of importing from
`catalog/`. Update all call sites.

After this phase, `action/` imports only `digest/`. The engine's transitive
dependency tree shrinks dramatically.

### Phase 2: Rename `dispatch/` → `engine/`

Rename the package directory and update all imports. Mechanical change.

### Phase 3: Merge `solve/` + `source/hcl/` → `hcl/`

Move all files from `solve/` and `source/hcl/` into `internal/build/hcl/`.
Remove the `source.Evaluator` and `source.RuntimeResolver` interfaces. Have the
solver call evaluation functions directly and vice versa. Delete
`source/source.go`. Update all imports.

This is the largest phase. The test file `solve/solver_test.go` (4,278 lines)
moves into `hcl/` and its test infrastructure merges with the existing
`source/hcl/*_test.go` files.

### Phase 4: Simplify session types

Define executor-oriented types (`TargetInspection`, `OutputInspection`,
`TargetInstances`) in the top-level `build` package. Have the `hcl/` session
construct these from its internal solver types. Remove `solve.*` types from
the session's public API. Update `executor.go` to use the simplified types.

### Phase 5: Clean up

- Remove `source/` directory.
- Remove any unused types or interfaces.
- Run `go mod tidy`.
- Verify that `go list -deps ./internal/build/engine` contains zero cty, hcl,
  addrs, configs, or providers packages.
- Verify all tests pass.

### Future work (not part of this refactoring)

- Define `catalog.ProviderAddr` and `catalog.ProviderRef` to remove the `addrs`
  dependency from `catalog/`.
- Consolidate `catalog.SourceRef`, `action.SourceRef`, and
  `tfdiags.SourceRange` into fewer types.
- Introduce a session interface when the CUE frontend arrives and proves what
  the abstraction needs to look like.
