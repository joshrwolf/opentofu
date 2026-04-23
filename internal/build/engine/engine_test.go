// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/opentofu/opentofu/internal/build/digest"
)

func TestEngineUsesFileCacheAcrossEngines(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), ".tofu", "build", "cache")
	var calls atomic.Int32

	runner := RunnerFunc(func(_ context.Context, spec Spec) (RunResult, error) {
		calls.Add(1)
		return RunResult{Payload: []byte(spec.Name)}, nil
	})

	spec := testSpec("cached")
	spec.Cacheable = true

	first, err := New(Config{Runner: runner, Cache: NewFileCache(cacheDir)}).Run(t.Context(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if first.Cached {
		t.Fatal("did not expect first execution to be cached")
	}

	second, err := New(Config{Runner: runner, Cache: NewFileCache(cacheDir)}).Run(t.Context(), spec)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if !second.Cached {
		t.Fatal("expected second execution to hit cache")
	}
	if got, want := calls.Load(), int32(1); got != want {
		t.Fatalf("wrong runner call count: got %d want %d", got, want)
	}
	if diff := cmp.Diff(first.Record, second.Record); diff != "" {
		t.Fatalf("wrong cached record (-want +got):\n%s", diff)
	}
}

func TestEngineCoalescesConcurrentRuns(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})

	runner := RunnerFunc(func(_ context.Context, spec Spec) (RunResult, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return RunResult{Payload: []byte(spec.Name)}, nil
	})

	engine := New(Config{Runner: runner, Parallelism: 2})
	spec := testSpec("coalesced")

	var wg sync.WaitGroup
	results := make([]Result, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Go(func() {
			results[i], errs[i] = engine.Run(t.Context(), spec)
		})
	}

	<-started
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("unexpected error for run %d: %s", i, err)
		}
	}
	if got, want := calls.Load(), int32(1); got != want {
		t.Fatalf("wrong runner call count: got %d want %d", got, want)
	}
	if diff := cmp.Diff(results[0].Record, results[1].Record); diff != "" {
		t.Fatalf("wrong coalesced results (-want +got):\n%s", diff)
	}
}

func TestEngineWaitsForDependenciesAcrossSubmitOrder(t *testing.T) {
	depStarted := make(chan struct{}, 1)
	rootStarted := make(chan struct{}, 1)
	releaseDep := make(chan struct{})

	var mu sync.Mutex
	var order []string
	runner := RunnerFunc(func(_ context.Context, spec Spec) (RunResult, error) {
		mu.Lock()
		order = append(order, spec.Name)
		mu.Unlock()

		switch spec.Name {
		case "dep":
			depStarted <- struct{}{}
			<-releaseDep
		case "root":
			rootStarted <- struct{}{}
		}
		return RunResult{Payload: []byte(spec.Name)}, nil
	})

	engine := New(Config{Runner: runner, Parallelism: 2})
	dep := testSpec("dep")
	root := testSpec("root")
	root.ExecDeps = []digest.Digest{dep.Key}

	if _, err := engine.Submit(t.Context(), root); err != nil {
		t.Fatalf("unexpected submit error: %s", err)
	}

	select {
	case <-depStarted:
		t.Fatal("dependency should not execute before its action is submitted")
	case <-rootStarted:
		t.Fatal("root should not execute before dependency result is available")
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := engine.Submit(t.Context(), dep); err != nil {
		t.Fatalf("unexpected submit error: %s", err)
	}
	<-depStarted

	select {
	case <-rootStarted:
		t.Fatal("root should not execute before dependency completion")
	default:
	}

	close(releaseDep)
	if _, err := engine.Result(t.Context(), root.Key); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if diff := cmp.Diff([]string{"dep", "root"}, order); diff != "" {
		t.Fatalf("wrong execution order (-want +got):\n%s", diff)
	}
}

func TestEngineEnforcesLocks(t *testing.T) {
	var current atomic.Int32
	var max atomic.Int32

	runner := RunnerFunc(func(_ context.Context, spec Spec) (RunResult, error) {
		running := current.Add(1)
		for {
			prev := max.Load()
			if running <= prev || max.CompareAndSwap(prev, running) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		current.Add(-1)
		return RunResult{Payload: []byte(spec.Name)}, nil
	})

	engine := New(Config{Runner: runner, Parallelism: 2})
	a := testSpec("locked-a")
	b := testSpec("locked-b")
	a.Locks = []LockKey{"registry"}
	b.Locks = []LockKey{"registry"}

	var wg sync.WaitGroup
	for _, spec := range []Spec{a, b} {
		wg.Go(func() {
			if _, err := engine.Run(t.Context(), spec); err != nil {
				t.Errorf("unexpected error for %s: %s", spec.Name, err)
			}
		})
	}
	wg.Wait()

	if got, want := max.Load(), int32(1); got != want {
		t.Fatalf("wrong maximum concurrency for locked actions: got %d want %d", got, want)
	}
}

func TestEngineLockWaitDoesNotBurnParallelismSlot(t *testing.T) {
	holdStarted := make(chan struct{})
	freeStarted := make(chan struct{})
	releaseHold := make(chan struct{})

	runner := RunnerFunc(func(_ context.Context, spec Spec) (RunResult, error) {
		switch spec.Name {
		case "hold":
			close(holdStarted)
			<-releaseHold
		case "free":
			close(freeStarted)
		}
		return RunResult{Payload: []byte(spec.Name)}, nil
	})

	engine := New(Config{Runner: runner, Parallelism: 2})
	hold := testSpec("hold")
	hold.Locks = []LockKey{"shared"}
	waiter := testSpec("waiter")
	waiter.Locks = []LockKey{"shared"}
	free := testSpec("free")

	if _, err := engine.Submit(t.Context(), hold); err != nil {
		t.Fatalf("unexpected submit error: %s", err)
	}
	<-holdStarted

	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		if _, err := engine.Run(t.Context(), waiter); err != nil {
			t.Errorf("unexpected waiter error: %s", err)
		}
	}()

	time.Sleep(50 * time.Millisecond)

	freeDone := make(chan struct{})
	go func() {
		defer close(freeDone)
		if _, err := engine.Run(t.Context(), free); err != nil {
			t.Errorf("unexpected free error: %s", err)
		}
	}()

	select {
	case <-freeStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("unrelated runnable action did not start while another action was waiting on a lock")
	}

	close(releaseHold)
	<-waiterDone
	<-freeDone
}

func TestFileCacheCorruptEntryReturnsError(t *testing.T) {
	cache := NewFileCache(t.TempDir())
	key := digest.FromString("corrupt")
	path := cache.recordPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("failed to create cache dir: %s", err)
	}
	if err := os.WriteFile(path, []byte("not-json"), 0o644); err != nil {
		t.Fatalf("failed to write cache file: %s", err)
	}

	_, found, err := cache.Load(t.Context(), key)
	if found {
		t.Fatal("did not expect corrupt cache entry to load successfully")
	}
	if err == nil {
		t.Fatal("expected error for corrupt cache entry")
	}
}

