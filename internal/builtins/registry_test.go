// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package builtins

import (
	"testing"
)

func TestRegistry_Empty(t *testing.T) {
	entries := Registry()
	if len(entries) != 0 {
		t.Errorf("expected empty registry, got %d entries", len(entries))
	}
}
