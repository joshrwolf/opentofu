// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"
	"database/sql"
)

// initDB creates the schema tables if they don't exist.
// PRAGMAs are set per-connection via the DSN in openDB, not here.
func initDB(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, dbSchema)
	return err
}

const dbSchema = `
CREATE TABLE IF NOT EXISTS metadata (
	workspace TEXT NOT NULL,
	key       TEXT NOT NULL,
	value     TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (workspace, key)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS resource_instances (
	workspace          TEXT NOT NULL DEFAULT 'default',
	module             TEXT NOT NULL DEFAULT '',
	mode               TEXT NOT NULL,
	type               TEXT NOT NULL,
	name               TEXT NOT NULL,
	instance_key       TEXT NOT NULL DEFAULT '',
	deposed_key        TEXT NOT NULL DEFAULT '',
	status             TEXT NOT NULL DEFAULT 'ready',
	provider           TEXT NOT NULL DEFAULT '',
	schema_version     INTEGER NOT NULL DEFAULT 0,
	attributes         BLOB,
	attributes_flat    BLOB,
	sensitive_paths    BLOB,
	private_raw        BLOB,
	dependencies       TEXT,
	create_before_destroy INTEGER NOT NULL DEFAULT 0,
	skip_destroy       INTEGER NOT NULL DEFAULT 0,
	identity           BLOB,
	identity_schema_version INTEGER,
	content_hash       TEXT NOT NULL DEFAULT '',
	cached_at          TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (workspace, module, mode, type, name, instance_key, deposed_key)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS outputs (
	workspace TEXT NOT NULL DEFAULT 'default',
	name      TEXT NOT NULL,
	value     BLOB,
	type      BLOB,
	sensitive INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (workspace, name)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS locks (
	workspace TEXT NOT NULL PRIMARY KEY,
	lock_id   TEXT NOT NULL,
	info      TEXT NOT NULL,
	created   TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS check_results (
	workspace   TEXT NOT NULL DEFAULT 'default',
	object_kind TEXT NOT NULL,
	config_addr TEXT NOT NULL,
	status      TEXT NOT NULL,
	elements    BLOB,
	PRIMARY KEY (workspace, object_kind, config_addr)
) WITHOUT ROWID;
`
