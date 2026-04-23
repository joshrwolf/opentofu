# Build-Native Architecture

This document proposes a clean-slate architecture for `tofu build`.

The goal is to design the build system around the workload we actually care
about:

- very large configurations
- fast time to first action
- explicit support for side effects
- future `query` and introspection support
- source neutrality, so HCL is the first source language rather than the only
  one

This is intentionally not a Terraform plan/apply architecture with a build
engine attached. It is a build architecture that can use Terraform/HCL as one
source language.

## 1. Goals

### Hard requirements

1. `tofu build` must handle `images-private`-scale configurations and continue
   to scale as those workloads grow.
2. `tofu build '**'` must start useful work quickly. There must be no global
   "analyze everything before running anything" barrier.
3. The system must support side effects as first-class behavior.
4. The same internal facts must support both:
   - build execution
   - query / introspection
5. The core architecture must not be coupled to HCL.
6. HCL and CUE must be source adapters, not architectural centers.

### Non-goals

1. Preserving plan/apply compatibility in the build path.
2. Reusing the evaluator stack just because it already exists.
3. Supporting every Terraform language feature on day one.
4. Pre-optimizing for remote/distributed execution before the local architecture
   is sound.

## 2. Core principles

### 2.1 Query-first

If a fact matters for build scheduling, cache identity, diagnostics, or
debugging, it must exist in an explicit form that query can inspect without
executing the action.

Examples:

- why a target exists
- which provider binding it uses
- what value dependencies it has
- what explicit order dependencies it has
- which lock or effect domains it requires
- what action key it lowered to
- why it is blocked

If those answers exist only implicitly inside evaluation control flow, query
will always be weak.

### 2.2 Stream analysis into execution

The architecture must allow:

1. cheap structural loading
2. immediate target expansion
3. lazy semantic resolution
4. action emission as soon as a target is runnable
5. engine execution while further analysis continues

This is the key requirement for fast time to first action on broad target
patterns.

### 2.3 Side effects need more than dependencies

For this system, a build graph is not just:

- value dependencies
- order dependencies

It must also model:

- effect or lock domains

Two actions may have no data edge and no explicit `depends_on`, but still must
not run concurrently if they mutate the same external system.

### 2.4 Source neutrality

The core packages must not import HCL/configs or CUE packages. Those belong at
the source edge.

The architecture should work for:

- HCL only
- mixed HCL + CUE
- CUE only

without a second architecture rewrite.

## 3. Package layout

The source-neutral build subsystem should look like this:

```text
internal/build/
    session.go
    config.go

    task/
    digest/

    selector/
        pattern.go
        parse.go
        match.go

    catalog/
        catalog.go
        package.go
        target.go
        import.go
        provider.go

    source/
        driver.go
        hcl/
        cue/

    provider/
        session.go
        binding.go
        functions.go
        invoke.go

    solve/
        solver.go
        package.go
        target.go
        output.go
        action.go

    action/
        spec.go
        edge.go
        lock.go

    engine/
        engine.go
        cache.go
        result.go
```

### Why these names

- `task` is the shared lazy computation primitive used by both `solve` and
  `engine`.
- `digest` is shared substrate for identity.
- `catalog` is syntax-neutral.
- `source` makes HCL and CUE explicit adapters.
- `solve` says what the package does: compute lazy semantic facts.
- `action` is clearer than `ir`.
- `engine` runs actions and should not know about source language details.
- `selector` is broader than `targets`, which matters once query becomes a
  first-class feature.
- cross-run cache starts inside `engine` because it is an execution concern, not
  a core semantic layer.

## 4. Package responsibilities

### `task`

Shared keyed lazy computation primitive.

Responsibilities:

- first caller runs the computation inline
- joiners wait for the shared result
- in-session memoization
- cycle detection
- caller-driven cancellation
- usable by both `solve` and `engine`

The exact API can evolve, but the shape should stay close to:

```go
type Task[T any] struct { /* unexported */ }
type Node struct { /* unexported */ }

func NewNode() *Node
func (t *Task[T]) Do(ctx context.Context, node *Node, key any, f func(context.Context) (T, error)) (T, error)
```

This is a low-level building block and should remain source-neutral and
engine-neutral.

### `digest`

Stable comparable digests used for:

- action identity
- provider bindings
- cache keys
- optionally later, solver fact keys

This is true shared substrate and deserves its own package.

### source-owned evaluated values

There is intentionally no top-level `internal/build/value` package in the first
implementation.

