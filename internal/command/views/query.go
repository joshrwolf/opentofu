// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package views

import "github.com/opentofu/opentofu/internal/tfdiags"

type Query interface {
	Result(string)
	Diagnostics(tfdiags.Diagnostics)
}

func NewQuery(view *View) Query {
	return &QueryHuman{view: view}
}

type QueryHuman struct {
	view *View
}

func (v *QueryHuman) Result(result string) {
	_, _ = v.view.streams.Println(result)
}

func (v *QueryHuman) Diagnostics(diags tfdiags.Diagnostics) {
	v.view.Diagnostics(diags)
}
