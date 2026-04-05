// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type QueryCommand struct {
	Meta
}

func (c *QueryCommand) Run(rawArgs []string) int {
	if len(rawArgs) < 1 {
		c.showError("Usage: tofu query <subcommand> [args]\n\nSubcommands: deps, rdeps, ls, status")
		return 1
	}

	db, err := c.openStateDB()
	if err != nil {
		c.showError("Error opening state database: %s", err)
		return 1
	}
	defer db.Close()

	switch rawArgs[0] {
	case "deps":
		return c.queryDeps(db, rawArgs[1:])
	case "rdeps":
		return c.queryRdeps(db, rawArgs[1:])
	case "ls":
		return c.queryLs(db, rawArgs[1:])
	case "status":
		return c.queryStatus(db)
	default:
		c.showError("Unknown subcommand: %s", rawArgs[0])
		return 1
	}
}

func (c *QueryCommand) openStateDB() (*sql.DB, error) {
	dbPath := filepath.Join(".chofu", "state.db")
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", dbPath))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w (run 'tofu build' first)", dbPath, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w (run 'tofu build' first)", dbPath, err)
	}
	return db, nil
}

func (c *QueryCommand) queryDeps(db *sql.DB, args []string) int {
	if len(args) < 1 {
		c.showError("Usage: tofu query deps <resource-pattern>")
		return 1
	}
	pattern := "%" + args[0] + "%"

	rows, err := db.Query(`
		SELECT module, type, name, instance_key, refs
		FROM resource_instances
		WHERE workspace = 'default'
		  AND (type || '.' || name LIKE ? OR module || '.' || type || '.' || name LIKE ?)
		  AND refs IS NOT NULL AND refs != ''
	`, pattern, pattern)
	if err != nil {
		c.showError("Query error: %s", err)
		return 1
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		found = true
		var module, resType, name, instanceKey, refsJSON string
		if err := rows.Scan(&module, &resType, &name, &instanceKey, &refsJSON); err != nil {
			c.showError("Error reading row: %s", err)
			return 1
		}

		addr := formatAddr(module, resType, name, instanceKey)
		var refs []string
		if err := json.Unmarshal([]byte(refsJSON), &refs); err != nil {
			c.showError("Error parsing refs for %s: %s", addr, err)
			return 1
		}

		c.output("%s depends on:", addr)
		for _, ref := range refs {
			c.output("  %s", ref)
		}
	}
	if err := rows.Err(); err != nil {
		c.showError("Query error: %s", err)
		return 1
	}
	if !found {
		c.showError("No resources matching %q", args[0])
		return 1
	}
	return 0
}

func (c *QueryCommand) queryRdeps(db *sql.DB, args []string) int {
	if len(args) < 1 {
		c.showError("Usage: tofu query rdeps <resource-pattern>")
		return 1
	}

	// Use json_each to properly search inside the JSON array, avoiding
	// false positives from substring matching.
	rows, err := db.Query(`
		SELECT ri.module, ri.type, ri.name, ri.instance_key
		FROM resource_instances ri, json_each(ri.refs) je
		WHERE ri.workspace = 'default' AND je.value LIKE ?
	`, "%"+args[0]+"%")
	if err != nil {
		c.showError("Query error: %s", err)
		return 1
	}
	defer rows.Close()

	c.output("Resources that depend on %q:", args[0])
	found := false
	for rows.Next() {
		found = true
		var module, resType, name, instanceKey string
		if err := rows.Scan(&module, &resType, &name, &instanceKey); err != nil {
			c.showError("Error reading row: %s", err)
			return 1
		}
		c.output("  %s", formatAddr(module, resType, name, instanceKey))
	}
	if err := rows.Err(); err != nil {
		c.showError("Query error: %s", err)
		return 1
	}
	if !found {
		c.output("  (none)")
	}
	return 0
}

func (c *QueryCommand) queryLs(db *sql.DB, args []string) int {
	query := `SELECT module, mode, type, name, instance_key, content_hash
		FROM resource_instances WHERE workspace = 'default'`
	var qargs []any
	if len(args) > 0 {
		query += ` AND type LIKE ?`
		qargs = append(qargs, "%"+args[0]+"%")
	}
	query += ` ORDER BY module, type, name, instance_key`

	rows, err := db.Query(query, qargs...)
	if err != nil {
		c.showError("Query error: %s", err)
		return 1
	}
	defer rows.Close()

	for rows.Next() {
		var module, mode, resType, name, instanceKey, hash string
		if err := rows.Scan(&module, &mode, &resType, &name, &instanceKey, &hash); err != nil {
			c.showError("Error reading row: %s", err)
			return 1
		}

		addr := formatAddr(module, resType, name, instanceKey)
		prefix := "resource"
		if mode == "data" {
			prefix = "data"
		}
		hashInfo := ""
		if len(hash) >= 12 {
			hashInfo = fmt.Sprintf("  %s…", hash[:12])
		} else if hash != "" {
			hashInfo = fmt.Sprintf("  %s", hash)
		}
		c.output("%s %s%s", prefix, addr, hashInfo)
	}
	if err := rows.Err(); err != nil {
		c.showError("Query error: %s", err)
		return 1
	}
	return 0
}

func (c *QueryCommand) queryStatus(db *sql.DB) int {
	rows, err := db.Query(`
		SELECT module, type, name, instance_key, content_hash, cached_at
		FROM resource_instances
		WHERE workspace = 'default' AND content_hash != ''
		ORDER BY module, type, name, instance_key
	`)
	if err != nil {
		c.showError("Query error: %s", err)
		return 1
	}
	defer rows.Close()

	var total, cached int
	for rows.Next() {
		var module, resType, name, instanceKey, hash, cachedAt string
		if err := rows.Scan(&module, &resType, &name, &instanceKey, &hash, &cachedAt); err != nil {
			c.showError("Error reading row: %s", err)
			return 1
		}
		total++
		if cachedAt != "" {
			cached++
		}
		hashDisplay := hash
		if len(hash) >= 12 {
			hashDisplay = hash[:12] + "…"
		}
		c.output("%-6s %s  %s", "CACHED", formatAddr(module, resType, name, instanceKey), hashDisplay)
	}
	if err := rows.Err(); err != nil {
		c.showError("Query error: %s", err)
		return 1
	}
	c.output("\n%d/%d cached", cached, total)
	return 0
}

func (c *QueryCommand) output(format string, args ...any) {
	fmt.Fprintf(os.Stdout, format+"\n", args...)
}

func (c *QueryCommand) showError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func formatAddr(module, resType, name, instanceKey string) string {
	var addr string
	if module != "" {
		addr = module + "."
	}
	addr += resType + "." + name
	if instanceKey != "" {
		switch instanceKey[0] {
		case 'i':
			addr += fmt.Sprintf("[%s]", instanceKey[1:])
		case 's':
			addr += fmt.Sprintf("[%q]", instanceKey[1:])
		}
	}
	return addr
}

func (c *QueryCommand) Help() string {
	return strings.TrimSpace(`
Usage: tofu query <subcommand> [args]

  Query the build graph stored in the state database.

Subcommands:

  deps <pattern>     Show what a resource depends on.
  rdeps <pattern>    Show what depends on a resource.
  ls [type]          List resources, optionally filtered by type.
  status             Show cache status for all resources.

Examples:

  tofu query deps apko_build
  tofu query rdeps apko_build.this
  tofu query ls cosign
  tofu query status
`)
}

func (c *QueryCommand) Synopsis() string {
	return "Query the build graph"
}
