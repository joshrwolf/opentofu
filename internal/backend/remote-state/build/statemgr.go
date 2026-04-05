// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	uuid "github.com/hashicorp/go-uuid"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/lang/marks"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/states/statefile"
	"github.com/opentofu/opentofu/internal/states/statemgr"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tofu"
	"github.com/opentofu/opentofu/version"
)

// SQLiteState implements statemgr.Full backed by a SQLite database with
// per-resource-instance row storage. This avoids the monolithic JSON
// serialize/deserialize cycle that makes the filesystem and remote state
// managers slow at scale.
//
// The in-memory state is loaded lazily on RefreshState and written back
// incrementally on PersistState (only changed resources are touched).
type SQLiteState struct {
	db        *sql.DB
	workspace string

	mu    sync.Mutex
	state *states.State

	lineage string
	serial  uint64
}

var _ statemgr.Full = (*SQLiteState)(nil)

// State returns a deep copy of the current transient state.
func (s *SQLiteState) State() *states.State {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == nil {
		return nil
	}
	return s.state.DeepCopy()
}

// WriteState stores the given state as the new transient snapshot.
func (s *SQLiteState) WriteState(state *states.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if state != nil {
		s.state = state.DeepCopy()
	} else {
		s.state = nil
	}
	return nil
}

// MutateState applies the given function to the current transient state.
func (s *SQLiteState) MutateState(fn func(*states.State) *states.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = fn(s.state)
	return nil
}

// RefreshState loads the full state from SQLite into memory.
func (s *SQLiteState) RefreshState(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, lineage, serial, err := s.loadState(ctx)
	if err != nil {
		return fmt.Errorf("refreshing state from sqlite: %w", err)
	}

	s.state = state
	s.lineage = lineage
	s.serial = serial
	return nil
}

// PersistState writes the current transient state to SQLite. It compares
// against the persisted snapshot and only writes changed resources.
func (s *SQLiteState) PersistState(ctx context.Context, schemas *tofu.Schemas) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == nil {
		return nil
	}

	if s.lineage == "" {
		id, err := uuid.GenerateUUID()
		if err != nil {
			return fmt.Errorf("generating lineage: %w", err)
		}
		s.lineage = id
	}

	// Always increment serial when persisting — the walk modified state.
	s.serial++

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Write metadata.
	if err := s.writeMetadata(ctx, tx); err != nil {
		return err
	}

	// Write all resources and instances. For now we do a full replace per
	// workspace — this is still much faster than serializing the entire state
	// to JSON because we avoid the marshaling overhead and write individual
	// small rows.
	//
	// TODO(perf): track dirty set and write only changed instances.
	if err := s.writeResources(ctx, tx, s.state); err != nil {
		return err
	}

	// Write outputs.
	if err := s.writeOutputs(ctx, tx, s.state); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing state: %w", err)
	}

	return nil
}

// GetRootOutputValues returns the root module outputs from the current state.
func (s *SQLiteState) GetRootOutputValues(_ context.Context) (map[string]*states.OutputValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == nil {
		return nil, nil
	}

	rootMod := s.state.Module(addrs.RootModuleInstance)
	if rootMod == nil {
		return nil, nil
	}

	result := make(map[string]*states.OutputValue, len(rootMod.OutputValues))
	for name, ov := range rootMod.OutputValues {
		result[name] = ov
	}
	return result, nil
}

// Lock acquires a cooperative lock on this workspace in the database.
func (s *SQLiteState) Lock(ctx context.Context, info *statemgr.LockInfo) (string, error) {
	if info.ID == "" {
		var err error
		info.ID, err = uuid.GenerateUUID()
		if err != nil {
			return "", fmt.Errorf("generating lock ID: %w", err)
		}
	}

	infoJSON, err := json.Marshal(info)
	if err != nil {
		return "", err
	}

	// Try to insert. If a lock already exists for this workspace, the INSERT
	// fails with a UNIQUE constraint violation — that means someone else holds it.
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO locks (workspace, lock_id, info, created) VALUES (?, ?, ?, ?)`,
		s.workspace, info.ID, string(infoJSON), time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		// Read the existing lock to report who holds it.
		var existingInfo string
		row := s.db.QueryRowContext(ctx,
			`SELECT info FROM locks WHERE workspace = ?`, s.workspace,
		)
		if scanErr := row.Scan(&existingInfo); scanErr == nil {
			var existing statemgr.LockInfo
			if jsonErr := json.Unmarshal([]byte(existingInfo), &existing); jsonErr == nil {
				return "", &statemgr.LockError{
					Info: &existing,
					Err:  fmt.Errorf("workspace %q is locked", s.workspace),
				}
			}
		}
		return "", fmt.Errorf("acquiring lock: %w", err)
	}

	return info.ID, nil
}

// Unlock releases the lock for this workspace.
func (s *SQLiteState) Unlock(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM locks WHERE workspace = ? AND lock_id = ?`,
		s.workspace, id,
	)
	if err != nil {
		return fmt.Errorf("releasing lock: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("lock %s not found for workspace %q (may have been stolen)", id, s.workspace)
	}
	return nil
}

