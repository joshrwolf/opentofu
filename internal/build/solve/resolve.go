// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"context"
	"errors"
	"fmt"

	"github.com/opentofu/opentofu/internal/build/catalog"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
)

func (s *Solver) computeTargetEval(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
	if !addr.Actionable() {
		return EvalResult{}, fmt.Errorf("address %s is not an actionable target", addr)
	}
	declAddr := addr
	declAddr.Module = declAddr.Module.Declaration()
	declAddr.Key = catalog.NoKey()
	if _, ok := s.catalog.Target(declAddr); !ok {
		return EvalResult{}, fmt.Errorf("unknown target %s", declAddr)
	}

	concreteModule, err := s.resolveModule(ctx, addr.Module)
	if errors.Is(err, ErrDeferred) {
		return EvalResult{Deferred: true}, nil
	}
	if errors.Is(err, errEmptyAncestor) {
		return EvalResult{}, nil
	}
	if err != nil {
		return EvalResult{}, err
	}

	concreteAddr := addr
	concreteAddr.Module = concreteModule

	if s.source == nil {
		return EvalResult{}, fmt.Errorf("build solver has no source evaluator for target facts")
	}
	result, diags := s.source.EvalTarget(ctx, EvalTargetRequest{
		Addr: concreteAddr,
	})
	if diags.HasErrors() {
		return EvalResult{}, wrapDiags(diags)
	}
	return result, nil
}

func (s *Solver) computeTargetConfig(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
	if !addr.Actionable() {
		return EvalResult{}, fmt.Errorf("address %s is not an actionable target", addr)
	}
	declAddr := addr
	declAddr.Module = declAddr.Module.Declaration()
	declAddr.Key = catalog.NoKey()
	target, ok := s.catalog.Target(declAddr)
	if !ok {
		return EvalResult{}, fmt.Errorf("unknown target %s", declAddr)
	}

	concreteModule, err := s.resolveModule(ctx, addr.Module)
	if errors.Is(err, ErrDeferred) {
		return EvalResult{Deferred: true}, nil
	}
	if errors.Is(err, errEmptyAncestor) {
		return EvalResult{}, nil
	}
	if err != nil {
		return EvalResult{}, err
	}

	concreteAddr := addr
	concreteAddr.Module = concreteModule

	if s.source == nil {
		return EvalResult{}, fmt.Errorf("build solver has no source evaluator for target facts")
	}
	if concreteAddr.Key.Kind == catalog.KeyKindNone {
		instances, err := s.instances.Get(ctx, concreteAddr)
		if err != nil {
			return EvalResult{}, err
		}
		if len(instances.Addrs) != 1 || instances.Addrs[0].Key.Kind != catalog.KeyKindNone {
			return EvalResult{
				Digest: instances.Digest,
				Known:  true,
			}, nil
		}
	}
	runtimeResolver := solverRuntimeResolver{solver: s}
	if s.providers == nil || (target != nil && target.Runner != nil && target.Runner.Kind == catalog.RunnerKindBuiltinRun) {
		result, diags := s.source.EvalTarget(ctx, EvalTargetRequest{Addr: concreteAddr, Runtime: runtimeResolver})
		if diags.HasErrors() {
			return EvalResult{}, wrapDiags(diags)
		}
		return result, nil
	}

	binding, bindingDiags := s.TargetBinding(ctx, concreteAddr.Module, target)
	if bindingDiags.HasErrors() {
		return EvalResult{}, wrapDiags(bindingDiags)
	}
	schema, schemaDiags := s.providers.TargetSchema(ctx, runnerKindForTarget(concreteAddr.Kind), binding.Provider, concreteAddr.Type)
	if schemaDiags.HasErrors() {
		return EvalResult{}, wrapDiags(schemaDiags)
	}
	if schema == nil {
		return EvalResult{}, wrapDiags(buildprovider.UnsupportedTargetDiagnostics(runnerKindForTarget(concreteAddr.Kind), binding.Provider, concreteAddr.Type, target.Source))
	}
	result, diags := s.source.EvalTarget(ctx, EvalTargetRequest{Addr: concreteAddr, Schema: schema, Runtime: runtimeResolver})
	if diags.HasErrors() {
		return EvalResult{}, wrapDiags(diags)
	}
	return result, nil
}

func (s *Solver) computeOutputValue(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
	result, err := s.outputEvals.Get(ctx, addr)
	if err != nil || result.Known || !result.Deferred {
		return result, err
	}
	return s.computeRuntimeOutputValue(ctx, addr)
}

func (s *Solver) computeOutputEval(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
	if addr.Kind != catalog.TargetKindOutput {
		return EvalResult{}, fmt.Errorf("address %s is not an output", addr)
	}
	declAddr := addr
	declAddr.Module = declAddr.Module.Declaration()
	if _, ok := s.catalog.Output(declAddr); !ok {
		return EvalResult{}, fmt.Errorf("unknown output %s", addr)
	}

	concreteModule, err := s.resolveModule(ctx, addr.Module)
	if errors.Is(err, ErrDeferred) {
		return EvalResult{Deferred: true}, nil
	}
	if errors.Is(err, errEmptyAncestor) {
		return EvalResult{Known: true}, nil
	}
	if err != nil {
		return EvalResult{}, err
	}

	concreteAddr := addr
	concreteAddr.Module = concreteModule

	if s.source == nil {
		return EvalResult{}, fmt.Errorf("build solver has no source evaluator for output facts")
	}
	result, diags := s.source.EvalOutput(ctx, EvalOutputRequest{
		Addr: concreteAddr,
	})
	if diags.HasErrors() {
		return EvalResult{}, wrapDiags(diags)
	}
	return result, nil
}

func (s *Solver) computeRuntimeOutputValue(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
	if addr.Kind != catalog.TargetKindOutput {
		return EvalResult{}, fmt.Errorf("address %s is not an output", addr)
	}
	declAddr := addr
	declAddr.Module = declAddr.Module.Declaration()
	if _, ok := s.catalog.Output(declAddr); !ok {
		return EvalResult{}, fmt.Errorf("unknown output %s", addr)
	}

	concreteModule, err := s.resolveModule(ctx, addr.Module)
	if errors.Is(err, ErrDeferred) {
		return EvalResult{Deferred: true}, nil
	}
	if err != nil {
		return EvalResult{}, err
	}

	concreteAddr := addr
	concreteAddr.Module = concreteModule

	if s.source == nil {
		return EvalResult{}, fmt.Errorf("build solver has no source evaluator for output facts")
	}
	runtimeResolver := solverRuntimeResolver{solver: s}
	result, diags := s.source.EvalOutput(ctx, EvalOutputRequest{
		Addr:    concreteAddr,
		Runtime: runtimeResolver,
	})
	if diags.HasErrors() {
		return EvalResult{}, wrapDiags(diags)
	}
	return result, nil
}