Rules:

- HCL-specific evaluation may use `cty` inside `source/hcl` and HCL-owned solve
  code.
- Future CUE-specific evaluation may use CUE's own value model inside its
  source adapter and source-owned solve code.
- Shared build packages must not depend on either value system.
- The shared core trades in explicit facts, identities, digests, dependency
  edges, bindings, runner payloads, and lowered `action.Spec` values.

If a true cross-source value transport becomes necessary later, it should be
added only after a concrete mixed-source use case proves what that transport
must represent.

### engine-owned cache

Cross-run cache is an engine concern in the first implementation.

Responsibilities:

- store completed action records by action key
- serve cache hits for engine execution
- stay outside semantic correctness

This is intentionally not a top-level package at first. Query and solve should
not depend on it.

### `selector`

User-facing selection syntax and matching.

Responsibilities:

- parse target/query patterns
- match against the loaded catalog
- stream matches without materializing the whole world

The selector operates over source-neutral catalog identities. HCL-style address
syntax is one selector grammar, not the only possible one.

### `catalog`

The loaded, normalized declaration graph.

This is the first source-neutral internal representation.

Responsibilities:

- packages
- imports
- declared targets
- declared outputs
- declared providers
- source ranges
- source-specific metadata that later stages need

The catalog is structural, not semantic. It knows what was declared, but not
yet what those declarations evaluate to.

### `source`

Source adapters live here.

`source/hcl` responsibilities:

- parse and decode HCL
- lower HCL/configs structures into `catalog`
- preserve enough metadata for later semantic resolution

`source/cue` responsibilities:

- parse and decode CUE
- lower CUE package structure into the same `catalog`

The source edge is the only place that should know about syntax-specific ASTs
and decoding rules.

### `provider`

Provider lifecycle and provider-facing semantics.

Responsibilities:

- provider process/session management
- provider alias handling
- provider binding resolution
- provider functions
- provider invocation helpers

This package is shared by `solve` and by concrete action runners.

Provider-function rule:

- pure, cheap, deterministic provider functions may execute in `solve` if they
  are explicitly marked query-safe
- provider functions that perform I/O, remote reads, mutation, or other
  meaningfully cacheable work must not execute inside `solve`
- those non-query-safe provider functions must lower into
  `action.Spec{Runner.Kind: "provider.function"}` and run through `engine`
- this boundary keeps query honest and ensures real work is visible for
  scheduling, caching, and diagnostics

### `solve`

The heart of the system.

This package computes lazy semantic facts over the catalog and lowers runnable
work into `action.Spec`.

Responsibilities:

- package instance expansion
- target instance expansion
- provider binding resolution
- config evaluation
- value dependency discovery
- explicit dependency expansion
- output evaluation
- action lowering

This is the shared foundation for both `build` and `query`.

### `action`

Explicit execution graph types.

Responsibilities:

- executable action specifications
- typed edges
- lock/effect keys
- cache and volatility policy

This package is the boundary between semantic resolution and execution.

### `engine`

Pure action executor.

Responsibilities:

- coalescing and memoization
- cache lookup/write
- dependency waiting
- lock acquisition
- limiter integration
- result records

The engine should know nothing about HCL, CUE, modules, or provider alias
syntax.

## 5. Core types

The exact details can evolve, but the shape should be close to this.

### identity

Every important object in the system needs a source-neutral identity.

Declaration identities belong to `catalog`. Instance identities belong to
`solve`.

```go
type InstanceKey struct {
    Kind   InstanceKeyKind
    Int    int64
    String string
}

type PackageID string
type TargetID string
type OutputID string
type ProviderID string

type PackageInstanceID struct {
    Path []PackageStep
}

type TargetInstanceID struct {
    Package PackageInstanceID
    Target  TargetID
    Key     InstanceKey
}

type OutputInstanceID struct {
    Package PackageInstanceID
    Output  OutputID
}

type BindingID struct {
    Package  PackageInstanceID
    Provider ProviderID
    Alias    string
    Key      InstanceKey
}
```

Rules:

- HCL addresses and CUE selectors are translated into these identities at the
  `selector` and `source/*` boundaries.
- No core package should treat source-language address strings as canonical
  identity.
- `selector` matches over these identities, not over HCL-specific syntax.

### evaluated values

There is no shared core value algebra in v1.

Rules:

- evaluated values remain source-owned
- HCL evaluation may use `cty`
- future CUE evaluation may use a CUE-native value model
- shared packages must not depend on either representation
- hashing and lowering happen at the source/provider boundary, producing
  digests, payload bytes, bindings, references, and `action.Spec` inputs

This is deliberate. The system should not invent a fake source-neutral value
system until a concrete mixed-source requirement proves one is necessary.

### `catalog`

```go
type Catalog struct {
    Root *Package
}

type Package struct {
    ID        PackageID
    Name      string
    Source    SourceRef
    Imports   []Import
    Targets   []TargetDecl
    Outputs   []OutputDecl
    Providers []ProviderDecl
}

type TargetDecl struct {
    ID       TargetID
    Kind     TargetKind
    Name     string
    Source   SourceRef
    Driver   string
    Payload  any
}
```

Notes:

- `Payload` is source-specific declaration data owned by the source driver.
- The catalog does not require the rest of the system to understand HCL blocks
  or CUE nodes directly.

### `provider`

```go
type Binding struct {
    ID         BindingID
    Provider   ProviderID
    ConfigKey  digest.Digest
    ConfigData any
}
```

Notes:

- Binding identity must include alias and package-instance context.
- `ConfigData` is provider-owned config payload, not a core-wide value type.
- `ConfigKey` becomes part of action identity.

### `action`

```go
type Spec struct {
    Key       digest.Digest
    Name      string
    Class     string

    ExecDeps  []digest.Digest
    After     []digest.Digest
    Locks     []LockKey

    Cacheable bool
    Volatile  bool

    Runner    RunnerSpec
    Source    SourceRef
}
```

Edge meanings:

- `ExecDeps`: data needed before running
- `After`: order-only edges
- `Locks`: mutual exclusion domains

That distinction is essential for side-effecting workloads.

### `engine`

```go
type Record struct {
    ActionKey    digest.Digest
    OutputKey    digest.Digest
    Payload      []byte
    Volatile     bool
}
```

## 6. Source driver contract

The source layer should expose a small fact-oriented interface.

Not a giant "frontend" abstraction.

Something close to:

```go
type Driver interface {
    Load(ctx context.Context, req LoadRequest) (*catalog.Package, error)

    EvalTarget(ctx context.Context, req EvalTargetRequest) (*EvalResult, error)

    EvalOutput(ctx context.Context, req EvalOutputRequest) (*EvalResult, error)
}

type EvalResult struct {
    Data        any
    Digest      digest.Digest
    Refs        []catalog.Reference
    Volatile    bool
    Sensitive   bool
}
```

Design rules:

- keep the interface small
- keep it fact-oriented
- keep syntax-specific behavior behind the driver boundary
- do not expose HCL-specific or CUE-specific node types to the core packages
- `Load` lowers declarations into `catalog`, but does not build the semantic
  graph
- `EvalTarget` and `EvalOutput` evaluate one declaration body in a scope that
  `solve` prepared
- `EvalResult.Data` is source-owned evaluated payload, not a shared core value
  type
- `EvalResult.Digest` is the canonical hashed form used by the shared core
- only pure/query-safe provider functions may execute through this evaluation
  path
- provider functions that are remote, effectful, or otherwise real executable
  work must be lowered by `solve` into explicit `provider.function` actions
  instead of being hidden inside source evaluation
- target instance expansion, provider binding, explicit dependency resolution,
  and action lowering are always owned by `solve`, not by `source/*`
- `EvalResult.Refs` is the explicit source-language reference output consumed by
  `solve`; dependency discovery must not be hidden inside evaluation control
  flow

## 7. Solve graph

`solve` should be implemented as a lazy keyed fact system.

Examples of useful facts:

- `PackageInstances(package-instance)`
- `ProviderBinding(target-instance)`
- `TargetConfig(target-instance)`
- `ValueDeps(target-instance)`
- `ExplicitDeps(target-instance)`
- `ActionSpec(target-instance)`
- `OutputValue(output-instance)`

Each fact:

- is memoized
- can depend on other facts
- can be queried directly
- can be lowered to action specs when relevant

This is where `task.Task` fits naturally.

## 8. Build flow

The happy path for `tofu build` should be:

1. Open a build session.
2. Load the root package into `catalog`.
3. Use `selector` to stream matches.
4. For each matched target, ask `solve` for its `ActionSpec`.
5. As soon as an action spec is complete enough to run, hand it to `engine`.
6. Continue solving more targets while the engine is already running ready work.

Important rule:

There must be no global "finish solving the whole graph first" barrier.