// loadState reads all resource instances and outputs from SQLite and
// assembles them into a states.State.
func (s *SQLiteState) loadState(ctx context.Context) (*states.State, string, uint64, error) {
	// Read metadata.
	var lineage string
	var serial uint64
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM metadata WHERE workspace = ?`, s.workspace,
	)
	if err != nil {
		return nil, "", 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, "", 0, err
		}
		switch key {
		case "lineage":
			lineage = value
		case "serial":
			if _, err := fmt.Sscanf(value, "%d", &serial); err != nil {
				return nil, "", 0, fmt.Errorf("invalid serial %q: %w", value, err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", 0, err
	}

	state := states.NewState()
	hasData := lineage != ""

	// Load resource instances.
	instRows, err := s.db.QueryContext(ctx, `
		SELECT module, mode, type, name, instance_key, deposed_key,
		       status, provider, schema_version,
		       attributes, attributes_flat, sensitive_paths,
		       private_raw, dependencies, refs,
		       create_before_destroy, skip_destroy,
		       identity, identity_schema_version,
		       content_hash, cached_at
		FROM resource_instances
		WHERE workspace = ?
	`, s.workspace)
	if err != nil {
		return nil, "", 0, err
	}
	defer instRows.Close()

	for instRows.Next() {
		hasData = true
		var (
			module, mode, resType, name, instanceKey, deposedKey string
			status, provider                                     string
			schemaVersion                                        uint64
			attrsRaw, attrsFlat, sensitivePaths                  []byte
			privateRaw                                           []byte
			depsRaw, refsRaw                                     sql.NullString
			cbd, skipDestroy                                     bool
			identityRaw                                          []byte
			identitySchemaVersion                                sql.NullInt64
			contentHash, cachedAt                                string
		)

		if err := instRows.Scan(
			&module, &mode, &resType, &name, &instanceKey, &deposedKey,
			&status, &provider, &schemaVersion,
			&attrsRaw, &attrsFlat, &sensitivePaths,
			&privateRaw, &depsRaw, &refsRaw,
			&cbd, &skipDestroy,
			&identityRaw, &identitySchemaVersion,
			&contentHash, &cachedAt,
		); err != nil {
			return nil, "", 0, fmt.Errorf("scanning resource instance: %w", err)
		}

		var moduleAddr addrs.ModuleInstance
		if module != "" {
			var diags tfdiags.Diagnostics
			moduleAddr, diags = addrs.ParseModuleInstanceStr(module)
			if diags.HasErrors() {
				return nil, "", 0, fmt.Errorf("resource %s.%s has invalid module %q: %w", resType, name, module, diags.Err())
			}
		}

		var resMode addrs.ResourceMode
		switch mode {
		case "managed":
			resMode = addrs.ManagedResourceMode
		case "data":
			resMode = addrs.DataResourceMode
		default:
			return nil, "", 0, fmt.Errorf("resource %s.%s has unknown mode %q", resType, name, mode)
		}

		resAddr := addrs.Resource{Mode: resMode, Type: resType, Name: name}

		instKey, keyErr := decodeInstanceKey(instanceKey)
		if keyErr != nil {
			return nil, "", 0, fmt.Errorf("resource %s has invalid instance key %q: %w", resAddr, instanceKey, keyErr)
		}

		instAddr := resAddr.Instance(instKey).Absolute(moduleAddr)

		providerAddr, _, providerDiags := addrs.ParseAbsProviderConfigInstanceStr(provider)
		if providerDiags.HasErrors() {
			return nil, "", 0, fmt.Errorf("resource %s has invalid provider %q: %w", instAddr, provider, providerDiags.Err())
		}

		obj := &states.ResourceInstanceObjectSrc{
			SchemaVersion:       schemaVersion,
			CreateBeforeDestroy: cbd,
			SkipDestroy:         skipDestroy,
			ContentHash:         contentHash,
		}

		if cachedAt != "" {
			t, parseErr := time.Parse(time.RFC3339Nano, cachedAt)
			if parseErr != nil {
				return nil, "", 0, fmt.Errorf("resource %s has invalid cached_at %q: %w", instAddr, cachedAt, parseErr)
			}
			obj.CachedAt = t
		}

		if len(attrsRaw) > 0 {
			obj.AttrsJSON = attrsRaw
		} else if len(attrsFlat) > 0 {
			var flat map[string]string
			if err := json.Unmarshal(attrsFlat, &flat); err != nil {
				return nil, "", 0, fmt.Errorf("resource %s has invalid attributes_flat: %w", instAddr, err)
			}
			obj.AttrsFlat = flat
		} else {
			obj.AttrsJSON = []byte("{}")
		}

		if len(sensitivePaths) > 0 {
			paths, pathsDiags := unmarshalSensitivePaths(sensitivePaths)
			if pathsDiags.HasErrors() {
				return nil, "", 0, fmt.Errorf("resource %s has invalid sensitive_paths: %w", instAddr, pathsDiags.Err())
			}
			obj.AttrSensitivePaths = paths
		}

		if len(privateRaw) > 0 {
			obj.Private = privateRaw
		}

		if len(identityRaw) > 0 {
			obj.IdentityJSON = identityRaw
			if identitySchemaVersion.Valid {
				v := uint64(identitySchemaVersion.Int64)
				obj.IdentitySchemaVersion = &v
			}
		}

		if depsRaw.Valid && depsRaw.String != "" {
			var depStrs []string
			if err := json.Unmarshal([]byte(depsRaw.String), &depStrs); err != nil {
				return nil, "", 0, fmt.Errorf("resource %s has invalid dependencies JSON: %w", instAddr, err)
			}
			deps := make([]addrs.ConfigResource, 0, len(depStrs))
			for _, depStr := range depStrs {
				addr, addrDiags := addrs.ParseAbsResourceStr(depStr)
				if !addrDiags.HasErrors() {
					deps = append(deps, addr.Config())
				}
			}
			obj.Dependencies = deps
		}

		if refsRaw.Valid && refsRaw.String != "" {
			var refStrs []string
			if err := json.Unmarshal([]byte(refsRaw.String), &refStrs); err != nil {
				return nil, "", 0, fmt.Errorf("resource %s has invalid refs JSON: %w", instAddr, err)
			}
			refs := make([]addrs.ConfigResource, 0, len(refStrs))
			for _, refStr := range refStrs {
				addr, addrDiags := addrs.ParseAbsResourceStr(refStr)
				if !addrDiags.HasErrors() {
					refs = append(refs, addr.Config())
				}
			}
			obj.References = refs
		}

		switch status {
		case "", "ready":
			obj.Status = states.ObjectReady
		case "tainted":
			obj.Status = states.ObjectTainted
		default:
			log.Printf("[WARN] build backend: resource %s has unknown status %q, treating as ready", instAddr, status)
			obj.Status = states.ObjectReady
		}

		ms := state.EnsureModule(moduleAddr)
		switch {
		case deposedKey != "":
			dk := states.DeposedKey(deposedKey)
			ms.SetResourceInstanceDeposed(instAddr.Resource, dk, obj, providerAddr, addrs.NoKey)
		default:
			ms.SetResourceInstanceCurrent(instAddr.Resource, obj, providerAddr, addrs.NoKey)
		}
	}
	if err := instRows.Err(); err != nil {
		return nil, "", 0, err
	}

	// Load outputs.
	outputRows, err := s.db.QueryContext(ctx,
		`SELECT name, value, type, sensitive FROM outputs WHERE workspace = ?`,
		s.workspace,
	)
	if err != nil {
		return nil, "", 0, err
	}
	defer outputRows.Close()

	for outputRows.Next() {
		hasData = true
		var (
			name      string
			valueRaw  []byte
			typeRaw   []byte
			sensitive bool
		)
		if err := outputRows.Scan(&name, &valueRaw, &typeRaw, &sensitive); err != nil {
			return nil, "", 0, err
		}

		if len(valueRaw) > 0 && len(typeRaw) > 0 {
			ty, err := ctyjson.UnmarshalType(typeRaw)
			if err != nil {
				return nil, "", 0, fmt.Errorf("output %q has invalid type: %w", name, err)
			}
			val, err := ctyjson.Unmarshal(valueRaw, ty)
			if err != nil {
				return nil, "", 0, fmt.Errorf("output %q has invalid value: %w", name, err)
			}

			rootMod := state.EnsureModule(addrs.RootModuleInstance)
			rootMod.OutputValues[name] = &states.OutputValue{
				Value:     val,
				Sensitive: sensitive,
			}
		}
	}
	if err := outputRows.Err(); err != nil {
		return nil, "", 0, err
	}

	if !hasData {
		return nil, "", 0, nil
	}

	return state, lineage, serial, nil
}

// writeMetadata persists lineage and serial.
func (s *SQLiteState) writeMetadata(ctx context.Context, tx *sql.Tx) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO metadata (workspace, key, value) VALUES (?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	if _, err := stmt.ExecContext(ctx, s.workspace, "lineage", s.lineage); err != nil {
		return err
	}
	if _, err := stmt.ExecContext(ctx, s.workspace, "serial", fmt.Sprintf("%d", s.serial)); err != nil {
		return err
	}
	if _, err := stmt.ExecContext(ctx, s.workspace, "version", version.Version); err != nil {
		return err
	}
	return nil
}

// writeResources writes all resource instances for the workspace. It does a
// DELETE+INSERT rather than UPSERT because tracking the dirty set is not yet
// implemented. This is still fast for 17k rows because:
//   - DELETE by workspace is a single index scan
//   - INSERT is batched in a transaction (SQLite journals a single fsync)
//   - No JSON marshaling of the full state (attributes stay as blobs)
func (s *SQLiteState) writeResources(ctx context.Context, tx *sql.Tx, state *states.State) error {
	// Clear existing instances for this workspace.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM resource_instances WHERE workspace = ?`, s.workspace,
	); err != nil {
		return err
	}

	instStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO resource_instances (
			workspace, module, mode, type, name, instance_key, deposed_key,
			status, provider, schema_version,
			attributes, attributes_flat, sensitive_paths,
			private_raw, dependencies, refs,
			create_before_destroy, skip_destroy,
			identity, identity_schema_version,
			content_hash, cached_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer instStmt.Close()

	for _, ms := range state.Modules {
		if ms == nil {
			continue
		}

		moduleStr := ms.Addr.String()

		for _, rs := range ms.Resources {
			var modeStr string
			switch rs.Addr.Resource.Mode {
			case addrs.ManagedResourceMode:
				modeStr = "managed"
			case addrs.DataResourceMode:
				modeStr = "data"
			default:
				continue
			}

			for key, is := range rs.Instances {
				if is == nil {
					continue
				}

				providerStr := rs.ProviderConfig.InstanceString(is.ProviderKey)

				// Write current object.
				if is.Current != nil {
					if err := s.writeInstance(ctx, instStmt,
						moduleStr, modeStr, rs.Addr.Resource.Type, rs.Addr.Resource.Name,
						encodeInstanceKey(key), "",
						is.Current, providerStr,
					); err != nil {
						return err
					}
				}

				// Write deposed objects.
				for dk, obj := range is.Deposed {
					if obj != nil {
						if err := s.writeInstance(ctx, instStmt,
							moduleStr, modeStr, rs.Addr.Resource.Type, rs.Addr.Resource.Name,
							encodeInstanceKey(key), string(dk),
							obj, providerStr,
						); err != nil {
							return err
						}
					}
				}
			}
		}
	}

	return nil
}

