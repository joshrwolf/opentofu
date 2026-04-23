// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package dice

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

type execution[V any] struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	done    chan struct{}
	waiters int
	value   V
	err     error
}

var errAbandoned = errors.New("dice execution abandoned")
var errInfrastructure = errors.New("dice infrastructure error")

type waitHandle struct {
	stop     func() bool
	detached atomic.Bool
}

type evalFrame struct {
	name     string
	ref      depRef
	parent   *evalFrame
	recorder *depRecorder // nil during verification
}

type evalFrameKey struct{}

func currentFrame(ctx context.Context) *evalFrame {
	f, _ := ctx.Value(evalFrameKey{}).(*evalFrame)
	return f
}

func pushFrame(ctx context.Context, ref depRef, name string, recorder *depRecorder) context.Context {
	return context.WithValue(ctx, evalFrameKey{}, &evalFrame{
		name:     name,
		ref:      ref,
		parent:   currentFrame(ctx),
		recorder: recorder,
	})
}

func (f *evalFrame) hasCycle(ref depRef) bool {
	for cur := f; cur != nil; cur = cur.parent {
		if cur.ref == ref {
			return true
		}
	}
	return false
}

func (f *evalFrame) cycleError(name string) error {
	names := []string{name}
	for cur := f; cur != nil; cur = cur.parent {
		names = append(names, cur.name)
	}
	slices.Reverse(names)
	return fmt.Errorf("cycle in computation graph: %q depends on itself through %s: %w",
		name, strings.Join(names, " -> "), errInfrastructure)
}

func (f *evalFrame) recordDep(d depRef) {
	if f != nil && f.recorder != nil {
		f.recorder.record(d)
	}
}

type depRecorder struct {
	mu   sync.Mutex
	deps []depRef
}

func (r *depRecorder) record(d depRef) {
	r.mu.Lock()
	r.deps = append(r.deps, d)
	r.mu.Unlock()
}

func (r *depRecorder) collect() []depRef {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.deps)
}

func cancellationError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("dice canceled: %w: %w", err, errInfrastructure)
}

func panicError(r any) error {
	if err, ok := r.(error); ok {
		return fmt.Errorf("dice computation panicked: %w", err)
	}
	return fmt.Errorf("dice computation panicked: %v", r)
}

func (c *Computation[K, A, V]) ref(e *entry[A, V]) depRef {
	return depRef{query: c.id, slot: e.id}
}

func (c *Computation[K, A, V]) getSlot(ctx context.Context, e *entry[A, V]) (V, error) {
	s := &e.slot
	ref := c.ref(e)
	frame := currentFrame(ctx)

	for {
		// 1. Fast path: verified at current revision (lock-free).
		rev := c.session.revision()
		if s.verifiedAt.Load() == rev {
			if snap := s.snap.Load(); snap != nil {
				c.counters.fastPath.Add(1)
				frame.recordDep(ref)
				return snap.value, snap.err
			}
		}

		// 2. Cycle detection.
		if frame.hasCycle(ref) {
			var zero V
			return zero, frame.cycleError(c.slotLabel(e))
		}

		// 3. Caller cancellation.
		if err := ctx.Err(); err != nil {
			var zero V
			return zero, cancellationError(err)
		}

		// 4. Lock slot.
		s.mu.Lock()

		// 5. Double-check under lock.
		rev = c.session.revision()
		if s.verifiedAt.Load() == rev {
			if snap := s.snap.Load(); snap != nil {
				s.mu.Unlock()
				c.counters.fastPath.Add(1)
				frame.recordDep(ref)
				return snap.value, snap.err
			}
		}

		snap := s.snap.Load()

		// 6. Verify deps if previously computed and no active execution.
		if snap != nil && snap.changedAt > 0 && s.exec == nil {
			deps := snap.deps
			since := s.verifiedAt.Load()
			pinnedRev := rev
			s.mu.Unlock()

			verifyCtx := pushFrame(ctx, ref, c.slotLabel(e), nil)
			valid, verifyErr := c.session.verifyDeps(verifyCtx, deps, since)

			if verifyErr != nil {
				var zero V
				return zero, verifyErr
			}

			if valid {
				s.mu.Lock()
				if s.snap.Load() == snap && s.exec == nil {
					s.verifiedAt.Store(pinnedRev)
					s.mu.Unlock()
					c.counters.verified.Add(1)
					frame.recordDep(ref)
					return snap.value, snap.err
				}
				s.mu.Unlock()
				continue
			}

			s.mu.Lock()
			if s.snap.Load() != snap || s.exec != nil {
				s.mu.Unlock()
				continue
			}
			v, err, retry := c.compute(ctx, e, snap, rev)
			if retry {
				continue
			}
			c.counters.computed.Add(1)
			frame.recordDep(ref)
			return v, err
		}

		// 7. Coalescing: wait on active execution.
		if s.exec != nil {
			exec := s.exec
			exec.waiters++
			s.mu.Unlock()
			v, err := c.wait(ctx, s, exec)
			if err != nil && ctx.Err() == nil && errors.Is(err, errAbandoned) {
				continue
			}
			c.counters.coalesced.Add(1)
			frame.recordDep(ref)
			return v, err
		}

		// 8. Caller cancellation before compute.
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			var zero V
			return zero, cancellationError(err)
		}

		// 9. Compute.
		v, err, retry := c.compute(ctx, e, snap, rev)
		if retry {
			continue
		}
		c.counters.computed.Add(1)
		frame.recordDep(ref)
		return v, err
	}
}

