// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"errors"

	"github.com/opentofu/opentofu/internal/tfdiags"
)

var ErrDeferred = errors.New("deferred")

type DeferredError struct {
	Detail string
}

func (e *DeferredError) Error() string { return e.Detail }
func (e *DeferredError) Unwrap() error { return ErrDeferred }

var errEmptyAncestor = errors.New("ancestor module has zero instances")

type diagsError struct {
	diags tfdiags.Diagnostics
}

func (e *diagsError) Error() string {
	if err := e.diags.Err(); err != nil {
		return err.Error()
	}
	return "build diagnostics"
}

func wrapDiags(diags tfdiags.Diagnostics) error {
	if len(diags) == 0 {
		return nil
	}
	return &diagsError{diags}
}

func unwrapDiags(err error) tfdiags.Diagnostics {
	if err == nil {
		return nil
	}
	if de, ok := errors.AsType[*diagsError](err); ok {
		return de.diags
	}
	return tfdiags.Diagnostics{}.Append(err)
}
