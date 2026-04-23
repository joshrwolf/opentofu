// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package engine

import "github.com/opentofu/opentofu/internal/build/digest"

type LockKey string

type SourcePos struct {
	Line   int
	Column int
	Byte   int
}

type SourceRef struct {
	Filename string
	Start    SourcePos
	End      SourcePos
}

type RunnerSpec struct {
	Kind    string
	Payload any
}

type Spec struct {
	Key       digest.Digest
	Name      string
	Class     string
	ExecDeps  []digest.Digest
	After     []digest.Digest
	Locks     []LockKey
	Cacheable bool
	Volatile  bool
	Runner    RunnerSpec
	Source    SourceRef
}
