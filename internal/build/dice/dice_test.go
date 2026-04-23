// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package dice_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/google/go-cmp/cmp"

	"github.com/opentofu/opentofu/internal/build/dice"
)

func intKey(n int) int         { return n }
func intLabel(n int) string    { return strconv.Itoa(n) }
func strKey(s string) string   { return s }
func strLabel(s string) string { return s }

// TestGet covers the core memoization contract: values and errors are
// computed at most once per key and returned on subsequent calls.
func TestGet(t *testing.T) {
	sentinel := errors.New("boom")

	tests := []struct {
		name      string
		fn        func(context.Context, int) (int, error)
		gets      []int
		want      []int
		wantErr   error
		wantCalls int64
	}{
		{
			name:      "single key memoized",
			fn:        func(_ context.Context, n int) (int, error) { return n * 2, nil },
			gets:      []int{5, 5, 5},
			want:      []int{10, 10, 10},
			wantCalls: 1,
		},
		{
			name:      "distinct keys compute separately",
			fn:        func(_ context.Context, n int) (int, error) { return n * 3, nil },
			gets:      []int{2, 3, 2, 3},
			want:      []int{6, 9, 6, 9},
			wantCalls: 2,
		},
		{
			name:      "zero value result cached",
			fn:        func(_ context.Context, _ int) (int, error) { return 0, nil },
			gets:      []int{1, 1},
			want:      []int{0, 0},
			wantCalls: 1,
		},
		{
			name:      "error cached across calls",
			fn:        func(_ context.Context, _ int) (int, error) { return 0, sentinel },
			gets:      []int{1, 1, 1},
			want:      []int{0, 0, 0},
			wantErr:   sentinel,
			wantCalls: 1,
		},
		{
			name:      "errors keyed independently",
			fn:        func(_ context.Context, _ int) (int, error) { return 0, sentinel },
			gets:      []int{1, 2, 1, 2},
			want:      []int{0, 0, 0, 0},
			wantErr:   sentinel,
			wantCalls: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int64
			s, _ := dice.NewSession(t.Context())
			c := dice.Register(s, "test.", func(ctx context.Context, n int) (int, error) {
				calls.Add(1)
				return tt.fn(ctx, n)
			}, intKey, intLabel)

			for i, key := range tt.gets {
				got, err := c.Get(t.Context(), key)
				if tt.wantErr != nil {
					if !errors.Is(err, tt.wantErr) {
						t.Fatalf("Get(%d): got err %v, want %v", key, err, tt.wantErr)
					}
				} else if err != nil {
					t.Fatalf("Get(%d): unexpected error: %v", key, err)
				}
				if got != tt.want[i] {
					t.Fatalf("Get(%d): got %d, want %d", key, got, tt.want[i])
				}
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Fatalf("compute called %d times, want %d", got, tt.wantCalls)
			}
		})
	}
}

