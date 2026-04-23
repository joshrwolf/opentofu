// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
)

type RunnerConfig struct {
	ProviderInvoker *buildprovider.Invoker
	ProviderSession *buildprovider.Session
}

type Runner struct {
	functions *buildprovider.Invoker
	targets   *buildprovider.Session

	mu       sync.Mutex
	warnings tfdiags.Diagnostics
}

func NewRunner(cfg RunnerConfig) *Runner {
	invoker := cfg.ProviderInvoker
	if invoker == nil {
		invoker = buildprovider.DefaultInvoker()
	}
	return &Runner{
		functions: invoker,
		targets:   cfg.ProviderSession,
	}
}

func DefaultRunner() *Runner {
	return NewRunner(RunnerConfig{})
}

func (r *Runner) Warnings() tfdiags.Diagnostics {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.warnings)
}

func (r *Runner) collectWarnings(diags tfdiags.Diagnostics) {
	if len(diags) == 0 {
		return
	}
	r.mu.Lock()
	for _, d := range diags {
		if d.Severity() == tfdiags.Warning {
			r.warnings = append(r.warnings, d)
		}
	}
	r.mu.Unlock()
}

func (r *Runner) Run(ctx context.Context, spec engine.Spec) (engine.RunResult, error) {
	ctx, span := tracing.Tracer().Start(ctx, "build.Runner.Run",
		tracing.SpanAttributes(
			attribute.String("build.action.name", spec.Name),
			attribute.String("build.action.runner", string(spec.Runner.Kind)),
		),
	)
	defer span.End()

	var result engine.RunResult
	var err error

	switch spec.Runner.Kind {
	case string(catalog.RunnerKindProviderFunction):
		result, err = r.runProviderFunction(ctx, spec)
	case string(catalog.RunnerKindProviderResource), string(catalog.RunnerKindProviderData):
		result, err = r.runProviderTarget(ctx, spec)
	case string(catalog.RunnerKindBuiltinRun):
		result, err = r.runBuiltinRun(ctx, spec)
	default:
		err = fmt.Errorf("runner kind %s is not implemented yet", spec.Runner.Kind)
	}

	tracing.SetSpanError(span, err)
	return result, err
}

func (r *Runner) runProviderFunction(ctx context.Context, spec engine.Spec) (engine.RunResult, error) {
	payload, ok := spec.Runner.Payload.(buildprovider.FunctionPayload)
	if !ok {
		return engine.RunResult{}, fmt.Errorf("provider-function action %s does not have a valid provider-function payload", spec.Name)
	}

	result, diags := r.functions.InvokeFunction(ctx, payload.Request)
	r.collectWarnings(diags)
	if diags.HasErrors() {
		return engine.RunResult{}, &runnerDiagsError{diags}
	}

	return engine.RunResult{
		OutputKey: result.OutputKey,
		Payload:   result.Payload,
	}, nil
}

func (r *Runner) runProviderTarget(ctx context.Context, spec engine.Spec) (engine.RunResult, error) {
	payload, ok := spec.Runner.Payload.(buildprovider.TargetPayload)
	if !ok {
		return engine.RunResult{}, fmt.Errorf("provider-target action %s does not have a valid provider-target payload", spec.Name)
	}
	if r.targets == nil {
		return engine.RunResult{}, &runnerDiagsError{buildprovider.UnsupportedTargetDiagnostics(catalog.RunnerKind(spec.Runner.Kind), payload.Binding.Provider, payload.TypeName, catalogSourceRef(spec.Source))}
	}

	result, diags := r.targets.InvokeTarget(ctx, payload.Binding, payload.Request)
	r.collectWarnings(diags)
	if diags.HasErrors() {
		return engine.RunResult{}, &runnerDiagsError{diags}
	}
	if result.Payload == nil {
		return engine.RunResult{}, fmt.Errorf("provider-target action %s completed without a result payload", spec.Name)
	}
	return engine.RunResult{
		OutputKey: result.OutputKey,
		Payload:   result.Payload,
	}, nil
}

func (r *Runner) runBuiltinRun(ctx context.Context, spec engine.Spec) (engine.RunResult, error) {
	payload, ok := spec.Runner.Payload.(buildrun.Payload)
	if !ok {
		return engine.RunResult{}, fmt.Errorf("built-in run action %s does not have a valid run payload", spec.Name)
	}

	req, diags := buildrun.DecodeRequest(payload.Request, runnerSourceRange(spec.Source))
	if diags.HasErrors() {
		return engine.RunResult{}, &runnerDiagsError{diags}
	}

	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Cwd
	cmd.Env = req.Environment()
	if len(req.Stdin) != 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			return engine.RunResult{}, fmt.Errorf("failed to execute %s: %w", req.Argv[0], err)
		}
	}

	if !slices.Contains(req.AllowedExitCodes, exitCode) {
		detail := fmt.Sprintf("process %q exited with code %d", req.Argv[0], exitCode)
		stderrText := strings.TrimSpace(stderr.String())
		if stderrText != "" {
			detail += ". stderr: " + stderrText
		}
		return engine.RunResult{}, fmt.Errorf("build run execution error: %s", detail)
	}

	result := buildrun.Result{
		ExitCode: exitCode,
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
	}
	switch req.Decode {
	case buildrun.DecodeModeText:
		result.Decoded = &buildrun.DecodedValue{
			Mode: buildrun.DecodeModeText,
			Text: stdout.String(),
		}
	case buildrun.DecodeModeJSON:
		result.Decoded = &buildrun.DecodedValue{
			Mode: buildrun.DecodeModeJSON,
			JSON: stdout.Bytes(),
		}
	}

	resultPayload, encodeErr := buildrun.EncodeResult(payload.Request, result)
	if encodeErr != nil {
		return engine.RunResult{}, fmt.Errorf("failed to encode build run result for %s: %w", spec.Name, encodeErr)
	}

	return engine.RunResult{
		OutputKey: digest.FromBytes(resultPayload),
		Payload:   resultPayload,
	}, nil
}

type runnerDiagsError struct {
	diags tfdiags.Diagnostics
}

func (e *runnerDiagsError) Error() string {
	if err := e.diags.Err(); err != nil {
		return err.Error()
	}
	return "build runner diagnostics"
}

func unwrapRunnerDiags(err error) tfdiags.Diagnostics {
	if err == nil {
		return nil
	}
	if de, ok := errors.AsType[*runnerDiagsError](err); ok {
		return de.diags
	}
	return tfdiags.Diagnostics{}.Append(err)
}

func runnerSourceRange(src engine.SourceRef) tfdiags.SourceRange {
	return tfdiags.SourceRange{
		Filename: src.Filename,
		Start: tfdiags.SourcePos{
			Line:   src.Start.Line,
			Column: src.Start.Column,
			Byte:   src.Start.Byte,
		},
		End: tfdiags.SourcePos{
			Line:   src.End.Line,
			Column: src.End.Column,
			Byte:   src.End.Byte,
		},
	}
}

func catalogSourceRef(src engine.SourceRef) catalog.SourceRef {
	return catalog.SourceRef{
		Filename: src.Filename,
		Start:    catalog.SourcePos{Line: src.Start.Line, Column: src.Start.Column, Byte: src.Start.Byte},
		End:      catalog.SourcePos{Line: src.End.Line, Column: src.End.Column, Byte: src.End.Byte},
	}
}
