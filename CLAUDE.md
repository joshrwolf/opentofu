# CLAUDE.md

## What This Is

This is a fork of [OpenTofu](https://github.com/opentofu/opentofu) used as a **build engine** for OCI container images at Chainguard. The `chofu-next` branch contains a clean-sheet build engine (`internal/chofu/`) that replaces the legacy graph walker with a three-phase pipeline optimized for the images-private workload: 1,881 modules expanding to ~66k resource instances.

The legacy engine takes 204s for a full build. chofu-next does it in 44s, and targeted builds (`-target=module.nginx`) complete in under 0.5s.

## Repository Structure

The build engine lives in `internal/chofu/`. Everything else is upstream OpenTofu.

```
internal/chofu/
  types.go      — ID, Vertex, Graph, Results (flat integer-indexed arrays)
  expand.go     — Phase 1: structural expansion (parallel sibling modules)
  graph.go      — Phase 2: DAG construction + target subgraph extraction
  walk.go       — Phase 3: bounded concurrent Kahn's walk
  eval.go       — lang.Data implementation backed by results array
  engine.go     — Build() entry point, OTel instrumented
  target.go     — TargetFilter (expansion pruning, graph filtering, walker filtering)
  providers.go  — concurrent schema fetch, provider config caching
  hash.go       — O(1) content hash index
  exec.go       — evalBuiltinResource orchestration (delegates to provider)
```

Built-in provider:
```
internal/builtin/providers/chofu/
  provider.go   — providers.Interface (schema-only, panics on Plan/Apply)
  schema.go     — chofu_exec resource schema
  exec.go       — RunExec: config parsing, command execution, output validation
```

Wiring into OpenTofu:
```
internal/backend/local/backend_build.go  — opBuild handler (bypasses localRun)
internal/backend/operation_type.go       — OperationTypeBuild
internal/command/build.go                — BuildCommand CLI
cmd/tofu/commands.go                     — "build" registration
internal/configs/configload/loader_load.go — LoadConfigWithWalker (target-aware loading)
internal/lang/eval.go                    — maps.Copy skip (copy-on-write for provider funcs)
internal/lang/scope.go                   — SharedFuncs field
```

## Building & Running

```bash
# Build the binary
go build -o /tmp/chofu ./cmd/tofu/

# Run against images-private (from the chofu/ worktree)
cd /path/to/images-private/chofu

# Full dry-run build
CHOFU_DRY_RUN=1 /tmp/chofu build

# Targeted build (one image)
CHOFU_DRY_RUN=1 /tmp/chofu build -target=module.nginx
```

## Profiling & Instrumentation

### CPU Profile

```bash
CHOFU_DRY_RUN=1 TOFU_CPU_PROFILE=/tmp/chofu.pprof /tmp/chofu build
go tool pprof -top -cum /tmp/chofu.pprof
```

`TOFU_CPU_PROFILE` is handled by main.go — don't add your own profiler.

### OTel Traces

Requires an OTel collector (e.g. ClickHouse + `otel-collector`):

```bash
CHOFU_DRY_RUN=1 \
  OTEL_TRACES_EXPORTER=otlp \
  OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 \
  OTEL_SERVICE_NAME=chofu \
  TF_LOG=INFO \
  /tmp/chofu build
```

Key spans: `chofu.Build`, `chofu.FetchSchemas`, `chofu.Expand`, `chofu.BuildGraph`, `chofu.Walk`, `chofu.EvalResource`, `chofu.EvalDataSource`, `chofu.EvalExec`, `chofu.ConfigureProvider`.

### Log-Based Phase Timings

With `TF_LOG=INFO`, grep for:

```bash
grep 'chofu' /tmp/stderr.log | grep 'INFO'
```

This gives timestamps for each phase boundary (schema fetch, expand, graph build, edges, target filter).

## images-private Workload

The validation target is the `chofu/` worktree of images-private. It uses `backend "inmem" {}` (overridden in `chofu/backend_override.tf`). The workload:

- 1,881 root module calls (one per image)
- Each image module has `module.build` (for_each), `module.test` (for_each), `module.tagger` (for_each)
- 92% of locals use `jsondecode(file("${path.module}/art.image.locks.json"))` to drive for_each
- Full expansion produces ~664k vertices, ~897k edges
- Targeted expansion of one image produces ~2k vertices

## Built-in Provider: `chofu`

Registered at `terraform.io/builtin/chofu` — in-process, no gRPC, no download. The engine intercepts `chofu_*` resources at eval time (via `v.ProviderAddr == chofuProviderAddr` in `executeVertex`) and delegates to the provider package for execution. The provider serves schemas; Plan/Apply are never called.

### `chofu_exec` — native command execution

Replaces `data "external"` and `null_resource` + `local-exec` patterns with a first-class build graph node. Design draws on REAPI idioms: declared outputs, content-addressed caching, structured capture.

```hcl
resource "chofu_exec" "example" {
  command     = ["sh", "-c", "art cache get"]
  env         = { CACHE_KEY = local.cache_key }     # nil env = inherit, non-nil = only declared
  stdin       = jsonencode({ key = "value" })        # optional, piped to command
  working_dir = path.module                          # optional
  timeout     = "30s"                                # optional, Go duration

  output_files = [                                   # optional, REAPI-style declared outputs
    { name = "result", path = "/tmp/output.json" },
  ]
}
```

Computed outputs (all paths, not content — avoids bloating the DAG with large values):
- `.stdout` — path to temp file containing captured stdout
- `.stderr` — path to temp file containing captured stderr
- `.exit_code` — integer, non-zero = hard error
- `.files["name"]` — path to declared output file (validated to exist after execution)

Downstream consumption: `file(chofu_exec.example.stdout)`, `jsondecode(file(...))`, or pass path to another exec.

### Adding new `chofu_*` types

1. Add schema to `internal/builtin/providers/chofu/schema.go` (returned by `GetProviderSchema`)
2. Add execution function to `internal/builtin/providers/chofu/` (e.g. `RunCache`)
3. Add case to the `switch resType` in `internal/chofu/exec.go:evalBuiltinResource`

No changes to types.go, graph.go, expand.go, eval.go, walk.go, or target.go — `chofu_*` resources are `KindResource` in the graph.

## Key Constraints

- `lang.Scope` + `lang.Data` is the expression evaluation interface — don't bypass it
- `instances.Expander` is thread-safe and handles all module/resource count/for_each algebra
- `configs.BuildConfig` + `ModuleWalker` is how the config tree is constructed — target filtering wraps the walker
- Provider functions (`provider::oci::parse`) are wired to real providers during the walk (see `providerFunction` in engine.go)
- Expansion cannot defer local evaluation — 92% of jsondecode locals drive for_each directly
