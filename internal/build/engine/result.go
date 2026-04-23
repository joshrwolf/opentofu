// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"context"

	"github.com/opentofu/opentofu/internal/build/digest"
)

type RunResult struct {
	OutputKey digest.Digest
	Payload   []byte
}

type Result struct {
	Record Record
	Cached bool
}

type Runner interface {
	Run(context.Context, Spec) (RunResult, error)
}

type RunnerFunc func(context.Context, Spec) (RunResult, error)

func (f RunnerFunc) Run(ctx context.Context, spec Spec) (RunResult, error) {
	return f(ctx, spec)
}
