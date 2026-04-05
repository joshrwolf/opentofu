// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package local

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/logging"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/states/statefile"
	"github.com/opentofu/opentofu/internal/states/statemgr"
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

	// Set up periodic state persistence.
	stateHook := new(StateHook)
	op.Hooks = append(op.Hooks, stateHook)

	lr, _, opState, contextDiags := b.localRun(ctx, op)
	diags = diags.Append(contextDiags)
	if contextDiags.HasErrors() {
		op.ReportResult(runningOp, diags)
		return
	}
	defer func() {
		diags := op.StateLocker.Unlock()
		if diags.HasErrors() {
			op.View.Diagnostics(diags)
			runningOp.Result = backend.OperationFailure
		}
	}()

	runningOp.State = lr.InputState

	schemas, moreDiags := lr.Core.Schemas(ctx, lr.Config, lr.InputState)
	diags = diags.Append(moreDiags)
	if moreDiags.HasErrors() {
		op.ReportResult(runningOp, diags)
		return
	}

	stateHook.Schemas = schemas
	persistInterval := getEnvAsInt(persistIntervalEnvironmentVariableName, defaultPersistInterval)
	if persistInterval < defaultPersistInterval {
		panic(fmt.Sprintf("Can't use value lower than %d for env variable %s, got %d",
			defaultPersistInterval, persistIntervalEnvironmentVariableName, persistInterval))
	}
	stateHook.PersistInterval = time.Duration(persistInterval) * time.Second
	stateHook.StateMgr = opState

	// Convert string role names from the CLI to typed ResourceRole values.
	var skipRoles []providers.ResourceRole
	for _, r := range op.BuildSkipRoles {
		skipRoles = append(skipRoles, providers.ResourceRole(r))
	}
	var onlyRoles []providers.ResourceRole
	for _, r := range op.BuildOnlyRoles {
		onlyRoles = append(onlyRoles, providers.ResourceRole(r))
	}

	buildOpts := &tofu.BuildOpts{
		SetVariables:  lr.PlanOpts.SetVariables,
		Targets:       op.Targets,
		Excludes:      op.Excludes,
		SkipRoles:     skipRoles,
		OnlyRoles:     onlyRoles,
		OutputTargets: op.BuildOutputTargets,
	}

	// Execute the build in a goroutine so it can be interrupted.
	var buildState *states.State
	var buildDiags tfdiags.Diagnostics
	doneCh := make(chan struct{})
	panicHandler := logging.PanicHandlerWithTraceFn()
	go func() {
		defer panicHandler()
		defer close(doneCh)
		log.Printf("[INFO] backend/local: build calling Build")
		buildState, buildDiags = lr.Core.Build(ctx, lr.Config, lr.InputState, buildOpts)
	}()

	if b.opWait(doneCh, stopCtx, cancelCtx, lr.Core, opState, op.View) {
		return
	}
	diags = diags.Append(buildDiags)

	if diags.HasErrors() && buildState == nil {
		log.Printf("[ERROR] backend/local: build returned nil state")
		op.ReportResult(runningOp, diags)
		return
	}

	runningOp.State = buildState
	err := statemgr.WriteAndPersist(context.TODO(), opState, buildState, schemas)
	if err != nil {
		stateFile := statemgr.Export(opState)
		if stateFile == nil {
			stateFile = &statefile.File{}
		}
		stateFile.State = buildState
		diags = diags.Append(b.backupStateForError(stateFile, err, op.View))
		op.ReportResult(runningOp, diags)
		return
	}

	if buildDiags.HasErrors() {
		op.ReportResult(runningOp, diags)
		return
	}

	op.View.Diagnostics(diags)
}
