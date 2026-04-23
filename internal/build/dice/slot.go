// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package dice

import (
	"sync"
	"sync/atomic"
)

type queryID uint16

type slotID uint32

type depRef struct {
	query queryID
	slot  slotID
}

const volatileQuery queryID = 0

type result[V any] struct {
	value     V
	err       error
	deps      []depRef
	changedAt uint64
}

type slot[V any] struct {
	snap       atomic.Pointer[result[V]]
	verifiedAt atomic.Uint64
	mu         sync.Mutex
	exec       *execution[V]
}

func (s *slot[V]) clearExec(exec *execution[V]) {
	s.mu.Lock()
	if s.exec == exec {
		s.exec = nil
	}
	s.mu.Unlock()
}

type entry[A any, V any] struct {
	id   slotID
	arg  A
	slot slot[V]
}

const (
	pageBits = 10
	pageSize = 1 << pageBits
	pageMask = pageSize - 1
)

type entryStore[A any, V any] struct {
	pages atomic.Pointer[[]*[pageSize]entry[A, V]]
	len   atomic.Uint32
}

func (s *entryStore[A, V]) alloc(arg A) (slotID, *entry[A, V]) {
	id := slotID(s.len.Load())
	pageIdx := uint32(id) >> pageBits

	var ps []*[pageSize]entry[A, V]
	if p := s.pages.Load(); p != nil {
		ps = *p
	}
	if int(pageIdx) >= len(ps) {
		next := make([]*[pageSize]entry[A, V], pageIdx+1)
		copy(next, ps)
		next[pageIdx] = new([pageSize]entry[A, V])
		s.pages.Store(&next)
		ps = next
	}
	e := &ps[pageIdx][uint32(id)&pageMask]
	e.id = id
	e.arg = arg
	s.len.Store(uint32(id) + 1)
	return id, e
}

func (s *entryStore[A, V]) at(id slotID) *entry[A, V] {
	pages := *s.pages.Load()
	return &pages[uint32(id)>>pageBits][uint32(id)&pageMask]
}