// TestGet_DependencyChain verifies computation graphs that mirror the
// solver's layered architecture: leaf → instances → config → value.
func TestGet_DependencyChain(t *testing.T) {
	t.Run("four-layer chain", func(t *testing.T) {
		s, _ := dice.NewSession(t.Context())

		layer0 := dice.Register(s, "eval.", func(_ context.Context, key string) (string, error) {
			return "eval:" + key, nil
		}, strKey, strLabel)

		layer1 := dice.Register(s, "instances.", func(ctx context.Context, key string) (string, error) {
			v, err := layer0.Get(ctx, key)
			if err != nil {
				return "", err
			}
			return "instances(" + v + ")", nil
		}, strKey, strLabel)

		layer2 := dice.Register(s, "config.", func(ctx context.Context, key string) (string, error) {
			v, err := layer1.Get(ctx, key)
			if err != nil {
				return "", err
			}
			return "config(" + v + ")", nil
		}, strKey, strLabel)

		layer3 := dice.Register(s, "value.", func(ctx context.Context, key string) (string, error) {
			v, err := layer2.Get(ctx, key)
			if err != nil {
				return "", err
			}
			return "value(" + v + ")", nil
		}, strKey, strLabel)

		got, err := layer3.Get(t.Context(), "x")
		if err != nil {
			t.Fatal(err)
		}
		want := "value(config(instances(eval:x)))"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}

		// Second call should be fully cached at every layer.
		got2, err := layer3.Get(t.Context(), "x")
		if err != nil {
			t.Fatal(err)
		}
		if got2 != want {
			t.Fatalf("cached: got %q, want %q", got2, want)
		}
		for _, stats := range []dice.Stats{layer0.Stats(), layer1.Stats(), layer2.Stats(), layer3.Stats()} {
			if stats.Computed != 1 {
				t.Fatalf("%s: computed %d times, want 1", stats.Name, stats.Computed)
			}
		}
	})

	t.Run("error propagates through chain", func(t *testing.T) {
		s, _ := dice.NewSession(t.Context())
		sentinel := errors.New("leaf error")

		leaf := dice.Register(s, "leaf.", func(_ context.Context, _ string) (string, error) {
			return "", sentinel
		}, strKey, strLabel)

		mid := dice.Register(s, "mid.", func(ctx context.Context, key string) (string, error) {
			return leaf.Get(ctx, key)
		}, strKey, strLabel)

		top := dice.Register(s, "top.", func(ctx context.Context, key string) (string, error) {
			return mid.Get(ctx, key)
		}, strKey, strLabel)

		_, err := top.Get(t.Context(), "x")
		if !errors.Is(err, sentinel) {
			t.Fatalf("error should propagate: got %v, want %v", err, sentinel)
		}
	})

	t.Run("diamond dependency", func(t *testing.T) {
		s, _ := dice.NewSession(t.Context())
		var leafCalls atomic.Int64

		leaf := dice.Register(s, "leaf.", func(_ context.Context, n int) (int, error) {
			leafCalls.Add(1)
			return n, nil
		}, intKey, intLabel)

		left := dice.Register(s, "left.", func(ctx context.Context, n int) (int, error) {
			v, err := leaf.Get(ctx, n)
			if err != nil {
				return 0, err
			}
			return v + 1, nil
		}, intKey, intLabel)

		right := dice.Register(s, "right.", func(ctx context.Context, n int) (int, error) {
			v, err := leaf.Get(ctx, n)
			if err != nil {
				return 0, err
			}
			return v + 2, nil
		}, intKey, intLabel)

		top := dice.Register(s, "top.", func(ctx context.Context, n int) (int, error) {
			l, err := left.Get(ctx, n)
			if err != nil {
				return 0, err
			}
			r, err := right.Get(ctx, n)
			if err != nil {
				return 0, err
			}
			return l + r, nil
		}, intKey, intLabel)

		got, err := top.Get(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		// left = 10+1=11, right = 10+2=12, top = 23
		if got != 23 {
			t.Fatalf("got %d, want 23", got)
		}
		if n := leafCalls.Load(); n != 1 {
			t.Fatalf("leaf computed %d times, want 1 (shared by left+right)", n)
		}
	})
}

// TestGet_FanOut models the solver's collection evaluation pattern:
// a parent computation fans out to N children, collects all results.
func TestGet_FanOut(t *testing.T) {
	t.Run("parallel children collected by parent", func(t *testing.T) {
		s, _ := dice.NewSession(t.Context())
		var childCalls atomic.Int64

		child := dice.Register(s, "child.", func(_ context.Context, n int) (int, error) {
			childCalls.Add(1)
			return n * n, nil
		}, intKey, intLabel)

		parent := dice.Register(s, "parent.", func(ctx context.Context, n int) (int, error) {
			sum := 0
			for i := range n {
				v, err := child.Get(ctx, i)
				if err != nil {
					return 0, err
				}
				sum += v
			}
			return sum, nil
		}, intKey, intLabel)

		// Sum of squares 0..15
		got, err := parent.Get(t.Context(), 16)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for i := range 16 {
			want += i * i
		}
		if got != want {
			t.Fatalf("got %d, want %d", got, want)
		}
		if n := childCalls.Load(); n != 16 {
			t.Fatalf("child computed %d times, want 16", n)
		}

		// Second call: everything cached.
		got2, err := parent.Get(t.Context(), 16)
		if err != nil {
			t.Fatal(err)
		}
		if got2 != want {
			t.Fatalf("cached: got %d, want %d", got2, want)
		}
		if n := childCalls.Load(); n != 16 {
			t.Fatalf("child computed %d times after cache hit, want still 16", n)
		}
	})

	t.Run("error in one child fails parent", func(t *testing.T) {
		s, _ := dice.NewSession(t.Context())
		sentinel := errors.New("child 5 failed")

		child := dice.Register(s, "child.", func(_ context.Context, n int) (int, error) {
			if n == 5 {
				return 0, sentinel
			}
			return n, nil
		}, intKey, intLabel)

		parent := dice.Register(s, "parent.", func(ctx context.Context, n int) (int, error) {
			for i := range n {
				if _, err := child.Get(ctx, i); err != nil {
					return 0, err
				}
			}
			return n, nil
		}, intKey, intLabel)

		_, err := parent.Get(t.Context(), 10)
		if !errors.Is(err, sentinel) {
			t.Fatalf("parent should see child error: got %v, want %v", err, sentinel)
		}
	})
}

