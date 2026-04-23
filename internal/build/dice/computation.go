// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package dice

import (
	"context"
	"hash/maphash"
	"sync"
	"sync/atomic"
)

const (
	shardCount = 64
	shardMask  = shardCount - 1
)

var hashSeed = maphash.MakeSeed()

type indexShard[K comparable] struct {
	mu    sync.RWMutex
	index map[K]slotID
}

type Computation[K comparable, A any, V any] struct {
	session *Session
	id      queryID
	name    string
	fn      func(context.Context, A) (V, error)
	key     func(A) K
	label   func(A) string
	eq      func(V, V) bool

	counters computationCounters

	shards [shardCount]indexShard[K]

	entriesMu sync.Mutex
	entries   entryStore[A, V]
}

type computationCounters struct {
	fastPath  atomic.Int64
	verified  atomic.Int64
	computed  atomic.Int64
	coalesced atomic.Int64
}

type Stats struct {
	Name      string
	Entries   int
	FastPath  int64
	Verified  int64
	Computed  int64
	Coalesced int64
}

func (s Stats) Gets() int64 {
	return s.FastPath + s.Verified + s.Computed + s.Coalesced
}

type Option[V any] func(*computationConfig[V])

type computationConfig[V any] struct {
	eq func(V, V) bool
}

// WithEqual enables early cutoff: if a recomputed value equals the
// cached value, downstream dependents skip re-verification. Values
// must be immutable — equal values are substituted freely.
func WithEqual[V any](eq func(V, V) bool) Option[V] {
	return func(c *computationConfig[V]) {
		c.eq = eq
	}
}

func Register[K comparable, A any, V any](
	s *Session,
	name string,
	fn func(context.Context, A) (V, error),
	key func(A) K,
	label func(A) string,
	opts ...Option[V],
) *Computation[K, A, V] {
	var cfg computationConfig[V]
	for _, opt := range opts {
		opt(&cfg)
	}
	c := &Computation[K, A, V]{
		session: s,
		name:    name,
		fn:      fn,
		key:     key,
		label:   label,
		eq:      cfg.eq,
	}
	for i := range c.shards {
		c.shards[i].index = make(map[K]slotID)
	}
	c.id = s.registerQuery(c)
	return c
}

func RecordVolatile(ctx context.Context) {
	currentFrame(ctx).recordDep(depRef{query: volatileQuery})
}

func (c *Computation[K, A, V]) Get(ctx context.Context, arg A) (V, error) {
	return c.getSlot(ctx, c.getOrCreate(arg))
}

func (c *Computation[K, A, V]) shardFor(k K) *indexShard[K] {
	h := maphash.Comparable(hashSeed, k)
	return &c.shards[h&shardMask]
}

func (c *Computation[K, A, V]) getOrCreate(arg A) *entry[A, V] {
	k := c.key(arg)
	shard := c.shardFor(k)

	shard.mu.RLock()
	if id, ok := shard.index[k]; ok {
		shard.mu.RUnlock()
		return c.entries.at(id)
	}
	shard.mu.RUnlock()

	shard.mu.Lock()
	defer shard.mu.Unlock()

	if id, ok := shard.index[k]; ok {
		return c.entries.at(id)
	}

	c.entriesMu.Lock()
	id, e := c.entries.alloc(arg)
	c.entriesMu.Unlock()

	shard.index[k] = id
	return e
}

func (c *Computation[K, A, V]) slotLabel(e *entry[A, V]) string {
	return c.name + c.label(e.arg)
}

func (c *Computation[K, A, V]) Stats() Stats {
	return Stats{
		Name:      c.name,
		Entries:   int(c.entries.len.Load()),
		FastPath:  c.counters.fastPath.Load(),
		Verified:  c.counters.verified.Load(),
		Computed:  c.counters.computed.Load(),
		Coalesced: c.counters.coalesced.Load(),
	}
}
