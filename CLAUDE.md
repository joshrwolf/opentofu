# Chofu — OpenTofu fork for build-system workloads

Chofu is an OpenTofu fork purpose-built as a build system for OCI container images. It replaces the plan/apply lifecycle with a single-pass, content-hash-cached execution model.

Primary branch: `chofu`

## Architecture

### Single-pass build execution

`Context.Build()` walks the resource graph once. For each resource: evaluate config, compute content hash, check state — if hash matches, skip; if not, call `ApplyResourceChange` directly. No `PlanResourceChange`, no refresh, no two-phase plan+apply.

### State is the cache

Content hashes are stored on each resource instance in state (`ContentHash` and `CachedAt` fields on `ResourceInstanceObjectSrc`). The backend persists state between runs. No separate cache store — the state file IS the cache. The V4 JSON serialization is backward-compatible (`omitempty`).

### Content hash cascade

`ComputeContentHash` hashes: resource type + evaluated config value + content hashes of all referenced resources. Changes cascade automatically — if an upstream resource re-executes and produces new output, all downstream hashes change because the evaluated config includes the new values. Dependency hashes are collected from `References()` (all config-derived references, not just `depends_on`).

### DAG scoping

Three composable mechanisms for restricting which resources execute:

- **`-target=module.nginx`** — Resource/module targeting via `TargetingTransformer`. Primary way to scope builds within the full DAG.
- **`-o=image_ref`** — Output targeting via `OutputTargetTransformer`. Prunes graph to backward closure of named root outputs.
- **`--skip=test` / `--only=build`** — Role filtering at execution time. Providers declare per-resource-type roles via optional `BuildMetaProvider` interface. Skipped resources no-op (graph ordering preserved).

### Provider metadata

Providers optionally implement `BuildMetaProvider` to declare `ResourceRole` (build/test/publish) and `CachePolicy` (CacheByInputs/CacheWithTTL/NeverCache) per resource type. Default is `CacheByInputs` — deterministic, same inputs = reuse cached output. The engine decides whether to execute; providers are pure executors.

## Fork

### What we changed and why

| What | Why |
|------|-----|
| Added `walkBuild` operation + `BuildGraphBuilder` + `NodeBuildableResourceInstance` | Single-pass execution — providers weren't designed for PlanResourceChange; plan phase is pure overhead for deterministic builds |
| Added `ContentHash`/`CachedAt` to state model | State IS the cache — no separate cache store, no cache.tf hacks |
| Eliminated refresh from the build path | No infrastructure drift in builds — if state says it's built, it's built |
| Added `OutputTargetTransformer` | Target outputs, not phases — "give me image_ref" is better than "run the build phase" |
| Added `BuildMetaProvider` optional interface on providers | Role + cache policy metadata without changing the core provider protocol |
| Added `OperationTypeBuild` + `opBuild` + `build` CLI command | Full plumbing from CLI through backend to engine |
| Added orphan cleanup in `Build()` | Persistent state needs pruning as resources are added/removed over time |
| Added `outputValueAddr()` to `nodeExpandOutput` | Enables output targeting via type assertion instead of string parsing |

### What we kept

The DAG engine, expression evaluator, HCL config language, provider plugin protocol, state file format (V4), graph walk infrastructure, module expansion, variable/local/output evaluation, backend/state-manager abstractions, and existing plan/apply/destroy paths are all untouched.

### Design principles

1. **State is the cache** — one source of truth. No separate cache store.
2. **Targets, not phases** — scope via graph pruning, not by generating different configs for "build" vs "test" vs "tag."
3. **The backend is the persistence interface** — today it's local JSON, tomorrow sqlite or sharded. Keep this abstraction clean.
4. **Providers are pure executors** — the engine decides whether to execute based on content hashes. Providers just do the work when asked.

### Key areas of the codebase we touch

- `internal/tofu/` — build execution engine: context, graph builder, node types, content hash, output targeting
- `internal/states/` — ContentHash/CachedAt fields threaded through object model, serialization, deep copy
- `internal/providers/` — BuildMetaProvider interface, ResourceRole, CachePolicy
- `internal/backend/` — OperationTypeBuild, opBuild handler, build backend
- `internal/command/` — build CLI command

## Building and testing

```bash
go build ./...
go test ./internal/tofu/ -run 'TestContextBuild|TestComputeContentHash'
go test ./internal/command/ -run TestBuild
go test ./internal/providers/ -run TestGetResourceMeta
go test ./internal/backend/remote-state/build/
```

## Future work

- **Compiled-in providers** — eliminate `tofu init` by compiling chainguard providers into the binary
- **Recipe-based hashing** — hash config source + dependency hashes before evaluating expressions, enabling O(1) cache checks (viable once `file()` hacks are replaced with proper DAG resources)
- **Efficient state backend** — sqlite or sharded storage for full-DAG scale (17,000+ resources)
- **First-class package resolution** — replace `art lock` + `art.image.locks.json` + `jsondecode(file(...))` with DAG resources
- **`--dry-run` mode** — show cache hit/miss per resource without executing
- **Build-specific views** — streaming provider output, test summaries, cache reporting
