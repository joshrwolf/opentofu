// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/encryption"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/states/statemgr"
)

func TestBackend_EmptyState(t *testing.T) {
	b := newTestBackend(t)

	sm, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}

	if err := sm.RefreshState(context.Background()); err != nil {
		t.Fatal(err)
	}

	state := sm.State()
	if state != nil {
		t.Fatalf("expected nil state for empty database, got %v", state)
	}
}

func TestBackend_WriteAndRefresh(t *testing.T) {
	b := newTestBackend(t)

	sm, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}

	// Write a state with one resource instance.
	state := states.NewState()
	rootModule := state.EnsureModule(addrs.RootModuleInstance)
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "apko_build",
			Name: "this",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:      states.ObjectReady,
			AttrsJSON:   []byte(`{"id":"abc123","image_ref":"cgr.dev/chainguard/static:latest"}`),
			ContentHash: "sha256:deadbeef",
			CachedAt:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("apko"),
			Module:   addrs.RootModule,
		},
		addrs.NoKey,
	)

	if err := sm.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if err := sm.PersistState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Create a new state manager (simulates a new process) and refresh.
	sm2, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm2.RefreshState(context.Background()); err != nil {
		t.Fatal(err)
	}

	refreshed := sm2.State()
	if refreshed == nil {
		t.Fatal("expected non-nil state after persist+refresh")
	}

	// Verify the resource instance round-tripped.
	addr := addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "apko_build",
		Name: "this",
	}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance)

	inst := refreshed.ResourceInstance(addr)
	if inst == nil {
		t.Fatal("expected resource instance")
	}
	obj := inst.Current
	if obj == nil {
		t.Fatal("expected resource instance in refreshed state")
	}
	if obj.ContentHash != "sha256:deadbeef" {
		t.Errorf("content hash = %q, want %q", obj.ContentHash, "sha256:deadbeef")
	}
	if obj.CachedAt.IsZero() {
		t.Error("expected non-zero CachedAt")
	}
}

func TestBackend_ContentHashRoundTrip(t *testing.T) {
	b := newTestBackend(t)

	sm, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}

	// Write state with content hash + cached_at (the build cache fields).
	state := states.NewState()
	rootModule := state.EnsureModule(addrs.RootModuleInstance)
	cachedAt := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	rootModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "oci_tags",
			Name: "latest",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:      states.ObjectReady,
			AttrsJSON:   []byte(`{"repo":"cgr.dev/chainguard/static","tags":{"latest":"sha256:abc"}}`),
			ContentHash: "sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
			CachedAt:    cachedAt,
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("oci"),
			Module:   addrs.RootModule,
		},
		addrs.NoKey,
	)

	if err := sm.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if err := sm.PersistState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Refresh into a new manager.
	sm2, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm2.RefreshState(context.Background()); err != nil {
		t.Fatal(err)
	}

	refreshed := sm2.State()
	addr := addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "oci_tags",
		Name: "latest",
	}.Instance(addrs.NoKey).Absolute(addrs.RootModuleInstance)

	inst := refreshed.ResourceInstance(addr)
	if inst == nil {
		t.Fatal("expected resource instance")
	}
	obj := inst.Current
	if obj == nil {
		t.Fatal("expected resource instance")
	}
	if obj.ContentHash != "sha256:abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890" {
		t.Errorf("content hash mismatch: %s", obj.ContentHash)
	}
	if !obj.CachedAt.Equal(cachedAt) {
		t.Errorf("cached_at = %v, want %v", obj.CachedAt, cachedAt)
	}
}

func TestBackend_MultipleResources(t *testing.T) {
	b := newTestBackend(t)

	sm, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}

	state := states.NewState()
	rootModule := state.EnsureModule(addrs.RootModuleInstance)

	provider := addrs.AbsProviderConfig{
		Provider: addrs.NewDefaultProvider("test"),
		Module:   addrs.RootModule,
	}

	// Add 100 resources to verify bulk write performance.
	for i := range 100 {
		rootModule.SetResourceInstanceCurrent(
			addrs.Resource{
				Mode: addrs.ManagedResourceMode,
				Type: "test_resource",
				Name: "this",
			}.Instance(addrs.IntKey(i)),
			&states.ResourceInstanceObjectSrc{
				Status:      states.ObjectReady,
				AttrsJSON:   []byte(`{"id":"` + string(rune('a'+i%26)) + `"}`),
				ContentHash: "sha256:hash" + string(rune('a'+i%26)),
			},
			provider,
			addrs.NoKey,
		)
	}

	if err := sm.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if err := sm.PersistState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Refresh and verify count.
	sm2, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm2.RefreshState(context.Background()); err != nil {
		t.Fatal(err)
	}

	refreshed := sm2.State()
	root := refreshed.Module(addrs.RootModuleInstance)
	if root == nil {
		t.Fatal("expected root module")
	}

	var count int
	for _, rs := range root.Resources {
		count += len(rs.Instances)
	}
	if count != 100 {
		t.Errorf("expected 100 instances, got %d", count)
	}
}