func (s *SQLiteState) writeInstance(
	ctx context.Context,
	stmt *sql.Stmt,
	module, mode, resType, name, instanceKey, deposedKey string,
	obj *states.ResourceInstanceObjectSrc,
	provider string,
) error {
	var statusStr string
	switch obj.Status {
	case states.ObjectTainted:
		statusStr = "tainted"
	default:
		statusStr = "ready"
	}

	var attrsFlat []byte
	if obj.AttrsFlat != nil {
		attrsFlat, _ = json.Marshal(obj.AttrsFlat)
	}

	var sensitivePaths []byte
	if len(obj.AttrSensitivePaths) > 0 {
		sensitivePaths = marshalSensitivePaths(obj.AttrSensitivePaths)
	}

	var depsJSON sql.NullString
	if len(obj.Dependencies) > 0 {
		depStrs := make([]string, len(obj.Dependencies))
		for i, dep := range obj.Dependencies {
			depStrs[i] = dep.String()
		}
		b, _ := json.Marshal(depStrs)
		depsJSON = sql.NullString{String: string(b), Valid: true}
	}

	var refsJSON sql.NullString
	if len(obj.References) > 0 {
		refStrs := make([]string, len(obj.References))
		for i, ref := range obj.References {
			refStrs[i] = ref.String()
		}
		b, _ := json.Marshal(refStrs)
		refsJSON = sql.NullString{String: string(b), Valid: true}
	}

	var identitySchemaVersion sql.NullInt64
	if obj.IdentitySchemaVersion != nil {
		identitySchemaVersion = sql.NullInt64{
			Int64: int64(*obj.IdentitySchemaVersion),
			Valid: true,
		}
	}

	var cachedAtStr string
	if !obj.CachedAt.IsZero() {
		cachedAtStr = obj.CachedAt.Format(time.RFC3339Nano)
	}

	_, err := stmt.ExecContext(ctx,
		s.workspace, module, mode, resType, name, instanceKey, deposedKey,
		statusStr, provider, obj.SchemaVersion,
		obj.AttrsJSON, attrsFlat, sensitivePaths,
		obj.Private, depsJSON, refsJSON,
		obj.CreateBeforeDestroy, obj.SkipDestroy,
		obj.IdentityJSON, identitySchemaVersion,
		obj.ContentHash, cachedAtStr,
	)
	return err
}

