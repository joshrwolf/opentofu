// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/encryption"
	"github.com/opentofu/opentofu/internal/states"
)

func TestBuildBackend_DefaultState(t *testing.T) {
	dir := t.TempDir()
	b := newTestBackend(t, dir)

	sm, err := b.StateMgr(context.Background(), backend.DefaultStateName)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if err := sm.RefreshState(context.Background()); err != nil {
		t.Fatalf("refresh error: %s", err)
	}

	// Write and persist an empty state
	if err := sm.WriteState(states.NewState()); err != nil {
		t.Fatalf("write error: %s", err)
	}
	if err := sm.PersistState(context.Background(), nil); err != nil {
		t.Fatalf("persist error: %s", err)
	}

	// Verify file was created at root of state dir
	statePath := filepath.Join(dir, "state.json")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Fatalf("state file not created at %s", statePath)
	}
}

func TestBuildBackend_ModulePathState(t *testing.T) {
	dir := t.TempDir()
	b := newTestBackend(t, dir)

	sm, err := b.StateMgr(context.Background(), "images/nginx")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if err := sm.WriteState(states.NewState()); err != nil {
		t.Fatalf("write error: %s", err)
	}
	if err := sm.PersistState(context.Background(), nil); err != nil {
		t.Fatalf("persist error: %s", err)
	}

	// Verify nested directory structure
	statePath := filepath.Join(dir, "images", "nginx", "state.json")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Fatalf("state file not created at %s", statePath)
	}
}

func TestBuildBackend_IndependentModuleStates(t *testing.T) {
	dir := t.TempDir()
	b := newTestBackend(t, dir)

	// Create two independent module states
	sm1, err := b.StateMgr(context.Background(), "images/nginx")
	if err != nil {
		t.Fatal(err)
	}
	sm2, err := b.StateMgr(context.Background(), "images/go")
	if err != nil {
		t.Fatal(err)
	}

	// Persist both
	if err := sm1.WriteState(states.NewState()); err != nil {
		t.Fatal(err)
	}
	if err := sm1.PersistState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := sm2.WriteState(states.NewState()); err != nil {
		t.Fatal(err)
	}
	if err := sm2.PersistState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Verify both state files exist independently
	for _, p := range []string{"images/nginx/state.json", "images/go/state.json"} {
		full := filepath.Join(dir, p)
		if _, err := os.Stat(full); os.IsNotExist(err) {
			t.Fatalf("expected state file at %s", full)
		}
	}
}

func TestBuildBackend_Workspaces(t *testing.T) {
	dir := t.TempDir()
	b := newTestBackend(t, dir)

	workspaces, err := b.Workspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 1 || workspaces[0] != backend.DefaultStateName {
		t.Fatalf("expected only default workspace, got %v", workspaces)
	}
}

func TestBuildBackend_DeleteWorkspaceFails(t *testing.T) {
	dir := t.TempDir()
	b := newTestBackend(t, dir)

	if err := b.DeleteWorkspace(context.Background(), "anything", false); err == nil {
		t.Fatal("expected error deleting workspace in build backend")
	}
}

func TestBuildBackend_DeepModulePath(t *testing.T) {
	dir := t.TempDir()
	b := newTestBackend(t, dir)

	// Test deeply nested module path
	sm, err := b.StateMgr(context.Background(), "charts/ingress-nginx/variants/fips")
	if err != nil {
		t.Fatal(err)
	}

	if err := sm.WriteState(states.NewState()); err != nil {
		t.Fatal(err)
	}
	if err := sm.PersistState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(dir, "charts", "ingress-nginx", "variants", "fips", "state.json")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		t.Fatalf("state file not created at %s", statePath)
	}
}

func TestBuildBackend_PersistAndRefresh(t *testing.T) {
	dir := t.TempDir()
	b := newTestBackend(t, dir)

	// Write state
	sm1, err := b.StateMgr(context.Background(), "images/redis")
	if err != nil {
		t.Fatal(err)
	}
	state := states.NewState()
	if err := sm1.WriteState(state); err != nil {
		t.Fatal(err)
	}
	if err := sm1.PersistState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// Create a NEW state manager for the same path (simulates a new build run)
	sm2, err := b.StateMgr(context.Background(), "images/redis")
	if err != nil {
		t.Fatal(err)
	}
	if err := sm2.RefreshState(context.Background()); err != nil {
		t.Fatal(err)
	}

	refreshed := sm2.State()
	if refreshed == nil {
		t.Fatal("expected non-nil state after persist+refresh cycle")
	}
}

func newTestBackend(t *testing.T, stateDir string) *Backend {
	t.Helper()
	b := New(encryption.StateEncryptionDisabled()).(*Backend)
	b.stateDir = stateDir
	return b
}
