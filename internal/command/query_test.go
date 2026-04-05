// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestQuery_SQLite(t *testing.T) {
	td := t.TempDir()
	t.Chdir(td)

	populateTestDB(t, td)

	tests := []struct {
		name string
		args []string
		want int
	}{
		{"ls", []string{"ls"}, 0},
		{"ls-filter", []string{"ls", "cosign"}, 0},
		{"deps", []string{"deps", "cosign_sign"}, 0},
		{"rdeps", []string{"rdeps", "apko_build"}, 0},
		{"status", []string{"status"}, 0},
		{"no-args", []string{}, 1},
		{"bad-subcommand", []string{"foo"}, 1},
		{"deps-no-match", []string{"deps", "nonexistent_xyz"}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qc := &QueryCommand{}
			code := qc.Run(tt.args)
			if code != tt.want {
				t.Errorf("exit code = %d, want %d", code, tt.want)
			}
		})
	}
}

func populateTestDB(t *testing.T, dir string) {
	t.Helper()
	dbDir := filepath.Join(dir, ".chofu")
	os.MkdirAll(dbDir, 0o755)
	dbPath := filepath.Join(dbDir, "state.db")

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS resource_instances (
			workspace TEXT NOT NULL DEFAULT 'default',
			module TEXT NOT NULL DEFAULT '',
			mode TEXT NOT NULL,
			type TEXT NOT NULL,
			name TEXT NOT NULL,
			instance_key TEXT NOT NULL DEFAULT '',
			deposed_key TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'ready',
			provider TEXT NOT NULL DEFAULT '',
			schema_version INTEGER NOT NULL DEFAULT 0,
			attributes BLOB, attributes_flat BLOB, sensitive_paths BLOB,
			private_raw BLOB, dependencies TEXT, refs TEXT,
			create_before_destroy INTEGER NOT NULL DEFAULT 0,
			skip_destroy INTEGER NOT NULL DEFAULT 0,
			identity BLOB, identity_schema_version INTEGER,
			content_hash TEXT NOT NULL DEFAULT '',
			cached_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (workspace, module, mode, type, name, instance_key, deposed_key)
		) WITHOUT ROWID
	`)
	if err != nil {
		t.Fatal(err)
	}

	resources := []struct {
		module, typ, name, hash, refs string
	}{
		{"module.pub", "apko_build", "this", "aaa111bbb222", "[]"},
		{"module.pub", "cosign_sign", "this", "ccc333ddd444", `["module.pub.apko_build.this"]`},
		{"module.pub", "cosign_attest", "this", "eee555fff666", `["module.pub.apko_build.this"]`},
		{"module.test", "imagetest_tests", "basic", "ggg777hhh888", `["module.pub.apko_build.this"]`},
		{"module.tag", "oci_tags", "this", "iii999jjj000", `["module.pub.apko_build.this"]`},
	}
	for _, r := range resources {
		_, err := db.Exec(`
			INSERT INTO resource_instances (workspace, module, mode, type, name, content_hash, refs, cached_at)
			VALUES ('default', ?, 'managed', ?, ?, ?, ?, '2025-01-01T00:00:00Z')
		`, r.module, r.typ, r.name, r.hash, r.refs)
		if err != nil {
			t.Fatal(err)
		}
	}
}
