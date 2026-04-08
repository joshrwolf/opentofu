// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"slices"
	"sync"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
)

// HashIndex provides O(1) content hash lookups indexed by resource address.
// This replaces the legacy O(n²) prefix-scan approach.
type HashIndex struct {
	mu    sync.RWMutex
	byRes map[string][]instHash // AbsResource.String() → per-instance hashes
}

type instHash struct {
	Key  addrs.InstanceKey
	Hash string
}

// NewHashIndex creates an empty hash index.
func NewHashIndex() *HashIndex {
	return &HashIndex{
		byRes: make(map[string][]instHash),
	}
}

// Record stores the content hash for a resource instance.
func (idx *HashIndex) Record(addr addrs.AbsResourceInstance, hash string) {
	resKey := addr.ContainingResource().String()
	idx.mu.Lock()
	idx.byRes[resKey] = append(idx.byRes[resKey], instHash{
		Key:  addr.Resource.Key,
		Hash: hash,
	})
	idx.mu.Unlock()
}

// CollectDependencyHashes gathers content hashes for all instances of
// the given upstream resources. This is O(deps × k) where k is the
// average instance count per resource — not O(deps × N).
func (idx *HashIndex) CollectDependencyHashes(deps []addrs.AbsResource) map[string]string {
	hashes := make(map[string]string)
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for _, dep := range deps {
		for _, h := range idx.byRes[dep.String()] {
			inst := dep.Instance(h.Key)
			hashes[inst.String()] = h.Hash
		}
	}
	return hashes
}

// ContentHashHex returns the hex representation of a content hash string.
// Since ComputeContentHash already returns hex, this is an identity function
// provided as an API convenience for external callers.
func ContentHashHex(h string) string { return h }

// ComputeContentHash produces a deterministic hash from the resource type,
// evaluated config value, and upstream dependency hashes. Changes in any
// input cascade to a different hash.
//
// The config value is hashed via a streaming tree walk that writes directly
// to SHA-256, avoiding the intermediate JSON serialization and the expensive
// big.Float.Text('f', -1) → roundShortest path used by cty/json.Marshal.
func ComputeContentHash(resourceType string, configVal cty.Value, depHashes map[string]string) string {
	h := sha256.New()

	h.Write([]byte(resourceType))
	h.Write([]byte{0})

	var buf [64]byte
	hashValue(h, configVal, buf[:0])
	h.Write([]byte{0})

	// Sort dependency keys for deterministic ordering.
	keys := make([]string, 0, len(depHashes))
	for k := range depHashes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(depHashes[k]))
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))
}

// Value type tags for the deterministic hasher. Each tag uniquely identifies
// the value kind so that structurally different values (e.g., string "1" vs
// number 1) never produce the same hash input.
const (
	htNull   byte = 0x00
	htFalse  byte = 0x01
	htTrue   byte = 0x02
	htNumber byte = 0x03
	htString byte = 0x04
	htList   byte = 0x05
	htMap    byte = 0x06
	htSet    byte = 0x07
	htEnd    byte = 0xFF
)

// hashValue writes a deterministic binary representation of val to h,
// streaming directly without building an intermediate byte buffer.
//
// Marks are stripped at each recursion level (equivalent to UnmarkDeep
// but amortized). Unknown values are treated as null — for content hashing,
// "not yet known" means "unchanged from previous state."
//
// The buf parameter is scratch space for number formatting, returned for
// reuse by the caller.
//
// Correctness invariant: for any two values a and b, if
//
//	a.UnmarkDeep().RawEquals(cty.UnknownAsNull(b.UnmarkDeep()))
//
// then hashValue(h, a, buf) and hashValue(h, b, buf) write identical bytes
// to h.
func hashValue(h hash.Hash, val cty.Value, buf []byte) []byte {
	// Strip marks at each level — amortizes UnmarkDeep's full-tree walk.
	val, _ = val.Unmark()

	if !val.IsKnown() {
		h.Write([]byte{htNull})
		return buf
	}
	if val.IsNull() {
		h.Write([]byte{htNull})
		return buf
	}

	ty := val.Type()

	switch {
	case ty == cty.Bool:
		if val.True() {
			h.Write([]byte{htTrue})
		} else {
			h.Write([]byte{htFalse})
		}

	case ty == cty.Number:
		h.Write([]byte{htNumber})
		// Use 'g' format with explicit precision (20 significant digits).
		// This avoids the expensive roundShortest path that Text('f', -1)
		// takes — roundShortest is O(precision²) via repeated big.Int
		// division, costing 3.2s of CPU on a full images-private build.
		//
		// 20 significant digits exceeds float64 precision (15-17 digits)
		// and covers all practical Terraform number values.
		buf = val.AsBigFloat().Append(buf[:0], 'g', 20)
		h.Write(buf)

	case ty == cty.String:
		h.Write([]byte{htString})
		s := val.AsString()
		// Length-prefix strings to prevent key/value boundary ambiguity.
		buf = binary.BigEndian.AppendUint32(buf[:0], uint32(len(s)))
		h.Write(buf[:4])
		h.Write([]byte(s))

	case ty.IsListType() || ty.IsTupleType():
		h.Write([]byte{htList})
		for it := val.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			buf = hashValue(h, elem, buf)
		}
		h.Write([]byte{htEnd})

	case ty.IsSetType():
		h.Write([]byte{htSet})
		buf = hashSetElements(h, val, buf)
		h.Write([]byte{htEnd})

	case ty.IsMapType() || ty.IsObjectType():
		h.Write([]byte{htMap})
		buf = hashMapElements(h, val, buf)
		h.Write([]byte{htEnd})

	default:
		// Capsule types or anything unexpected — hash as null.
		h.Write([]byte{htNull})
	}

	return buf
}

// hashMapElements writes map/object key-value pairs in sorted key order.
// Go map iteration is non-deterministic, so explicit sorting is required.
//
// Note: the previous JSON-based hasher (via cty/json.Marshal) did NOT sort
// map keys — only object attribute names were sorted. This was a latent
// non-determinism bug for resources with map-typed attributes. The direct
// hasher fixes this.
func hashMapElements(h hash.Hash, val cty.Value, buf []byte) []byte {
	vals := val.AsValueMap()
	if len(vals) == 0 {
		return buf
	}

	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	for _, k := range keys {
		// Length-prefixed key.
		buf = binary.BigEndian.AppendUint32(buf[:0], uint32(len(k)))
		h.Write(buf[:4])
		h.Write([]byte(k))
		buf = hashValue(h, vals[k], buf)
	}

	return buf
}

// hashSetElements writes set elements in canonical order. cty set iteration
// order depends on internal bucket hashing and is not guaranteed stable
// across Go versions. We hash each element independently, sort the digests,
// and feed the sorted sequence into the parent hasher.
func hashSetElements(h hash.Hash, val cty.Value, buf []byte) []byte {
	n := val.LengthInt()
	if n == 0 {
		return buf
	}

	digests := make([][sha256.Size]byte, 0, n)
	scratch := sha256.New()

	for it := val.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		scratch.Reset()
		buf = hashValue(scratch, elem, buf)
		var d [sha256.Size]byte
		scratch.Sum(d[:0])
		digests = append(digests, d)
	}

	slices.SortFunc(digests, func(a, b [sha256.Size]byte) int {
		return bytes.Compare(a[:], b[:])
	})

	for i := range digests {
		h.Write(digests[i][:])
	}

	return buf
}
