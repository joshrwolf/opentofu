// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"flag"
	"strings"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/backend"
	backendLocal "github.com/opentofu/opentofu/internal/backend/local"
	backendBuild "github.com/opentofu/opentofu/internal/backend/remote-state/build"
	"github.com/opentofu/opentofu/internal/command/arguments"
	"github.com/opentofu/opentofu/internal/command/flags"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// BuildCommand implements the "tofu build" command for single-pass
// build execution with content-addressed caching.
type BuildCommand struct {
	Meta
}

func (c *BuildCommand) Run(rawArgs []string) int {
	var diags tfdiags.Diagnostics
	ctx := c.CommandContext()

	common, rawArgs := arguments.ParseView(rawArgs)
	c.View.Configure(common)

	// Parse build-specific flags.
	var outputTargetRaw, skipRolesRaw, onlyRolesRaw string
	var parallelism int

	var targetsRaw flags.FlagStringSlice

	varFlags := flags.NewRawFlags("var")
	varFileFlags := flags.NewRawFlags("var-file")

	cmdFlags := flag.NewFlagSet("build", flag.ContinueOnError)
	cmdFlags.StringVar(&outputTargetRaw, "output", "", "")
	cmdFlags.StringVar(&outputTargetRaw, "o", "", "")
	cmdFlags.StringVar(&skipRolesRaw, "skip", "", "")
	cmdFlags.StringVar(&onlyRolesRaw, "only", "", "")
	cmdFlags.IntVar(&parallelism, "parallelism", 10, "")
	cmdFlags.Var(varFlags, "var", "")
	cmdFlags.Var(varFileFlags, "var-file", "")
	cmdFlags.Var((*flags.FlagStringSlice)(&targetsRaw), "target", "")

	if err := cmdFlags.Parse(rawArgs); err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	c.Meta.parallelism = parallelism
	c.Meta.variableArgs = varFlags.AllItems()

	view := views.NewApply(arguments.ViewOptions{ViewType: arguments.ViewHuman}, false, c.View)

	if skipRolesRaw != "" && onlyRolesRaw != "" {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Conflicting role filters",
			"The -skip and -only flags are mutually exclusive.",
		))
		view.Diagnostics(diags)
		return 1
	}

	enc, encDiags := c.Encryption(ctx)
	diags = diags.Append(encDiags)
	if encDiags.HasErrors() {
		view.Diagnostics(diags)
		return 1
	}

	be, beDiags := c.Backend(ctx, nil, enc.State())
	diags = diags.Append(beDiags)
	if diags.HasErrors() {
		view.Diagnostics(diags)
		return 1
	}

	// Verify the backend is suitable for build mode. The build command
	// requires the "build" backend for proper content-hash caching. Using
	// other backends (inmem, local, s3, etc.) would either discard state
	// between runs or be pathologically slow at scale.
	// Skip this check when running under test with testingOverrides.
	if c.testingOverrides == nil && !isBuildBackend(be) {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Unsupported backend for build mode",
			`The "tofu build" command requires the "build" backend. Configure it in your terraform block:

  terraform {
    backend "build" {}
  }

The build backend uses SQLite for fast, persistent state storage that scales to large DAGs.`,
		))
		view.Diagnostics(diags)
		return 1
	}

	opReq := c.Operation(ctx, be, view.Backend(), enc)
	opReq.Type = backend.OperationTypeBuild
	opReq.AutoApprove = true
	opReq.ConfigDir = "."
	opReq.Hooks = view.Hooks()
	opReq.View = view.Operation()

	loader, err := c.initConfigLoader()
	if err != nil {
		diags = diags.Append(err)
		view.Diagnostics(diags)
		return 1
	}
	opReq.ConfigLoader = loader

	if outputTargetRaw != "" {
		opReq.BuildOutputTargets = splitComma(outputTargetRaw)
	}
	if skipRolesRaw != "" {
		opReq.BuildSkipRoles = splitComma(skipRolesRaw)
	}
	if onlyRolesRaw != "" {
		opReq.BuildOnlyRoles = splitComma(onlyRolesRaw)
	}

	// Parse -target flags into addresses.
	for _, raw := range targetsRaw {
		target, targetDiags := addrs.ParseTargetStr(raw)
		diags = diags.Append(targetDiags)
		if !targetDiags.HasErrors() {
			opReq.Targets = append(opReq.Targets, target.Subject)
		}
	}

	view.Diagnostics(diags)
	if diags.HasErrors() {
		return 1
	}

	op, opDiags := c.RunOperation(ctx, be, opReq)
	view.Diagnostics(opDiags)
	if opDiags.HasErrors() || op.Result != backend.OperationSuccess {
		return 1
	}

	return 0
}

func splitComma(s string) []string {
	var parts []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// isBuildBackend checks whether the enhanced backend wraps a build backend.
// The local backend wraps non-enhanced backends (like "build") to provide
// the Enhanced interface, so we check the inner backend.
func isBuildBackend(be backend.Enhanced) bool {
	if local, ok := be.(*backendLocal.Local); ok {
		_, ok = local.Backend.(backendBuild.BuildBackend)
		return ok
	}
	return false
}

func (c *BuildCommand) Help() string {
	return strings.TrimSpace(`
Usage: tofu build [options]

  Executes a single-pass build with content-addressed caching. Unlike the
  traditional plan+apply workflow, build evaluates each resource's config,
  computes a content hash, and skips execution if the cached output in
  state is still valid.

Options:

  -target=ADDR      Restrict execution to specific resources or modules and
                    their dependencies (e.g., -target=module.nginx). Can be
                    specified multiple times.

  -o=NAME           Target a specific root output. Only resources needed to
                    produce this output will execute. Comma-separated.

  -skip=ROLE        Skip resources with the given role (e.g., -skip=test).
                    Comma-separated.

  -only=ROLE        Only execute resources with the given role (e.g., -only=build).
                    Comma-separated. Mutually exclusive with -skip.

  -var 'NAME=VALUE' Set a variable in the root module.

  -var-file=FILE    Set variables from a file.

  -parallelism=N    Limit concurrent operations (default: 10).
`)
}

func (c *BuildCommand) Synopsis() string {
	return "Execute a single-pass build with content-addressed caching"
}