func TestFileCacheMismatchedActionKeyReturnsError(t *testing.T) {
	cache := NewFileCache(t.TempDir())
	key := digest.FromString("expected")
	path := cache.recordPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("failed to create cache dir: %s", err)
	}

	src, err := json.Marshal(fileRecord{
		Version:   "build-cache-v1",
		ActionKey: digest.FromString("actual").String(),
		OutputKey: digest.FromString("out").String(),
		Payload:   base64.StdEncoding.EncodeToString([]byte("payload")),
	})
	if err != nil {
		t.Fatalf("failed to encode cache entry: %s", err)
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatalf("failed to write cache file: %s", err)
	}

	_, found, err := cache.Load(t.Context(), key)
	if found {
		t.Fatal("did not expect mismatched cache entry to load successfully")
	}
	if err == nil {
		t.Fatal("expected error for mismatched cache entry")
	}
}

func TestEngineResultBeforeSubmit(t *testing.T) {
	runner := RunnerFunc(func(_ context.Context, spec Spec) (RunResult, error) {
		return RunResult{Payload: []byte(spec.Name)}, nil
	})

	engine := New(Config{Runner: runner})
	spec := testSpec("late-submit")

	done := make(chan struct{})
	go func() {
		defer close(done)
		result, err := engine.Result(t.Context(), spec.Key)
		if err != nil {
			t.Errorf("unexpected error: %s", err)
			return
		}
		if string(result.Record.Payload) != "late-submit" {
			t.Errorf("wrong payload: %s", result.Record.Payload)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	if _, err := engine.Submit(t.Context(), spec); err != nil {
		t.Fatalf("unexpected submit error: %s", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Result did not unblock after Submit")
	}
}

func TestEngineSelfCycleDetection(t *testing.T) {
	runner := RunnerFunc(func(_ context.Context, spec Spec) (RunResult, error) {
		return RunResult{Payload: []byte(spec.Name)}, nil
	})

	engine := New(Config{Runner: runner})
	spec := testSpec("self-cycle")
	spec.ExecDeps = []digest.Digest{spec.Key}

	_, err := engine.Run(t.Context(), spec)
	if err == nil {
		t.Fatal("expected self-cycle error")
	}
	if got := err.Error(); !strings.Contains(got, "depends on itself") {
		t.Fatalf("wrong error: %s", got)
	}
}

func TestEnginePanicRecovery(t *testing.T) {
	runner := RunnerFunc(func(_ context.Context, spec Spec) (RunResult, error) {
		panic("runner exploded")
	})

	engine := New(Config{Runner: runner})
	spec := testSpec("panicker")

	result, err := engine.Run(t.Context(), spec)
	if err == nil {
		t.Fatal("expected error from panicking runner")
	}

	panicErr, ok := errors.AsType[*PanicError](err)
	if !ok {
		t.Fatalf("expected *PanicError, got %T: %s", err, err)
	}
	if panicErr.Name != "panicker" {
		t.Fatalf("wrong panic name: %s", panicErr.Name)
	}
	if panicErr.Value != "runner exploded" {
		t.Fatalf("wrong panic value: %v", panicErr.Value)
	}
	if len(panicErr.Stack) == 0 {
		t.Fatal("expected stack trace")
	}
	if result.Cached || result.Record.ActionKey != (digest.Digest{}) {
		t.Fatalf("expected zero result, got %+v", result)
	}
}

func TestEngineCallerCancellationDoesNotStopExecution(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	runner := RunnerFunc(func(_ context.Context, spec Spec) (RunResult, error) {
		close(started)
		<-release
		return RunResult{Payload: []byte("completed")}, nil
	})

	engine := New(Config{Runner: runner})
	spec := testSpec("cancel-wait")

	if _, err := engine.Submit(t.Context(), spec); err != nil {
		t.Fatalf("unexpected submit error: %s", err)
	}
	<-started

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := engine.Result(ctx, spec.Key)
	if err == nil {
		t.Fatal("expected cancellation error")
	}

	close(release)

	result, err := engine.Result(t.Context(), spec.Key)
	if err != nil {
		t.Fatalf("unexpected error after execution completed: %s", err)
	}
	if string(result.Record.Payload) != "completed" {
		t.Fatalf("wrong payload: %s", result.Record.Payload)
	}
}

func testSpec(name string) Spec {
	return Spec{
		Key:   digest.FromString(name),
		Name:  name,
		Class: "test",
	}
}
