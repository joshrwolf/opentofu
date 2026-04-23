// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"github.com/opentofu/opentofu/internal/build/digest"
)

type Record struct {
	ActionKey digest.Digest
	OutputKey digest.Digest
	Payload   []byte
	Volatile  bool
}
