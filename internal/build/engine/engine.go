// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/tracing"
)

type Config struct {
	Context     context.Context
	Parallelism int
	Runner      Runner
	Cache       Cache
}

type Engine struct {
	ctx    context.Context
	runner Runner
	cache  Cache
	sem    chan struct{}

	mu      sync.Mutex
	actions map[digest.Digest]*action
	running int

	lockMu   sync.Mutex
	lockCond *sync.Cond
	locks    map[LockKey]digest.Digest
}

type action struct {
	started bool
	done    chan struct{}
	result  Result
	err     error
}

func (a *action) wait(ctx context.Context) (Result, error) {
	select {
	case <-a.done:
		return a.result, a.err
	case <-ctx.Done():
		return Result{}, fmt.Errorf("build engine execution was canceled: %w", ctx.Err())
	}
}


type PanicError struct {
	Key   digest.Digest
	Name  string
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("build action %q panicked: %v", e.Name, e.Value)
}

func New(cfg Config) *Engine {
	ctx := cfg.Context
	if ctx == nil {
		ctx = context.Background()
	}
	parallelism := cfg.Parallelism
	if parallelism <= 0 {
		parallelism = runtime.GOMAXPROCS(0)
		if parallelism <= 0 {
			parallelism = 1
		}
	}

	ret := &Engine{
		ctx:     ctx,
		runner:  cfg.Runner,
		cache:   cfg.Cache,
		sem:     make(chan struct{}, parallelism),
		actions: make(map[digest.Digest]*action),
		locks:   make(map[LockKey]digest.Digest),
	}
	ret.lockCond = sync.NewCond(&ret.lockMu)
	return ret
}

func (e *Engine) Submit(_ context.Context, spec Spec) (bool, error) {
	if spec.Key == (digest.Digest{}) {
		return false, errors.New("build action has no key and cannot be submitted to the engine")
	}

	e.mu.Lock()
	a := e.actions[spec.Key]
	if a == nil {
		a = &action{done: make(chan struct{})}
		e.actions[spec.Key] = a
	}
	if a.started {
		e.mu.Unlock()
		return false, nil
	}
	a.started = true
	e.running++
	e.mu.Unlock()

	go e.run(a, spec)
	return true, nil
}

func (e *Engine) Run(ctx context.Context, spec Spec) (Result, error) {
	if _, err := e.Submit(ctx, spec); err != nil {
		return Result{}, err
	}
	return e.Result(ctx, spec.Key)
}

func (e *Engine) Result(ctx context.Context, key digest.Digest) (Result, error) {
	if key == (digest.Digest{}) {
		return Result{}, errors.New("build action result lookup requires a non-zero action key")
	}
	return e.getAction(key).wait(ctx)
}


func (e *Engine) getAction(key digest.Digest) *action {
	e.mu.Lock()
	defer e.mu.Unlock()

	if a := e.actions[key]; a != nil {
		return a
	}

	a := &action{done: make(chan struct{})}
	e.actions[key] = a
	return a
}

func (e *Engine) run(a *action, spec Spec) {
	defer func() {
		e.mu.Lock()
		e.running--
		e.mu.Unlock()
	}()
	defer close(a.done)
	defer func() {
		if r := recover(); r != nil {
			a.err = &PanicError{
				Key:   spec.Key,
				Name:  spec.Name,
				Value: r,
				Stack: debug.Stack(),
			}
		}
	}()
	a.result, a.err = e.execute(spec)
}

