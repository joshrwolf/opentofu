// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package arguments

import (
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/command/flags"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// BuildUIMode selects which build output renderer to use.
type BuildUIMode string

const (
	BuildUIDefault BuildUIMode = ""    // legacy terraform-style output
	BuildUITUI     BuildUIMode = "tui" // interactive bubbletea TUI
	BuildUIText    BuildUIMode = "text" // cargo-style line output
	BuildUIJSON    BuildUIMode = "json" // structured JSONL
)

// Build represents the command-line arguments for the build command.
type Build struct {
	Vars        *Vars
	ViewOptions ViewOptions

	Parallelism int
	Targets     []addrs.Targetable
	UIMode      BuildUIMode

	targetsRaw      []string
	targetsFilesRaw []string
	uiRaw           string
}

// ParseBuild processes CLI arguments, returning a Build value and errors.
func ParseBuild(args []string) (*Build, func(), tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	build := &Build{
		Vars: &Vars{},
	}

	cmdFlags := defaultFlagSet("build")
	cmdFlags.IntVar(&build.Parallelism, "parallelism", DefaultParallelism, "parallelism")
	cmdFlags.StringVar(&build.uiRaw, "ui", "", "ui")
	cmdFlags.Var((*flags.FlagStringSlice)(&build.targetsRaw), "target", "target")
	cmdFlags.Var((*flags.FlagStringSlice)(&build.targetsFilesRaw), "target-file", "target-file")

	// Wire up -var and -var-file.
	varsFlags := flags.NewRawFlags("-var")
	varFilesFlags := varsFlags.Alias("-var-file")
	build.Vars.vars = &varsFlags
	build.Vars.varFiles = &varFilesFlags
	cmdFlags.Var(build.Vars.vars, "var", "var")
	cmdFlags.Var(build.Vars.varFiles, "var-file", "var-file")

	build.ViewOptions.AddFlags(cmdFlags, true)

	if err := cmdFlags.Parse(args); err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to parse command-line flags",
			err.Error(),
		))
	}

	if len(cmdFlags.Args()) > 0 {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Too many command line arguments",
			"The build command expects no positional arguments.",
		))
	}

	// Parse targets.
	targets, _, parseDiags := parseRawTargetsAndExcludes(build.targetsRaw, nil, build.targetsFilesRaw, nil)
	diags = diags.Append(parseDiags)
	build.Targets = targets

	// Parse --ui mode.
	switch BuildUIMode(build.uiRaw) {
	case BuildUIDefault, BuildUITUI, BuildUIText, BuildUIJSON:
		build.UIMode = BuildUIMode(build.uiRaw)
	default:
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid --ui value",
			"Supported values are: tui, text, json",
		))
	}

	closer, moreDiags := build.ViewOptions.Parse()
	diags = diags.Append(moreDiags)

	return build, closer, diags
}
