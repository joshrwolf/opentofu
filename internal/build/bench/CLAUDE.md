# internal/build/bench — benchmark + e2e harness

This package runs the build pipeline end-to-end against a synthetic project on
disk. It is the ground truth for perf work on the `internal/build/*` stack
(loader → catalog → solver → memo → engine → executor) and it also drives the
txtar-based e2e regression suite.

## What we're modeling

The target workload is the **chainguard/images-private** repo: ~1,760 leaf
modules, each one declaring `for_each` over a JSON lockfile, invoking
`provider::oci::parse`/`get` provider functions, emitting a summary output, and
depending on a small tree of sub-modules (`build/`, `tests/`, `tagger/`). A
fraction use aliased providers via `configuration_aliases`. The root module
aggregates per-leaf summaries.

The shape is fixed; only the leaf count varies across sizes. Providers are
synthetic no-ops (`commandtesting.NewProvider` + a fake OCI resolver), so
every number is pure evaluator/solver/loader time — **no network, no real
provider work**.

## The four sizes

| tier   | leaves | use                                                     |
|--------|--------|---------------------------------------------------------|
| tiny   | ~10    | smoke / unit tests; also the sanity benchmarks in CI    |
| fast   | ~200   | iteration target for perf work; runs in a few seconds   |
| medium | ~500   | interstitial datapoint for pinning scaling exponents    |
| real   | ~1920  | ground truth, calibrated to images-private              |

All four share identical per-leaf shape (`RepeatKeys=16`, `AliasModuleEvery=256`,
etc. — see `baseTopology` in `bench_test.go`). Perf work should iterate on
`fast` and occasionally validate against `real` to confirm the delta ratio
holds. If an improvement at `fast` doesn't translate to `real`, something
about the workload only shows up at scale — investigate, don't ignore.

## Running

```bash
# One iteration of one size, with profiles and JSON export.
mkdir -p /tmp/bench
TOFU_BUILD_BENCH_EXPORT_DIR=/tmp/bench go test ./internal/build/bench \
  -run '^$' \
  -bench 'BenchmarkBuild/fast$' \
  -benchtime=1x \
  -timeout=60m \
  -cpuprofile=/tmp/bench/cpu.out \
  -memprofile=/tmp/bench/mem.out \
  -trace=/tmp/bench/trace.out
```

- `BenchmarkBuild/<size>` — full execute path, selector `output.*`.
- `BenchmarkQuery/<size>` — inspector-only path, selector `**`.
- `-benchtime=1x` is the default for real-scale work (one iteration is plenty).
- `TOFU_BUILD_BENCH_EXPORT_DIR` writes one JSON summary per sub-benchmark.

Real scale currently takes ~24 s. Fast ~2.5 s. Medium ~7 s. Tiny ~150 ms.

## Key metrics (in `go test` output and export JSON)

| metric                 | what it means                                                  |
|------------------------|----------------------------------------------------------------|
| `ns/load`              | `build.Load` span: HCL parse + catalog build                   |
| `ns/new_engine`        | `build.NewEngine` span: engine + solver construction           |
| `ns/execute`           | `build.Execute` span: solver + action submission + wait        |
| `ns/first_submit`      | wall time from `Run` start to the first `build.submit` event   |
| `ns/first_dispatch`    | wall time to the first `dispatch.execute` span start           |
| `ns/first_runner`      | wall time to the first `build.Runner.Run` span start           |
| `submitted`/`executed` | action counts from `dispatch.execute` spans                    |
| `memo_gets`/`computed` | per-query memo hit counters (sums across 14 queries)           |
| `heap_alloc_bytes`     | live heap at end of run                                        |
| `total_alloc_bytes`    | cumulative allocations during the run — GC-pressure proxy      |
| `gc_cycles`            | GC cycles during the run                                       |

**The headline number for perf work is `ns/first_submit`** — time from "go"
to the first action reaching the engine queue. Target is single-digit
seconds at real scale; current state depends on the tree.

## Files

```
run.go           shared Runner harness (used by bench + txtar)
bench_test.go    topology, generator, export, bench-specific span helpers,
                 BenchmarkBuild/BenchmarkQuery, sanity tests
txtar_test.go    e2e txtar-driven regression tests
testdata/*.txtar  e2e test cases with golden outputs
```

`run.go` is the only non-test file. Everything bench-specific (topology,
generator, benchmarks, metrics) lives inline in `bench_test.go` by design —
it used to be scattered across 9 files and nobody could follow it.

## How to read a profile

`go tool pprof -top -nodecount=15 /tmp/bench/cpu.out` — flat CPU.
`go tool pprof -top -cum -nodecount=15 /tmp/bench/cpu.out` — cumulative.
`go tool pprof -alloc_space -top /tmp/bench/mem.out` — who allocates bytes.
`go tool pprof -alloc_objects -top /tmp/bench/mem.out` — who allocates objects.
`go tool trace -pprof=sched /tmp/bench/trace.out > sched.pprof; go tool pprof -top sched.pprof` — scheduler delay (lock/channel contention ends up here).
`go tool trace -pprof=sync /tmp/bench/trace.out > sync.pprof; go tool pprof -top sync.pprof` — blocked-on-channel/mutex time.

**If wall time is flat but CPU utilization is low and scheduler delay is high,
the bottleneck is synchronization** — mutex contention or channel backpressure.
Look at the `sync.(*RWMutex).Unlock` / `sync.(*Mutex).Unlock` flat lines in
the sched profile.

**If CPU utilization is high and GC functions (`runtime.scanObject*`,
`runtime.madvise`, `runtime.tryDeferToSpanScan`) dominate the flat CPU
profile, the bottleneck is allocation pressure.** Look at `-alloc_space` to
find the source.

## Known architecture hotspots (as of 2026-04-24)

- `lang.Scope.evalContext` + `evalVarBuilder.putValueBySubject` — ~68% of
  cumulative allocations. EvalContext is rebuilt from scratch for every HCL
  expression evaluation.
- `memo.Query.getSlot` is the hot call tree for the solver. The index-map
  lock (`indexMu`) was sharded in one recent pass; the per-slot mutex is now
  the next-largest contention point but only matters on cache-miss paths.
- `engine.run` calls `db.Advance()` on every action completion, cascading
  Layer 3 memo recomputes. At real scale with ~92k actions this drives
  substantial churn and is the reason lock-contention wins don't always
  translate to wall-time wins at scale.
- `jsondecode(file("locks.json"))` decoded many times even though the value
  cache (`configs.EvalCache`) should catch it — worth verifying hit rate.
- `build/hcl/eval_instances.go:cloneForEachValues` does a defensive
  map-copy on every cache read (~900 MB at real).

## When "fast" and "real" disagree

The scaling exponent from `tiny → fast → medium → real` should be roughly
linear or sub-linear if the pipeline is healthy. If a specific size jumps
(e.g., real grows 3× faster than the `medium → real` leaf-count ratio would
predict), an O(N²) data structure or a cache whose miss rate climbs with N
just got exposed. Add datapoints between medium and real if you need to pin
where the cliff is.

Conversely, a change that helps `fast` but not `real` usually means it
removed a bottleneck the real run wasn't hitting first, or it unlocked more
concurrency that the real run's *second* bottleneck can't absorb. Profile
real to find the new leader before declaring victory.
