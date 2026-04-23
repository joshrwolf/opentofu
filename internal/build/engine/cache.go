// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/opentofu/opentofu/internal/build/digest"
)

type Cache interface {
	Load(context.Context, digest.Digest) (Record, bool, error)
	Store(context.Context, Record) error
	Clear(context.Context) error
}

type FileCache struct {
	Root string
}

type fileRecord struct {
	Version   string `json:"version"`
	ActionKey string `json:"action_key"`
	OutputKey string `json:"output_key"`
	Payload   string `json:"payload"`
	Volatile  bool   `json:"volatile"`
}

func NewFileCache(root string) *FileCache {
	return &FileCache{Root: root}
}

func (c *FileCache) Load(_ context.Context, key digest.Digest) (Record, bool, error) {
	if c == nil || c.Root == "" {
		return Record{}, false, nil
	}

	src, err := os.ReadFile(c.recordPath(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("build cache: failed to read entry %s: %w", key, err)
	}

	var stored fileRecord
	if err := json.Unmarshal(src, &stored); err != nil {
		return Record{}, false, fmt.Errorf("build cache: failed to decode entry %s: %w", key, err)
	}
	if stored.Version != "build-cache-v1" {
		return Record{}, false, fmt.Errorf("build cache: entry %s has unsupported version %q", key, stored.Version)
	}

	actionKey, err := parseDigest(stored.ActionKey)
	if err != nil {
		return Record{}, false, fmt.Errorf("build cache: entry %s has invalid action key: %w", key, err)
	}
	if actionKey != key {
		return Record{}, false, fmt.Errorf("build cache: entry %s stored mismatched action key %s", key, actionKey)
	}
	outputKey, err := parseDigest(stored.OutputKey)
	if err != nil {
		return Record{}, false, fmt.Errorf("build cache: entry %s has invalid output key: %w", key, err)
	}
	payload, err := base64.StdEncoding.DecodeString(stored.Payload)
	if err != nil {
		return Record{}, false, fmt.Errorf("build cache: entry %s has invalid payload encoding: %w", key, err)
	}

	return Record{
		ActionKey: actionKey,
		OutputKey: outputKey,
		Payload:   payload,
		Volatile:  stored.Volatile,
	}, true, nil
}

func (c *FileCache) Store(_ context.Context, record Record) error {
	if c == nil || c.Root == "" {
		return nil
	}

	path := c.recordPath(record.ActionKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("build cache: failed to create directory for %s: %w", record.ActionKey, err)
	}

	src, err := json.Marshal(fileRecord{
		Version:   "build-cache-v1",
		ActionKey: record.ActionKey.String(),
		OutputKey: record.OutputKey.String(),
		Payload:   base64.StdEncoding.EncodeToString(record.Payload),
		Volatile:  record.Volatile,
	})
	if err != nil {
		return fmt.Errorf("build cache: failed to encode entry for %s: %w", record.ActionKey, err)
	}

	if err := os.WriteFile(path, src, 0o644); err != nil {
		return fmt.Errorf("build cache: failed to write entry for %s: %w", record.ActionKey, err)
	}
	return nil
}

func (c *FileCache) Clear(_ context.Context) error {
	if c == nil || c.Root == "" {
		return nil
	}
	if err := os.RemoveAll(c.Root); err != nil {
		return fmt.Errorf("build cache: failed to clear at %s: %w", c.Root, err)
	}
	return nil
}

func (c *FileCache) recordPath(key digest.Digest) string {
	name := key.String()
	return filepath.Join(c.Root, name[:2], name[2:]+".json")
}

func parseDigest(src string) (digest.Digest, error) {
	raw, err := hex.DecodeString(src)
	if err != nil {
		return digest.Digest{}, err
	}
	if len(raw) != len(digest.Digest{}) {
		return digest.Digest{}, fmt.Errorf("wrong digest size %d", len(raw))
	}

	var ret digest.Digest
	copy(ret[:], raw)
	return ret, nil
}
