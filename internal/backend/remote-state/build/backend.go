// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package build implements a SQLite-backed state backend for the build
// execution mode.
//
// Unlike the standard local backend which stores a monolithic JSON state file
// per workspace, the build backend stores each resource instance as an
// individual row in a SQLite database. This eliminates the O(N)
// serialize/deserialize bottleneck that makes the local backend pathologically
// slow at scale (17k+ resources).
//
// Key properties:
//   - WAL mode for concurrent readers/writers across processes
//   - Per-resource-instance row storage (no monolithic JSON)
//   - Incremental writes (only changed resources are persisted)
//   - Cooperative locking via database rows
//   - Single file: .chofu/state.db (plus WAL/SHM files managed by SQLite)
//
// State is stored at a configurable path (default: .chofu/state.db).
package build

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // SQLite driver

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
				Description: "Path to the SQLite database file. Defaults to .chofu/state.db in the working directory.",
				Default:     "",
			},
		},
	}
	b := &Backend{Backend: s, encryption: enc}
	b.Backend.ConfigureFunc = b.configure
	return b
}

// BuildBackend is the marker interface that identifies a backend as suitable
// for use with the `tofu build` command. The build command checks for this
// interface and rejects backends that don't implement it.
type BuildBackend interface {
	IsBuildBackend()
}

// Backend implements the build-system state backend backed by SQLite.
type Backend struct {
	*schema.Backend
	encryption encryption.StateEncryption

	dbPath string
	db     *sql.DB
}

// IsBuildBackend marks this backend as suitable for `tofu build`.
func (b *Backend) IsBuildBackend() {}

func (b *Backend) configure(ctx context.Context) error {
	data := schema.FromContextBackendConfig(ctx)

	dbPath := data.Get("path").(string)
	if dbPath == "" {
		dbPath = ".chofu/state.db"
	}

	if !filepath.IsAbs(dbPath) {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
		dbPath = filepath.Join(wd, dbPath)
	}

	b.dbPath = dbPath

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	b.db = db

	return initDB(ctx, db)
}

// openDB opens a SQLite database at the given path with performance PRAGMAs
// configured via the DSN. Creates parent directories as needed.
func openDB(path string) (*sql.DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating state directory %s: %w", dir, err)
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=synchronous(normal)&_pragma=cache_size(-64000)&_pragma=foreign_keys(on)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite database %s: %w", path, err)
	}

	// SQLite supports only one writer at a time. A single connection avoids
	// SQLITE_BUSY errors within the process; cross-process contention is
	// handled by the busy_timeout pragma.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging sqlite database %s: %w", path, err)
	}

	return db, nil
}

// Workspaces returns the list of known workspaces. The build backend always
// has at least the default workspace.
func (b *Backend) Workspaces(_ context.Context) ([]string, error) {
	// For now, only default. Future: query distinct workspaces from metadata.
	return []string{backend.DefaultStateName}, nil
}

// DeleteWorkspace is not supported by the build backend.
func (b *Backend) DeleteWorkspace(_ context.Context, name string, _ bool) error {
	if name == backend.DefaultStateName {
		return fmt.Errorf("cannot delete default state")
	}
	return fmt.Errorf("the build backend does not support workspace deletion")
}

// StateMgr returns a SQLite-backed state manager for the given workspace.
func (b *Backend) StateMgr(_ context.Context, name string) (statemgr.Full, error) {
	if b.db == nil {
		return nil, fmt.Errorf("backend not configured (call Configure first)")
	}

	return &SQLiteState{
		db:        b.db,
		workspace: name,
	}, nil
}

// Close closes the database connection. Should be called when the backend
// is no longer needed.
func (b *Backend) Close() error {
	if b.db != nil {
		return b.db.Close()
	}
	return nil
}
