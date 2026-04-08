// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package local

import (
	"context"
	"log"

	"github.com/hashicorp/go-hclog"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/buildui"
	"github.com/opentofu/opentofu/internal/chofu"
	"github.com/opentofu/opentofu/internal/logging"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tofu"
)

func (b *Local) opBuild(
	stopCtx context.Context,
	cancelCtx context.Context,
	op *backend.Operation,
	runningOp *backend.RunningOperation,
) {
	log.Printf("[INFO] backend/local: starting Build operation")

	var diags tfdiags.Diagnostics
	ctx := context.WithoutCancel(stopCtx)

	if !op.HasConfig() {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"No configuration files",
			"Build requires configuration to be present.",
		))
		op.ReportResult(runningOp, diags)
		return
	}

	// Build uses a minimal load path — no state, no validation, no snapshot.
	// Target-aware: only parses module subtrees relevant to the targets.
	filter := chofu.NewTargetFilter(op.Targets)

	config, configDiags := op.ConfigLoader.LoadConfigWithWalker(
		ctx, op.ConfigDir, op.RootCall,
		filter.WrapWalker,
	)
	diags = diags.Append(configDiags)
	if configDiags.HasErrors() {
		op.ReportResult(runningOp, diags)
		return
	}

	// Parse variables.
	rawVariables := b.stubUnsetRequiredVariables(op.Variables, config.Module.Variables)
	variables, varDiags := backend.ParseVariableValues(rawVariables, config.Module.Variables)
	diags = diags.Append(varDiags)
	if varDiags.HasErrors() {
		op.ReportResult(runningOp, diags)
		return
	}

	plainVars := make(map[string]cty.Value, len(variables))
	for name, iv := range variables {
		if iv.Value != cty.NilVal {
			plainVars[name] = iv.Value
		}
	}

	buildOpts := &chofu.BuildOpts{
		Config:    config,
		Plugins:   b.ContextOpts.Plugins,
		Variables: plainVars,
		Targets:   op.Targets,
		Workspace: op.Workspace,
		Hooks:     &buildHookAdapter{hooks: op.Hooks},
		UI:        op.BuildUI,
	}

	// When using the build UI, suppress the global logger's stderr output
	// so raw hclog lines don't corrupt the TUI, and force provider log
	// level to INFO so providers emit logs for the sink to capture —
	// without requiring the user to set TF_LOG env vars.
	var restoreOutput func()
	var providerSink *buildui.ProviderLogSink
	if op.BuildUI != nil {
		logging.OverrideProviderLogLevel(hclog.Info)
		restoreOutput = logging.SuppressOutput()
		providerSink = buildui.NewProviderLogSink(op.BuildUI)
	}

	var result *chofu.BuildResult
	var buildDiags tfdiags.Diagnostics
	doneCh := make(chan struct{})
	panicHandler := logging.PanicHandlerWithTraceFn()
	go func() {
		defer panicHandler()
		defer close(doneCh)
		log.Printf("[INFO] backend/local: build calling chofu.Build")
		result, buildDiags = chofu.Build(ctx, buildOpts)
	}()

	// Wait for completion or cancellation. Context cancellation propagates
	// to chofu.Build directly — no tofu.Context needed.
	select {
	case <-stopCtx.Done():
		log.Printf("[TRACE] backend/local: build interrupted, waiting for shutdown")
		<-doneCh
	case <-doneCh:
	}

	if providerSink != nil {
		providerSink.Close()
	}
	if restoreOutput != nil {
		restoreOutput()
		logging.ClearProviderLogLevel()
	}

	diags = diags.Append(buildDiags)

	if result != nil {
		runningOp.State = result.State
	}

	if op.BuildUI != nil {
		// When using the build UI, diagnostics are rendered by the observer
		// via BuildCompleteEvent.Diagnostics. Set the result directly to
		// avoid ReportResult rendering legacy ╷│╵ box diagnostics.
		if diags.HasErrors() {
			runningOp.Result = backend.OperationFailure
		} else {
			runningOp.Result = backend.OperationSuccess
		}
	} else {
		op.ReportResult(runningOp, diags)
	}
}

// stubUnsetRequiredVariables is defined in backend_local.go.
// We reference it here to fill in required-but-unset variables with
// unknown values, matching the behavior of other non-interactive operations.
var _ = (*Local).stubUnsetRequiredVariables

// buildHookAdapter bridges tofu.Hook to chofu.BuildHooks, forwarding
// PreApply/PostApply calls so the standard UI output works.
type buildHookAdapter struct {
	hooks []tofu.Hook
}

func (a *buildHookAdapter) PreBuildResource(addr addrs.AbsResourceInstance, action string) {
	planAction := plans.Create
	if action == "read" {
		planAction = plans.Read
	}
	for _, h := range a.hooks {
		h.PreApply(addr, states.CurrentGen, planAction, cty.NullVal(cty.DynamicPseudoType), cty.DynamicVal) //nolint:errcheck
	}
}

func (a *buildHookAdapter) PostBuildResource(addr addrs.AbsResourceInstance, action string, err error) {
	for _, h := range a.hooks {
		h.PostApply(addr, states.CurrentGen, cty.NilVal, err) //nolint:errcheck
	}
}