// Called with e.slot.mu held; always releases it before returning.
func (c *Computation[K, A, V]) compute(ctx context.Context, e *entry[A, V], snap *result[V], rev uint64) (V, error, bool) {
	s := &e.slot
	pinnedRev := max(c.session.revision(), rev)

	execCtx, cancel := context.WithCancelCause(c.session.ctx)
	exec := &execution[V]{
		ctx:     execCtx,
		cancel:  cancel,
		done:    make(chan struct{}),
		waiters: 1,
	}
	if frame := currentFrame(ctx); frame != nil {
		exec.ctx = context.WithValue(exec.ctx, evalFrameKey{}, frame)
	}
	s.exec = exec
	s.mu.Unlock()

	handle := c.detachOnCancel(ctx, s, exec)

	defer func() {
		if r := recover(); r != nil {
			handle.stop()
			s.clearExec(exec)
			pErr := panicError(r)
			exec.cancel(pErr)
			exec.err = pErr
			close(exec.done)
			panic(r)
		}
	}()

	value, err, deps := c.call(exec, e)
	handle.stop()

	cause := context.Cause(exec.ctx)
	if cause != nil {
		s.clearExec(exec)

		var zero V
		exec.value = zero
		exec.err = cancellationError(cause)
		close(exec.done)

		if c.session.ctx.Err() != nil {
			return zero, cancellationError(cause), false
		}
		if handle.detached.Load() && ctx.Err() != nil {
			return zero, cancellationError(ctx.Err()), false
		}
		return zero, nil, true // retry (abandonment)
	}

	if c.eq != nil && snap != nil && snap.changedAt > 0 && err == nil && snap.err == nil && c.eq(value, snap.value) {
		s.snap.Store(&result[V]{
			value:     snap.value,
			err:       nil,
			deps:      deps,
			changedAt: snap.changedAt,
		})
		s.verifiedAt.Store(pinnedRev)
		s.clearExec(exec)
		exec.value = snap.value
		exec.err = nil
		close(exec.done)
		return snap.value, nil, false
	}

	if err == nil || !errors.Is(err, errInfrastructure) {
		s.snap.Store(&result[V]{
			value:     value,
			err:       err,
			deps:      deps,
			changedAt: pinnedRev,
		})
		s.verifiedAt.Store(pinnedRev)
	}
	s.clearExec(exec)
	exec.value = value
	exec.err = err
	close(exec.done)
	return value, err, false
}

func (c *Computation[K, A, V]) call(exec *execution[V], e *entry[A, V]) (V, error, []depRef) {
	recorder := &depRecorder{}
	execCtx := pushFrame(exec.ctx, c.ref(e), c.slotLabel(e), recorder)
	v, err := c.fn(execCtx, e.arg)
	return v, err, recorder.collect()
}

func (c *Computation[K, A, V]) wait(ctx context.Context, s *slot[V], exec *execution[V]) (V, error) {
	select {
	case <-exec.done:
		return exec.value, exec.err
	case <-ctx.Done():
		select {
		case <-exec.done:
			return exec.value, exec.err
		default:
		}
		if !c.detach(s, exec) {
			<-exec.done
			return exec.value, exec.err
		}
		var zero V
		return zero, cancellationError(ctx.Err())
	}
}

func (c *Computation[K, A, V]) detachOnCancel(ctx context.Context, s *slot[V], exec *execution[V]) *waitHandle {
	if ctx.Done() == nil {
		return &waitHandle{stop: func() bool { return true }}
	}
	handle := &waitHandle{}
	handle.stop = context.AfterFunc(ctx, func() {
		if c.detach(s, exec) {
			handle.detached.Store(true)
		}
	})
	return handle
}

func (c *Computation[K, A, V]) detach(s *slot[V], exec *execution[V]) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exec != exec {
		return false
	}
	exec.waiters--
	if exec.waiters == 0 {
		exec.cancel(errAbandoned)
	}
	return true
}

func (c *Computation[K, A, V]) changedAfterSlot(ctx context.Context, id slotID, since uint64) (bool, error) {
	e := c.entries.at(id)
	s := &e.slot

	if s.verifiedAt.Load() == c.session.revision() {
		if snap := s.snap.Load(); snap != nil {
			return snap.changedAt > since, nil
		}
	}

	_, getErr := c.getSlot(ctx, e)

	if s.verifiedAt.Load() == c.session.revision() {
		if snap := s.snap.Load(); snap != nil {
			return snap.changedAt > since, nil
		}
	}

	if getErr != nil {
		return true, getErr
	}
	return true, nil
}
