// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package digest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type Digest [sha256.Size]byte

func FromBytes(src []byte) Digest {
	return sha256.Sum256(src)
}

func FromString(src string) Digest {
	return FromBytes([]byte(src))
}

func FromStrings(parts ...string) Digest {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}

	var ret Digest
	copy(ret[:], h.Sum(nil))
	return ret
}

func FromValue(v any) (Digest, error) {
	src, err := json.Marshal(v)
	if err != nil {
		return Digest{}, err
	}
	return FromBytes(src), nil
}

func (d Digest) String() string {
	return hex.EncodeToString(d[:])
}