// TestGet_Suspension models the core dice insight: a computation blocks on
// an external event (channel), other computations proceed independently,
// and the blocked computation resumes when the event fires.
func TestGet_Suspension(t *testing.T) {
	t.Run("blocked computation resumes after external event", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := dice.NewSession(t.Context())
			engineResult := make(chan int, 1)

			// Models a Layer 3 computation blocking on engine.Result.
			value := dice.Register(s, "value.", func(ctx context.Context, _ int) (int, error) {
				select {
				case v := <-engineResult:
					return v, nil
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			}, intKey, intLabel)

			// Models a Layer 0 computation that resolves instantly.
			eval := dice.Register(s, "eval.", func(_ context.Context, n int) (int, error) {
				return n * 10, nil
			}, intKey, intLabel)

			// Launch both concurrently.
			var valueCh, evalCh = make(chan int, 1), make(chan int, 1)
			go func() { v, _ := value.Get(t.Context(), 1); valueCh <- v }()
			go func() { v, _ := eval.Get(t.Context(), 1); evalCh <- v }()
			synctest.Wait()

			// eval resolves immediately.
			if v := <-evalCh; v != 10 {
				t.Fatalf("eval = %d, want 10", v)
			}

			// value is still blocked. Deliver the engine result.
			engineResult <- 42
			synctest.Wait()

			if v := <-valueCh; v != 42 {
				t.Fatalf("value = %d, want 42", v)
			}
		})
	})

	t.Run("parent blocks until suspended child resolves", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := dice.NewSession(t.Context())
			engineResult := make(chan string, 1)

			action := dice.Register(s, "action.", func(ctx context.Context, key string) (string, error) {
				select {
				case v := <-engineResult:
					return v, nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}, strKey, strLabel)

			output := dice.Register(s, "output.", func(ctx context.Context, key string) (string, error) {
				v, err := action.Get(ctx, key)
				if err != nil {
					return "", err
				}
				return "output:" + v, nil
			}, strKey, strLabel)

			resultCh := make(chan string, 1)
			go func() { v, _ := output.Get(t.Context(), "x"); resultCh <- v }()
			synctest.Wait()

			engineResult <- "done"
			synctest.Wait()

			if got := <-resultCh; got != "output:done" {
				t.Fatalf("got %q, want %q", got, "output:done")
			}
		})
	})
}

