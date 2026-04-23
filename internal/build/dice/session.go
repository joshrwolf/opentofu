// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package dice

import (
	"context"
	"sync"
	"sync/atomic"
)

type queryRuntime interface {
	changedAfterSlot(ctx context.Context, id slotID, since uint64) (bool, error)
}

type Session struct {
	ctx context.Context
	rev atomic.Uint64

	queries    atomic.Pointer[[]queryRuntime]
	registerMu sync.Mutex
}

func NewSession(ctx context.Context) (*Session, func()) {
	s := &Session{ctx: ctx}
	s.rev.Store(1)
	return s, func() { s.rev.Add(1) }
}

func (s *Session) revision() uint64 {
	return s.rev.Load()
}

func (s *Session) registerQuery(q queryRuntime) queryID {
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	var cur []queryRuntime
	if p := s.queries.Load(); p != nil {
		cur = *p
	}
	n := len(cur) + 1
	if n > int(^queryID(0)) {
		panic("dice: too many registered computations")
	}
	next := make([]queryRuntime, n)
	copy(next, cur)
	next[n-1] = q
	s.queries.Store(&next)
	return queryID(n)
}

func (s *Session) changedAfter(ctx context.Context, dep depRef, since uint64) (bool, error) {
	if dep.query == volatileQuery {
		return true, nil
	}
	queries := *s.queries.Load()
	q := queries[int(dep.query)-1]
	return q.changedAfterSlot(ctx, dep.slot, since)
}

func (s *Session) verifyDeps(ctx context.Context, deps []depRef, since uint64) (bool, error) {
	for _, dep := range deps {
		changed, err := s.changedAfter(ctx, dep, since)
		if err != nil {
			return false, err
		}
		if changed {
			return false, nil
		}
	}
	return true, nil
}