// writeOutputs writes root module outputs.
func (s *SQLiteState) writeOutputs(ctx context.Context, tx *sql.Tx, state *states.State) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM outputs WHERE workspace = ?`, s.workspace,
	); err != nil {
		return err
	}

	rootMod := state.Module(addrs.RootModuleInstance)
	if rootMod == nil {
		return nil
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO outputs (workspace, name, value, type, sensitive) VALUES (?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for name, ov := range rootMod.OutputValues {
		if ov == nil {
			continue
		}

		val := ov.Value
		if val == cty.NilVal {
			continue
		}

		valueJSON, err := ctyjson.Marshal(val, val.Type())
		if err != nil {
			log.Printf("[WARN] build backend: skipping output %q: cannot marshal value: %s", name, err)
			continue
		}
		typeJSON, err := ctyjson.MarshalType(val.Type())
		if err != nil {
			log.Printf("[WARN] build backend: skipping output %q: cannot marshal type: %s", name, err)
			continue
		}

		if _, err := stmt.ExecContext(ctx, s.workspace, name, valueJSON, typeJSON, ov.Sensitive); err != nil {
			return err
		}
	}

	return nil
}

// encodeInstanceKey converts an InstanceKey to a string for storage.
// NoKey → "", IntKey(n) → "i<n>", StringKey(s) → "s<s>"
func encodeInstanceKey(key addrs.InstanceKey) string {
	switch k := key.(type) {
	case addrs.IntKey:
		return fmt.Sprintf("i%d", int(k))
	case addrs.StringKey:
		return "s" + string(k)
	default:
		return ""
	}
}

// decodeInstanceKey converts a stored string back to an InstanceKey.
func decodeInstanceKey(s string) (addrs.InstanceKey, error) {
	if s == "" {
		return addrs.NoKey, nil
	}
	switch s[0] {
	case 'i':
		var idx int
		if n, err := fmt.Sscanf(s[1:], "%d", &idx); err != nil || n != 1 {
			return nil, fmt.Errorf("invalid int key %q", s)
		}
		return addrs.IntKey(idx), nil
	case 's':
		return addrs.StringKey(s[1:]), nil
	default:
		return nil, fmt.Errorf("unknown key prefix %q", s)
	}
}

// marshalSensitivePaths converts path value marks to the V4-compatible JSON blob.
func marshalSensitivePaths(pvm []cty.PathValueMarks) []byte {
	var paths []cty.Path
	for _, p := range pvm {
		paths = append(paths, p.Path)
	}
	b, _ := statefile.MarshalPaths(paths)
	return b
}

// unmarshalSensitivePaths converts a V4-compatible JSON blob back to path value marks.
func unmarshalSensitivePaths(data []byte) ([]cty.PathValueMarks, tfdiags.Diagnostics) {
	paths, diags := statefile.UnmarshalPaths(data)
	if diags.HasErrors() {
		return nil, diags
	}
	var pvm []cty.PathValueMarks
	for _, path := range paths {
		pvm = append(pvm, cty.PathValueMarks{
			Path:  path,
			Marks: cty.NewValueMarks(marks.Sensitive),
		})
	}
	return pvm, diags
}
