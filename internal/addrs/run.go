// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package addrs

// Run is the address of a native "run" block in the current module.
type Run struct {
	referenceable
	Name string
}

func (r Run) String() string {
	return "run." + r.Name
}

func (r Run) Equal(other Run) bool {
	return r.Name == other.Name
}

func (r Run) UniqueKey() UniqueKey {
	return r
}

func (r Run) uniqueKeySigil() {}
