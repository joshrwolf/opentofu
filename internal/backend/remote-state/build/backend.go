// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package build implements a backend for the build execution mode.
//
// Unlike the standard local backend which stores a single state file per
// workspace, the build backend stores state per module path. This allows
// each component (e.g., images/nginx, images/go) to have independent state
// that persists between builds, enabling content-hash-based caching.
//
// State is stored under a configurable directory (default: .buildtofu/state/)
// with the module path as the subdirectory structure:
//
//	.buildtofu/state/
//	  images/nginx/state.json
//	  images/go/state.json
//	  charts/ingress-nginx/state.json
//
// The build backend does not support workspaces — each module path IS
// effectively a workspace. It does not support remote state, locking (it's
// designed for single-user build systems), or encryption.
package build

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/encryption"
	"github.com/opentofu/opentofu/internal/legacy/helper/schema"
	"github.com/opentofu/opentofu/internal/states/statemgr"
)

// New creates a new build backend.
func New(enc encryption.StateEncryption) backend.Backend {
	s := &schema.Backend{
		Schema: map[string]*schema.Schema{
			"path": {
				Type:        schema.TypeString,
				Optional:    true,
				Description: "Directory where per-module state files are stored. Defaults to .buildtofu/state/ in the working directory.",
				Default:     "",
			},
		},
	}
	b := &Backend{Backend: s, encryption: enc}
	b.Backend.ConfigureFunc = b.configure
	return b
}

// Backend implements the build-system state backend. State is stored
// on the local filesystem, one state file per module path.
type Backend struct {
	*schema.Backend
	encryption encryption.StateEncryption

	// stateDir is the root directory for state files.
	stateDir string
}

func (b *Backend) configure(ctx context.Context) error {
	data := schema.FromContextBackendConfig(ctx)

	stateDir := data.Get("path").(string)
	if stateDir == "" {
		stateDir = ".buildtofu/state"
	}

	// Resolve relative to working directory
	if !filepath.IsAbs(stateDir) {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
		stateDir = filepath.Join(wd, stateDir)
	}

	b.stateDir = stateDir
	return nil
}

// Workspaces returns the default workspace only. The build backend uses
// module paths as the state key, not workspaces.
func (b *Backend) Workspaces(context.Context) ([]string, error) {
	return []string{backend.DefaultStateName}, nil
}

// DeleteWorkspace is not supported by the build backend.
func (b *Backend) DeleteWorkspace(_ context.Context, name string, _ bool) error {
	if name == backend.DefaultStateName {
		return fmt.Errorf("can't delete default state")
	}
	return fmt.Errorf("the build backend does not support workspaces")
}

// StateMgr returns a filesystem-based state manager for the given name.
//
// In the build backend, the "name" is expected to be the module path
// (e.g., "images/nginx"). The state is stored at:
//
//	<stateDir>/<name>/state.json
//
// For the default workspace name, state is stored at:
//
//	<stateDir>/state.json
func (b *Backend) StateMgr(_ context.Context, name string) (statemgr.Full, error) {
	var statePath string
	if name == backend.DefaultStateName {
		statePath = filepath.Join(b.stateDir, "state.json")
	} else {
		statePath = filepath.Join(b.stateDir, name, "state.json")
	}

	// Ensure the directory exists
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create state directory %s: %w", dir, err)
	}

	return statemgr.NewFilesystem(statePath, b.encryption), nil
}