## 9. Query flow

`tofu query` should reuse the same `catalog` and `solve` facts.

Examples of queries:

- what matches this selector?
- which targets depend on this target?
- what provider binding does this target use?
- what action key would this target lower to?
- why is this target blocked?
- what lock domains exist?

Query should stop before `engine`.

No second graph, no separate architecture.

### 9.1 Query side-effect boundary

Query must never execute action runners.

By default, query should also avoid effectful external operations while solving
facts.

Query may:

- load and lower source packages
- evaluate pure source-language expressions
- inspect provider schemas and other metadata
- use provider functions that are explicitly marked query-safe

Query must not:

- invoke action runners
- perform resource CRUD or data-source execution
- call provider functions that are effectful or require external reads unless a
  separate opt-in mode explicitly allows it

If a fact depends on a non-query-safe operation, the solver should report it as
unknown or deferred rather than silently executing it during query.

In build mode, that same non-query-safe provider function should usually lower
into an explicit `provider.function` action rather than remaining hidden inside
solve-time evaluation.

## 10. Performance rules

These are architectural rules, not optional optimizations.

### 10.1 No global analysis barrier

The system must not require complete semantic analysis of the whole tree before
starting execution.

### 10.2 Cheap structural load

The first step must produce a structural catalog quickly:

- packages
- imports
- declarations
- source ranges
- provider declarations

This is enough for early selector expansion and many query operations.

### 10.3 Streaming selection

Broad selectors like `**` must stream matches rather than building a global
match list first.

### 10.4 Stable indexes

Catalog lookup must be indexed by identity, not implemented as repeated scans
through slices.

### 10.5 Explicit edge kinds

Do not collapse data deps, order deps, and lock domains into one undifferentiated
dependency list.

### 10.6 Action identity must be source-independent

Action keys should be built from semantic inputs such as:

- target kind/type
- driver version
- provider binding key
- canonical configuration
- other true data inputs

Action identity must not depend on source address if the system wants true
content-addressed sharing.

### 10.7 Query facts are first-class

If something matters for scheduling or debugging, it must be cheaply
inspectable.

## 11. Compatibility envelope from `images-private`

This architecture is motivated by real usage, not just theory.

The `images-private` workload implies support for:

- large module fanout
- heavy `for_each` and some `count`
- explicit `depends_on`, especially on modules
- provider alias passing across module boundaries
- provider functions
- many outputs and output wiring
- side-effecting resources and data sources
- compatibility shims for `null_resource`, `terraform_data`, `external`, and
  similar patterns where needed

This is why the design must have:

- strong lazy solving
- explicit order edges
- provider bindings as first-class facts
- fast time to first action

## 12. What not to do

These are the main architectural traps to avoid.

- Do not make the build path a thin layer on top of a plan/apply evaluator.
- Do not make HCL-specific packages or types the center of the architecture.
- Do not create one graph for build and another graph for query.
- Do not hide important scheduling facts only inside implicit value propagation.
- Do not collapse data dependencies, order dependencies, and lock domains into
  one generic edge type.
- Do not require whole-tree semantic analysis before the first action can run.
- Do not key actions by source address when semantic identity is intended.
- Do not let the engine import source-language packages.
- Do not let query silently execute non-query-safe provider operations.
- Do not introduce broad generic interfaces before there are multiple real
  implementations that need them.
- Do not treat side effects as a special-case escape hatch. They are part of
  the main model.

## 13. Implementation order

The bootstrap order should be:

1. Implement `task` and `digest`.
2. Build `selector`.
3. Implement `catalog` and `source/hcl` on top of existing config loading.
4. Implement `provider` session and binding logic, including aliases.
5. Implement `solve` for package expansion, target expansion, provider binding,
   config evaluation, and dependency discovery.
6. Implement `action` and `engine`.
7. Implement `tofu build` on top of `session + selector + solve + engine`.
8. Add `tofu query` on top of `session + selector + solve`.
9. Add `source/cue`.

That order gives a useful build path early without locking the architecture to
HCL.

## 14. Summary

The right architecture is:

- source-neutral at the core
- query-first
- lazy and streaming
- explicit about side effects
- explicit about action and edge semantics

In short:

```text
source -> catalog -> solve -> action -> engine
                 \-> query
```

That is the shape that best supports:

- `images-private`-scale workloads
- future growth
- fast time to first action
- future mixed HCL + CUE
- eventual source evolution beyond Terraform semantics