func TestGet_CycleDetection(t *testing.T) {
	t.Run("direct self-cycle", func(t *testing.T) {
		s, _ := dice.NewSession(t.Context())
		var c *dice.Computation[int, int, int]
		c = dice.Register(s, "self.", func(ctx context.Context, n int) (int, error) {
			return c.Get(ctx, n)
		}, intKey, intLabel)

		_, err := c.Get(t.Context(), 1)
		if err == nil {
			t.Fatal("expected cycle error")
		}
		if !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("error should mention cycle, got: %v", err)
		}
	})

	t.Run("indirect cycle A-B-A", func(t *testing.T) {
		s, _ := dice.NewSession(t.Context())
		var a, b *dice.Computation[string, string, string]
		a = dice.Register(s, "a.", func(ctx context.Context, _ string) (string, error) {
			return b.Get(ctx, "from-a")
		}, strKey, strLabel)
		b = dice.Register(s, "b.", func(ctx context.Context, _ string) (string, error) {
			return a.Get(ctx, "from-b")
		}, strKey, strLabel)

		_, err := a.Get(t.Context(), "start")
		if err == nil {
			t.Fatal("expected cycle error")
		}
		if !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("error should mention cycle, got: %v", err)
		}
	})

	t.Run("three-node cycle", func(t *testing.T) {
		s, _ := dice.NewSession(t.Context())
		var a, b, c *dice.Computation[string, string, string]
		a = dice.Register(s, "a.", func(ctx context.Context, _ string) (string, error) {
			return b.Get(ctx, "x")
		}, strKey, strLabel)
		b = dice.Register(s, "b.", func(ctx context.Context, _ string) (string, error) {
			return c.Get(ctx, "x")
		}, strKey, strLabel)
		c = dice.Register(s, "c.", func(ctx context.Context, _ string) (string, error) {
			return a.Get(ctx, "x")
		}, strKey, strLabel)

		_, err := a.Get(t.Context(), "x")
		if err == nil {
			t.Fatal("expected cycle error")
		}
		if !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("error should mention cycle, got: %v", err)
		}
	})
}

func TestGet_Coalescing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		gate := make(chan struct{})
		s, _ := dice.NewSession(t.Context())
		c := dice.Register(s, "slow.", func(_ context.Context, n int) (int, error) {
			calls.Add(1)
			<-gate
			return n * 10, nil
		}, intKey, intLabel)

		const N = 50
		results := make([]int, N)
		errs := make([]error, N)
		var wg sync.WaitGroup
		for i := range N {
			wg.Go(func() {
				results[i], errs[i] = c.Get(t.Context(), 7)
			})
		}

		synctest.Wait()
		close(gate)
		wg.Wait()

		for i := range N {
			if errs[i] != nil {
				t.Fatalf("goroutine %d: %v", i, errs[i])
			}
			if results[i] != 70 {
				t.Fatalf("goroutine %d: got %d, want 70", i, results[i])
			}
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("compute called %d times, want 1", got)
		}
	})
}

