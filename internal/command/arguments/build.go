// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package arguments

import (
	buildselector "github.com/opentofu/opentofu/internal/build/selector"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type Build struct {
	Parallelism int
	UseCache    bool
	CacheClear  bool
	Selectors   buildselector.Set
	ViewOptions ViewOptions
	Vars        *Vars
}

func ParseBuild(args []string) (*Build, func(), tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	build := &Build{
		Parallelism: DefaultParallelism,
		UseCache:    true,
		Vars:        &Vars{},
	}

	cmdFlags := extendedFlagSet("build", nil, nil, build.Vars)
	cmdFlags.IntVar(&build.Parallelism, "parallelism", DefaultParallelism, "parallelism")
	cmdFlags.BoolVar(&build.CacheClear, "cache-clear", false, "cache-clear")
	noCache := cmdFlags.Bool("no-cache", false, "no-cache")

	build.ViewOptions.AddFlags(cmdFlags, true)

	if err := cmdFlags.Parse(args); err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to parse command-line flags",
			err.Error(),
		))
	}

	build.UseCache = !*noCache
	selectorArgs := cmdFlags.Args()
	if len(selectorArgs) == 0 {
		selectorArgs = []string{"**"}
	}

	var selectorErr error
	build.Selectors, selectorErr = buildselector.ParseAll(selectorArgs)
	if selectorErr != nil {
		diags = diags.Append(selectorErr)
	}

	closer, moreDiags := build.ViewOptions.Parse()
	diags = diags.Append(moreDiags)

	return build, closer, diags
}
