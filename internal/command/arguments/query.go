// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package arguments

import (
	buildselector "github.com/opentofu/opentofu/internal/build/selector"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type Query struct {
	RawSelectors []string
	Selectors    buildselector.Set
	ReverseDeps  bool
	Vars         *Vars
}

func ParseQuery(args []string) (*Query, func(), tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	query := &Query{
		Vars: &Vars{},
	}

	cmdFlags := extendedFlagSet("query", nil, nil, query.Vars)
	cmdFlags.BoolVar(&query.ReverseDeps, "rdeps", false, "rdeps")
	if err := cmdFlags.Parse(args); err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to parse command-line flags",
			err.Error(),
		))
	}

	selectorArgs := cmdFlags.Args()
	if len(selectorArgs) == 0 {
		selectorArgs = []string{"**"}
	}
	query.RawSelectors = append(query.RawSelectors, selectorArgs...)

	var selectorErr error
	query.Selectors, selectorErr = buildselector.ParseAll(selectorArgs)
	if selectorErr != nil {
		diags = diags.Append(selectorErr)
	}

	return query, func() {}, diags
}