func TestGet_Cancellation(t *testing.T) {
	t.Run("caller cancelled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := dice.NewSession(t.Context())
			started := make(chan struct{})
			c := dice.Register(s, "block.", func(ctx context.Context, _ int) (int, error) {
				close(started)
				<-ctx.Done()
				return 0, ctx.Err()
			}, intKey, intLabel)

			ctx, cancel := context.WithCancel(t.Context())
			errCh := make(chan error, 1)
			go func() {
				_, err := c.Get(ctx, 1)
				errCh <- err
			}()

			<-started
			cancel()
			synctest.Wait()

			select {
			case err := <-errCh:
				if err == nil {
					t.Fatal("expected error after caller cancellation")
				}
			default:
				t.Fatal("Get should have returned after cancellation")
			}
		})
	})

	t.Run("session cancelled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			s, _ := dice.NewSession(ctx)
			started := make(chan struct{})

			c := dice.Register(s, "sess.", func(ctx context.Context, _ int) (int, error) {
				close(started)
				<-ctx.Done()
				return 0, ctx.Err()
			}, intKey, intLabel)

			errCh := make(chan error, 1)
			go func() {
				_, err := c.Get(t.Context(), 1)
				errCh <- err
			}()

			<-started
			cancel()
			synctest.Wait()

			select {
			case err := <-errCh:
				if err == nil {
					t.Fatal("expected error from session cancellation")
				}
			default:
				t.Fatal("Get should have returned after session cancellation")
			}
		})
	})

	t.Run("detachment preserves computation for other callers", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := dice.NewSession(t.Context())
			gate := make(chan struct{})
			started := make(chan struct{})

			c := dice.Register(s, "detach.", func(ctx context.Context, n int) (int, error) {
				close(started)
				select {
				case <-gate:
					return n * 2, nil
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			}, intKey, intLabel)

			// Computing goroutine (never cancelled).
			val1Ch := make(chan int, 1)
			err1Ch := make(chan error, 1)
			go func() {
				v, err := c.Get(t.Context(), 42)
				val1Ch <- v
				err1Ch <- err
			}()
			<-started

			// Coalesced waiter (will be cancelled).
			ctx2, cancel2 := context.WithCancel(t.Context())
			err2Ch := make(chan error, 1)
			go func() {
				_, err := c.Get(ctx2, 42)
				err2Ch <- err
			}()
			synctest.Wait()

			cancel2()
			synctest.Wait()

			select {
			case err := <-err2Ch:
				if err == nil {
					t.Fatal("detached waiter should get error")
				}
			default:
				t.Fatal("detached waiter should have returned")
			}

			close(gate)
			synctest.Wait()

			if err := <-err1Ch; err != nil {
				t.Fatalf("computing goroutine got error: %v", err)
			}
			if v := <-val1Ch; v != 84 {
				t.Fatalf("computing goroutine got %d, want 84", v)
			}

			// Cached from the surviving computation.
			v, err := c.Get(t.Context(), 42)
			if err != nil {
				t.Fatalf("cached Get: %v", err)
			}
			if v != 84 {
				t.Fatalf("cached Get = %d, want 84", v)
			}
		})
	})

	t.Run("abandonment allows fresh computation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := dice.NewSession(t.Context())
			var calls atomic.Int64
			started := make(chan struct{})

			c := dice.Register(s, "abandon.", func(ctx context.Context, n int) (int, error) {
				call := calls.Add(1)
				if call == 1 {
					close(started)
					<-ctx.Done()
					return 0, ctx.Err()
				}
				return n * 2, nil
			}, intKey, intLabel)

			ctx1, cancel1 := context.WithCancel(t.Context())
			err1Ch := make(chan error, 1)
			go func() {
				_, err := c.Get(ctx1, 5)
				err1Ch <- err
			}()

			<-started
			cancel1()
			synctest.Wait()

			select {
			case err := <-err1Ch:
				if err == nil {
					t.Fatal("abandoned caller should get error")
				}
			default:
				t.Fatal("abandoned caller should have returned")
			}

			v, err := c.Get(t.Context(), 5)
			if err != nil {
				t.Fatalf("fresh Get after abandonment: %v", err)
			}
			if v != 10 {
				t.Fatalf("fresh Get = %d, want 10", v)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("compute called %d times, want 2 (initial + retry)", got)
			}
		})
	})
}

func TestGet_Panic(t *testing.T) {
	t.Run("propagates to computing caller", func(t *testing.T) {
		s, _ := dice.NewSession(t.Context())
		c := dice.Register(s, "panic.", func(_ context.Context, _ int) (int, error) {
			panic("kaboom")
		}, intKey, intLabel)

		panicked := make(chan any, 1)
		go func() {
			defer func() { panicked <- recover() }()
			c.Get(t.Context(), 1)
		}()

		r := <-panicked
		if r == nil {
			t.Fatal("expected panic")
		}
		if got := fmt.Sprint(r); got != "kaboom" {
			t.Fatalf("panic value = %q, want %q", got, "kaboom")
		}
	})

	t.Run("coalesced waiter gets error not panic", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, _ := dice.NewSession(t.Context())
			started := make(chan struct{})
			proceed := make(chan struct{})

			c := dice.Register(s, "panic.", func(_ context.Context, _ int) (int, error) {
				close(started)
				<-proceed
				panic("kaboom")
			}, intKey, intLabel)

			panicCh := make(chan any, 1)
			go func() {
				defer func() { panicCh <- recover() }()
				c.Get(t.Context(), 1)
			}()
			<-started

			errCh := make(chan error, 1)
			go func() {
				_, err := c.Get(t.Context(), 1)
				errCh <- err
			}()
			synctest.Wait()

			close(proceed)
			synctest.Wait()

			if r := <-panicCh; r == nil {
				t.Fatal("computing goroutine should have panicked")
			}

			err := <-errCh
			if err == nil {
				t.Fatal("coalesced waiter should get error")
			}
			if !strings.Contains(err.Error(), "panicked") && !strings.Contains(err.Error(), "canceled") {
				t.Fatalf("unexpected error for coalesced waiter: %v", err)
			}
		})
	})
}

