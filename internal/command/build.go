// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/buildui"
	"github.com/opentofu/opentofu/internal/chofu"
	"github.com/opentofu/opentofu/internal/command/arguments"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// BuildCommand implements the "tofu build" command.
type BuildCommand struct {
	Meta
}

func (c *BuildCommand) Run(rawArgs []string) int {
	var diags tfdiags.Diagnostics
	ctx := c.CommandContext()

	common, rawArgs := arguments.ParseView(rawArgs)
	c.View.Configure(common)

	args, closer, argDiags := arguments.ParseBuild(rawArgs)
	defer closer()
	diags = diags.Append(argDiags)

	view := views.NewApply(args.ViewOptions, false, c.View)

	if diags.HasErrors() {
		view.Diagnostics(diags)
		return 1
	}

	// Create build UI observer when --ui is set. Default (empty) uses
	// the legacy terraform-style output via Apply view hooks.
	var ui chofu.BuildUI
	var tuiObs *buildui.TUIObserver
	switch args.UIMode {
	case arguments.BuildUITUI:
		tuiObs = buildui.NewTUIObserver(ctx)
		ui = tuiObs
	case arguments.BuildUIText:
		ui = buildui.NewTextObserver(os.Stderr, isatty.IsTerminal(os.Stderr.Fd()))
	case arguments.BuildUIJSON:
		ui = buildui.NewJSONObserver(os.Stdout)
	}

	c.Meta.parallelism = args.Parallelism
	c.Meta.variableArgs = args.Vars.All()

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

	opReq := c.Operation(ctx, be, view.Backend(), enc)
	opReq.Type = backend.OperationTypeBuild
	opReq.AutoApprove = true
	opReq.ConfigDir = "."
	opReq.View = view.Operation()
	opReq.Targets = args.Targets

	// When using the new UI, skip legacy hooks — they'd produce
	// duplicate "Creating..." output alongside the new renderer.
	if ui == nil {
		opReq.Hooks = view.Hooks()
	}
	opReq.BuildUI = ui

	loader, err := c.initConfigLoader()
	if err != nil {
		diags = diags.Append(fmt.Errorf("failed to initialize config loader: %w", err))
		view.Diagnostics(diags)
		return 1
	}
	opReq.ConfigLoader = loader

	view.Diagnostics(diags)
	diags = nil

	op, diags := c.RunOperation(ctx, be, opReq)

	// Wait for TUI to finish rendering before printing diagnostics.
	if tuiObs != nil {
		tuiObs.Wait()
	}

	// When using the new UI, diagnostics are rendered by the observer
	// via BuildCompleteEvent.Diagnostics — classified, deduplicated,
	// and cascade-suppressed. Skip the legacy ╷│╵ box rendering.
	if ui == nil {
		view.Diagnostics(diags)
	}
	if diags.HasErrors() {
		return 1
	}

	if op.Result != backend.OperationSuccess {
		return op.Result.ExitStatus()
	}

	return 0
}

func (c *BuildCommand) Help() string {
	return strings.TrimSpace(`
Usage: tofu build [options]

  Executes a single-pass build with content-addressed caching.

Options:

  -var 'NAME=VALUE'   Set a variable in the root module.
  -var-file=FILE      Set variables from a file.
  -parallelism=N      Limit concurrent operations (default: 10).
  -target=ADDRESS     Limit build to specific resources.
  -target-file=FILE   Limit build to resources listed in a file.
  -ui=MODE            Output mode: tui (interactive), text (CI-friendly),
                      json (structured JSONL). Default: legacy terraform output.
`)
}

func (c *BuildCommand) Synopsis() string {
	return "Execute a single-pass build with content-addressed caching"
}
