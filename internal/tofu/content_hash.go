// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// ComputeContentHash computes a content-addressed hash for a resource instance
// based on its resource type, evaluated configuration value, and the content
// hashes of its upstream dependencies.
//
// This is the core mechanism of the build execution mode's caching: if a
// resource's content hash matches the one stored in state, the provider call
// can be skipped entirely and the cached output reused.
//
// The hash includes dependency hashes so that changes cascade correctly through
// the graph. If an upstream resource is re-executed (producing a new hash),
// all downstream resources will compute different hashes even if their direct
// config hasn't changed (because the dependency hash component changed).
func ComputeContentHash(resourceType string, configVal cty.Value, depHashes map[string]string) string {
	h := sha256.New()

	// Include the resource type so that different resource types with
	// identical configs don't collide.
	h.Write([]byte(resourceType))
	h.Write([]byte{0}) // null separator

	// Marshal the fully-evaluated config to JSON for hashing. This captures
	// all input attributes after expression evaluation, including interpolated
	// values from upstream resource outputs.
	//
	// We strip marks (sensitive, etc.) before marshaling since they don't
	// affect the actual values and shouldn't influence the hash.
	unmarked, _ := configVal.UnmarkDeep()
	unmarked = cty.UnknownAsNull(unmarked)

	if configJSON, err := ctyjson.Marshal(unmarked, unmarked.Type()); err == nil {
		h.Write(configJSON)
	} else {
		// If we can't marshal, include the error string so the hash at least
		// varies from a successfully-marshaled empty config.
		h.Write([]byte("marshal_error:" + err.Error()))
	}
	h.Write([]byte{0})

	// Include upstream dependency hashes in sorted order for determinism.
	if len(depHashes) > 0 {
		keys := slices.Sorted(maps.Keys(depHashes))
		for _, k := range keys {
			h.Write([]byte(k))
			h.Write([]byte{0})
			h.Write([]byte(depHashes[k]))
			h.Write([]byte{0})
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}