func TestGet_ConcurrentDistinctKeys(t *testing.T) {
	s, _ := dice.NewSession(t.Context())
	c := dice.Register(s, "conc.", func(_ context.Context, n int) (int, error) {
		return n * n, nil
	}, intKey, intLabel)

	const N = 200
	results := make([]int, N)
	errs := make([]error, N)
	var wg sync.WaitGroup
	for i := range N {
		wg.Go(func() {
			results[i], errs[i] = c.Get(t.Context(), i)
		})
	}
	wg.Wait()

	for i := range N {
		if errs[i] != nil {
			t.Fatalf("key %d: %v", i, errs[i])
		}
		if results[i] != i*i {
			t.Fatalf("key %d: got %d, want %d", i, results[i], i*i)
		}
	}

	want := dice.Stats{Name: "conc.", Entries: N, Computed: N}
	if diff := cmp.Diff(want, c.Stats(), ignoreStatsFlexible); diff != "" {
		t.Fatalf("Stats mismatch (-want +got):\n%s", diff)
	}
}

func TestStats(t *testing.T) {
	tests := []struct {
		name string
		gets []int
		want dice.Stats
	}{
		{
			name: "all unique",
			gets: []int{1, 2, 3},
			want: dice.Stats{Name: "stats.", Entries: 3, Computed: 3, FastPath: 0},
		},
		{
			name: "repeated key",
			gets: []int{1, 1, 1},
			want: dice.Stats{Name: "stats.", Entries: 1, Computed: 1, FastPath: 2},
		},
		{
			name: "mixed",
			gets: []int{1, 2, 1, 2, 3},
			want: dice.Stats{Name: "stats.", Entries: 3, Computed: 3, FastPath: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := dice.NewSession(t.Context())
			c := dice.Register(s, "stats.", func(_ context.Context, n int) (int, error) {
				return n, nil
			}, intKey, intLabel)

			for _, n := range tt.gets {
				if _, err := c.Get(t.Context(), n); err != nil {
					t.Fatalf("Get(%d): %v", n, err)
				}
			}

			if diff := cmp.Diff(tt.want, c.Stats()); diff != "" {
				t.Fatalf("Stats mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

var ignoreStatsFlexible = cmp.FilterPath(func(p cmp.Path) bool {
	switch p.Last().String() {
	case ".Coalesced", ".FastPath", ".Verified":
		return true
	}
	return false
}, cmp.Ignore())

func TestAdvance(t *testing.T) {
	t.Run("recomputes after advance when dep changed", func(t *testing.T) {
		s, advance := dice.NewSession(t.Context())
		var leafVal atomic.Int64
		leafVal.Store(10)

		leaf := dice.Register(s, "leaf.", func(ctx context.Context, _ int) (int, error) {
			dice.RecordVolatile(ctx)
			return int(leafVal.Load()), nil
		}, intKey, intLabel)

		parent := dice.Register(s, "parent.", func(ctx context.Context, n int) (int, error) {
			v, err := leaf.Get(ctx, n)
			if err != nil {
				return 0, err
			}
			return v + 1, nil
		}, intKey, intLabel)

		got, err := parent.Get(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if got != 11 {
			t.Fatalf("got %d, want 11", got)
		}

		leafVal.Store(20)
		advance()

		got2, err := parent.Get(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if got2 != 21 {
			t.Fatalf("after advance: got %d, want 21", got2)
		}
	})

	t.Run("verified without recompute when dep unchanged", func(t *testing.T) {
		s, advance := dice.NewSession(t.Context())
		var leafCalls, parentCalls atomic.Int64

		leaf := dice.Register(s, "leaf.", func(ctx context.Context, n int) (int, error) {
			dice.RecordVolatile(ctx)
			leafCalls.Add(1)
			return n, nil
		}, intKey, intLabel, dice.WithEqual(func(a, b int) bool { return a == b }))

		parent := dice.Register(s, "parent.", func(ctx context.Context, n int) (int, error) {
			parentCalls.Add(1)
			v, err := leaf.Get(ctx, n)
			if err != nil {
				return 0, err
			}
			return v + 1, nil
		}, intKey, intLabel)

		got, err := parent.Get(t.Context(), 5)
		if err != nil {
			t.Fatal(err)
		}
		if got != 6 {
			t.Fatalf("got %d, want 6", got)
		}

		advance()

		got2, err := parent.Get(t.Context(), 5)
		if err != nil {
			t.Fatal(err)
		}
		if got2 != 6 {
			t.Fatalf("after advance: got %d, want 6", got2)
		}

		// Leaf recomputes (always does on advance), but its value is the
		// same, so early cutoff keeps changedAt unchanged. Parent verifies
		// its dep on leaf, sees no change, and returns cached without
		// recomputing.
		if n := leafCalls.Load(); n != 2 {
			t.Fatalf("leaf computed %d times, want 2", n)
		}
		if n := parentCalls.Load(); n != 1 {
			t.Fatalf("parent computed %d times, want 1 (early cutoff)", n)
		}
	})

	t.Run("volatile deps always recompute", func(t *testing.T) {
		s, advance := dice.NewSession(t.Context())
		var calls atomic.Int64

		c := dice.Register(s, "volatile.", func(ctx context.Context, _ int) (int, error) {
			dice.RecordVolatile(ctx)
			calls.Add(1)
			return int(calls.Load()), nil
		}, intKey, intLabel)

		v1, _ := c.Get(t.Context(), 1)
		if v1 != 1 {
			t.Fatalf("got %d, want 1", v1)
		}

		advance()

		v2, _ := c.Get(t.Context(), 1)
		if v2 != 2 {
			t.Fatalf("after advance: got %d, want 2", v2)
		}
	})
}

func TestWithEqual(t *testing.T) {
	t.Run("early cutoff prevents downstream recompute", func(t *testing.T) {
		s, advance := dice.NewSession(t.Context())
		var baseCalls, derivedCalls atomic.Int64

		base := dice.Register(s, "base.", func(ctx context.Context, _ int) (int, error) {
			dice.RecordVolatile(ctx)
			baseCalls.Add(1)
			return 42, nil
		}, intKey, intLabel, dice.WithEqual(func(a, b int) bool { return a == b }))

		derived := dice.Register(s, "derived.", func(ctx context.Context, n int) (int, error) {
			derivedCalls.Add(1)
			v, err := base.Get(ctx, n)
			if err != nil {
				return 0, err
			}
			return v * 2, nil
		}, intKey, intLabel)

		v, _ := derived.Get(t.Context(), 1)
		if v != 84 {
			t.Fatalf("got %d, want 84", v)
		}

		advance()

		v2, _ := derived.Get(t.Context(), 1)
		if v2 != 84 {
			t.Fatalf("after advance: got %d, want 84", v2)
		}

		// Base recomputes (volatile dep forces it), but value is unchanged
		// so early cutoff keeps changedAt stable. Derived verifies its dep
		// on base, sees no change, and returns cached.
		if n := baseCalls.Load(); n != 2 {
			t.Fatalf("base computed %d times, want 2 (initial + reverify)", n)
		}
		if n := derivedCalls.Load(); n != 1 {
			t.Fatalf("derived computed %d times, want 1 (early cutoff saved it)", n)
		}
	})

	t.Run("recomputes downstream when value actually changes", func(t *testing.T) {
		s, advance := dice.NewSession(t.Context())
		var counter atomic.Int64

		base := dice.Register(s, "base.", func(ctx context.Context, _ int) (int, error) {
			dice.RecordVolatile(ctx)
			return int(counter.Add(1)), nil
		}, intKey, intLabel, dice.WithEqual(func(a, b int) bool { return a == b }))

		derived := dice.Register(s, "derived.", func(ctx context.Context, n int) (int, error) {
			v, err := base.Get(ctx, n)
			if err != nil {
				return 0, err
			}
			return v * 10, nil
		}, intKey, intLabel)

		v1, _ := derived.Get(t.Context(), 1)
		if v1 != 10 {
			t.Fatalf("got %d, want 10", v1)
		}

		advance()

		v2, _ := derived.Get(t.Context(), 1)
		if v2 != 20 {
			t.Fatalf("after advance: got %d, want 20", v2)
		}
	})
}
