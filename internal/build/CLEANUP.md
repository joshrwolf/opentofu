# Build System Cleanup Tasks

Findings from a deep review of `internal/build/`. Ordered by priority.
Items 1–6 are mechanical slop cleanup. Items 7–10 are design improvements.

## Mechanical Cleanup

### 1. Remove dead `providerFunctions` field from `targetAnalysis`

`hcl/eval_target.go` — the `targetAnalysis` struct has a `providerFunctions []ProviderFunctionRef` field and an `appendProviderFunction` method that populates it. The `result()` method never includes this field in the returned `EvalResult`. The slice is accumulated but never read. The `providerSeen` map alone handles dedup. Delete the field, the method, and the slice.

### 2. Deduplicate module-prefix rewriting

`hcl/eval_output.go` defines `rewriteAddrModulePrefix` and `rewriteExplicitRefsForModule`. `solve/deps.go` defines `rewriteModulePrefix` and `rewriteExplicitDeps`. These are the same function duplicated across packages — they operate on `catalog.Addr` and `catalog.ModulePath`. Move one canonical implementation to `catalog/` and delete the copies.

### 3. Inline `runtimeLookupForTarget`

`hcl/eval_target.go` — `runtimeLookupForTarget` is a one-liner that calls `runtimeLookupForModule(addr.Module, resolver)`. It has a single call site in `hcl/decode.go`. Inline it and delete the function.

### 4. Make `DefaultQuerySafetyRegistry` / `DefaultQueryFunctionResolver` true singletons

`provider/querysafe.go` and `provider/functions.go` — these are called 8+ times across `hcl/eval_target.go`, `hcl/eval_output.go`, `hcl/eval_instances.go`, `hcl/eval_provider.go`, `hcl/eval_options.go`, `hcl/decode.go`, and `solve/value_deps.go`. Each call allocates a new instance. Use a package-level `var` with `sync.Once` so they're constructed once.

### 5. Cache `defaultExecEnvironment()` at package level

`hcl/exec.go` — this function copies the entire process environment into a new `map[string]string` on every call. Called from `compat_null_resource.go` and `compat_external.go`. Process environment doesn't change. Compute once with `sync.Once`.

### 6. Remove redundant `Context` field from `unsupportedDiag`

`hcl/validator.go` — the `unsupportedDiag` helper sets both `Subject` and `Context` to the same `hcl.Range`. `Context` is supposed to be the broader enclosing range. Setting it to the same value as `Subject` is noise. Drop `Context`.

## Design Improvements

### 7. Add a flag to disable compat code

`hcl/compat_null_resource.go` and `hcl/compat_external.go` — the `isNullResourceCompatibilityTarget` and `isExternalCompatibilityTarget` checks are scattered across `eval_target.go`, `validator.go`, `catalog.go`, and `compat_external.go`. There's no single switch to disable them. Add a flag on `Loaded` (or a build tag) so null_resource/external compat can be turned off in one place when we're ready to drop it. This also makes it easy to verify no other code paths depend on them.

### 8. Fix `PreparedScope.Evaluate` fallback to not retry on genuine errors

`configs/prepared_scope.go` — the `Evaluate` method falls back to the standard `EvaluateWithOptions` path whenever `expr.Value(hclCtx)` returns any error. This is correct for "the prepared context was missing a variable" but wasteful for genuine evaluation errors (type mismatch, division by zero, etc.) where the retry produces the same error. Distinguish "incomplete context" from "expression invalid" before retrying.

### 9. Consolidate caching on `Loaded`

`hcl/loader.go` — the `Loaded` struct has 6 independent mutexes and 6 cache maps (`moduleEvalCache`, `targetRepetitionCache`, `importRepetitionCache`, `evalCacheValues`, `purityAnalysis`, `preparedScopeCache`). These were added incrementally. Audit whether any caches overlap, whether `purityAnalysis` and `preparedScopeCache` (both read-heavy, rarely written) can share a lock, and whether the defensive cloning in `cloneCachedRepetitionEval` is necessary (diagnostics are immutable once produced).

### 10. Unify `buildTargetCollectionValue` / `buildModuleCollectionValue`

`solve/runtime.go` — these two functions share ~85% identical logic (the Single/Optional/List/Map shape handling). They differ only in how they extract the sort key from their items (`catalog.Addr.Key` vs `catalog.ModulePath.LastStep().Key`). Extract a generic `buildCollectionValue` that takes a key-extraction function, and delete the duplication. Any bug fix currently needs to be applied twice.