func TestBackend_Outputs(t *testing.T) {
	b := newTestBackend(t)

	sm, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}

	state := states.NewState()
	rootModule := state.EnsureModule(addrs.RootModuleInstance)
	rootModule.OutputValues["image_ref"] = &states.OutputValue{
		Value:     cty.StringVal("cgr.dev/chainguard/static:latest"),
		Sensitive: false,
	}

	if err := sm.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if err := sm.PersistState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Refresh and verify output.
	sm2, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm2.RefreshState(context.Background()); err != nil {
		t.Fatal(err)
	}

	outputs, err := sm2.GetRootOutputValues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 1 {
		t.Fatalf("expected 1 output, got %d", len(outputs))
	}
	ov := outputs["image_ref"]
	if ov == nil {
		t.Fatal("missing image_ref output")
	}
	if ov.Value.AsString() != "cgr.dev/chainguard/static:latest" {
		t.Errorf("output value = %q", ov.Value.AsString())
	}
}

func TestBackend_Locking(t *testing.T) {
	b := newTestBackend(t)

	sm1, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}

	// Acquire lock.
	lockID, err := sm1.Lock(context.Background(), &statemgr.LockInfo{
		Operation: "build",
		Info:      "test",
	})
	if err != nil {
		t.Fatalf("failed to acquire lock: %s", err)
	}

	// Second lock attempt should fail.
	sm2, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sm2.Lock(context.Background(), &statemgr.LockInfo{
		Operation: "build",
		Info:      "test2",
	})
	if err == nil {
		t.Fatal("expected error acquiring second lock")
	}

	// Unlock and try again.
	if err := sm1.Unlock(context.Background(), lockID); err != nil {
		t.Fatalf("failed to unlock: %s", err)
	}

	lockID2, err := sm2.Lock(context.Background(), &statemgr.LockInfo{
		Operation: "build",
		Info:      "test2",
	})
	if err != nil {
		t.Fatalf("failed to acquire lock after unlock: %s", err)
	}
	if err := sm2.Unlock(context.Background(), lockID2); err != nil {
		t.Fatal(err)
	}
}

func TestBackend_Workspaces(t *testing.T) {
	b := newTestBackend(t)

	workspaces, err := b.Workspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 1 || workspaces[0] != backend.DefaultStateName {
		t.Fatalf("expected only default workspace, got %v", workspaces)
	}
}

func TestBackend_ModuleInstances(t *testing.T) {
	b := newTestBackend(t)

	sm, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}

	// Create state with resources in a child module.
	state := states.NewState()
	childAddr := addrs.RootModuleInstance.Child("publisher", addrs.NoKey)
	childModule := state.EnsureModule(childAddr)
	childModule.SetResourceInstanceCurrent(
		addrs.Resource{
			Mode: addrs.ManagedResourceMode,
			Type: "apko_build",
			Name: "this",
		}.Instance(addrs.NoKey),
		&states.ResourceInstanceObjectSrc{
			Status:      states.ObjectReady,
			AttrsJSON:   []byte(`{"id":"build1"}`),
			ContentHash: "sha256:modulehash",
		},
		addrs.AbsProviderConfig{
			Provider: addrs.NewDefaultProvider("apko"),
			Module:   addrs.RootModule,
		},
		addrs.NoKey,
	)

	if err := sm.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if err := sm.PersistState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Refresh and verify module structure.
	sm2, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm2.RefreshState(context.Background()); err != nil {
		t.Fatal(err)
	}

	refreshed := sm2.State()
	mod := refreshed.Module(childAddr)
	if mod == nil {
		t.Fatal("expected child module in refreshed state")
	}

	addr := addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "apko_build",
		Name: "this",
	}.Instance(addrs.NoKey).Absolute(childAddr)

	inst := refreshed.ResourceInstance(addr)
	if inst == nil {
		t.Fatal("expected resource instance")
	}
	obj := inst.Current
	if obj == nil {
		t.Fatal("expected resource in child module")
	}
	if obj.ContentHash != "sha256:modulehash" {
		t.Errorf("content hash = %q", obj.ContentHash)
	}
}

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	b := New(encryption.StateEncryptionDisabled()).(*Backend)
	b.dbPath = dbPath

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := initDB(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	b.db = db
	t.Cleanup(func() { db.Close() })
	return b
}
