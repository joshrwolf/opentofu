// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/encryption"
	"github.com/opentofu/opentofu/internal/states"
)

// buildStateN creates a state with n resource instances in the root module.
func buildStateN(n int) *states.State {
	state := states.NewState()
	rootModule := state.EnsureModule(addrs.RootModuleInstance)

	provider := addrs.AbsProviderConfig{
		Provider: addrs.NewDefaultProvider("test"),
		Module:   addrs.RootModule,
	}

	for i := range n {
		rootModule.SetResourceInstanceCurrent(
			addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: "test_resource",
				Name: fmt.Sprintf("r%d", i),
			}.Instance(addrs.NoKey),
			&states.ResourceInstanceObjectSrc{
				Status:      states.ObjectReady,
				AttrsJSON:   []byte(fmt.Sprintf(`{"id":"id-%d","name":"resource-%d","tags":{"env":"prod","team":"platform"}}`, i, i)),
				ContentHash: fmt.Sprintf("sha256:%064x", i),
			},
			provider,
			addrs.NoKey,
		)
	}
	return state
}

func benchBackend(b *testing.B) *Backend {
	b.Helper()
	dbPath := filepath.Join(b.TempDir(), "state.db")
	be := New(encryption.StateEncryptionDisabled()).(*Backend)
	be.dbPath = dbPath
	db, err := openDB(dbPath)
	if err != nil {
		b.Fatal(err)
	}
	if err := initDB(context.Background(), db); err != nil {
		b.Fatal(err)
	}
	be.db = db
	b.Cleanup(func() { db.Close() })
	return be
}

// BenchmarkPersistState measures write performance at various scales.
func BenchmarkPersistState(b *testing.B) {
	for _, n := range []int{100, 1000, 5000, 17000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			be := benchBackend(b)
			state := buildStateN(n)

			sm, err := be.StateMgr(context.Background(), backend.DefaultStateName)
			if err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			for range b.N {
				if err := sm.WriteState(state); err != nil {
					b.Fatal(err)
				}
				if err := sm.PersistState(context.Background(), nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRefreshState measures read performance at various scales.
func BenchmarkRefreshState(b *testing.B) {
	for _, n := range []int{100, 1000, 5000, 17000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			be := benchBackend(b)
			state := buildStateN(n)

			// Pre-populate the database.
			sm, err := be.StateMgr(context.Background(), backend.DefaultStateName)
			if err != nil {
				b.Fatal(err)
			}
			if err := sm.WriteState(state); err != nil {
				b.Fatal(err)
			}
			if err := sm.PersistState(context.Background(), nil); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			for range b.N {
				sm2, err := be.StateMgr(context.Background(), backend.DefaultStateName)
				if err != nil {
					b.Fatal(err)
				}
				if err := sm2.RefreshState(context.Background()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