func (e *Engine) execute(spec Spec) (Result, error) {
	ctx := e.ctx

	ctx, span := tracing.Tracer().Start(ctx, "dispatch.execute",
		tracing.SpanAttributes(
			attribute.String("build.action.key", spec.Key.String()),
			attribute.String("build.action.name", spec.Name),
			attribute.String("build.action.runner", string(spec.Runner.Kind)),
			attribute.Bool("build.action.cacheable", spec.Cacheable),
			attribute.Int("build.action.exec_deps", len(spec.ExecDeps)),
		),
	)
	defer span.End()

	if spec.Cacheable && !spec.Volatile && e.cache != nil {
		record, ok, _ := e.cache.Load(ctx, spec.Key)
		if ok {
			span.SetAttributes(attribute.Bool("build.action.cached", true))
			return Result{Record: record, Cached: true}, nil
		}
	}

	if err := e.waitForDependencies(ctx, spec); err != nil {
		tracing.SetSpanError(span, err)
		return Result{}, err
	}

	releaseRunState, err := e.acquireRunState(ctx, spec)
	if err != nil {
		tracing.SetSpanError(span, err)
		return Result{}, err
	}
	defer releaseRunState()

	if e.runner == nil {
		err := errors.New("build engine has no action runner configured")
		tracing.SetSpanError(span, err)
		return Result{}, err
	}

	runResult, err := e.runner.Run(ctx, spec)
	if err != nil {
		tracing.SetSpanError(span, err)
		return Result{}, err
	}

	record := Record{
		ActionKey: spec.Key,
		OutputKey: runResult.OutputKey,
		Payload:   slices.Clone(runResult.Payload),
		Volatile:  spec.Volatile,
	}
	if record.OutputKey == (digest.Digest{}) {
		record.OutputKey = digest.FromBytes(record.Payload)
	}

	if spec.Cacheable && !record.Volatile && e.cache != nil {
		_ = e.cache.Store(ctx, record)
	}

	span.SetAttributes(attribute.Bool("build.action.cached", false))
	return Result{Record: record}, nil
}

func (e *Engine) waitForDependencies(ctx context.Context, spec Spec) error {
	deps := make([]digest.Digest, 0, len(spec.ExecDeps)+len(spec.After))
	deps = append(deps, spec.ExecDeps...)
	deps = append(deps, spec.After...)
	for _, dep := range deps {
		if dep == spec.Key {
			return fmt.Errorf("build action %s depends on itself", spec.Key)
		}
		if _, err := e.Result(ctx, dep); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) acquireSlot(ctx context.Context) (func(), error) {
	select {
	case e.sem <- struct{}{}:
		return func() { <-e.sem }, nil
	case <-ctx.Done():
		return func() {}, errors.New("build engine execution was canceled while waiting for an execution slot")
	}
}

func (e *Engine) acquireRunState(ctx context.Context, spec Spec) (func(), error) {
	if len(spec.Locks) == 0 {
		return e.acquireSlot(ctx)
	}

	keys := slices.Clone(spec.Locks)
	slices.Sort(keys)
	keys = slices.Compact(keys)

	stop := context.AfterFunc(ctx, func() {
		e.lockCond.Broadcast()
	})
	defer stop()

	for {
		e.lockMu.Lock()
		if ctx.Err() != nil {
			e.lockMu.Unlock()
			return func() {}, errors.New("build engine execution was canceled while waiting for action locks")
		}
		if !e.locksAvailable(keys) {
			e.lockCond.Wait()
			e.lockMu.Unlock()
			continue
		}
		e.lockMu.Unlock()

		releaseSlot, err := e.acquireSlot(ctx)
		if err != nil {
			return func() {}, err
		}

		e.lockMu.Lock()
		if ctx.Err() != nil {
			e.lockMu.Unlock()
			releaseSlot()
			return func() {}, errors.New("build engine execution was canceled while waiting for action locks")
		}
		if !e.locksAvailable(keys) {
			e.lockMu.Unlock()
			releaseSlot()
			continue
		}
		for _, key := range keys {
			e.locks[key] = spec.Key
		}
		e.lockMu.Unlock()

		return func() {
			e.lockMu.Lock()
			for _, key := range keys {
				delete(e.locks, key)
			}
			e.lockMu.Unlock()
			releaseSlot()
			e.lockCond.Broadcast()
		}, nil
	}
}

func (e *Engine) locksAvailable(keys []LockKey) bool {
	for _, key := range keys {
		if _, ok := e.locks[key]; ok {
			return false
		}
	}
	return true
}
