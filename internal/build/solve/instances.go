// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"context"
	"errors"
	"fmt"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type Instances struct {
	Addrs                 []catalog.Addr
	Digest                digest.Digest
	Shape                 InstanceShape
	Refs                  []catalog.Addr
	ProviderFunctionCalls []ProviderFunctionCall
}

func (s *Solver) TargetInstances(ctx context.Context, addr catalog.Addr) (Instances, tfdiags.Diagnostics) {
	if addr.Key.Kind != catalog.KeyKindNone {
		return Instances{}, sourcelessError("TargetInstances requires a declaration address without an instance key.")
	}
	result, err := s.instances.Get(ctx, addr)
	return result, unwrapDiags(err)
}

func (s *Solver) computeDeclaredTargetInstances(ctx context.Context, declAddr catalog.Addr) (Instances, error) {
	if !declAddr.Actionable() {
		return Instances{}, fmt.Errorf("address %s is not an actionable target", declAddr)
	}

	concreteModule, err := s.resolveModule(ctx, declAddr.Module)
	if errors.Is(err, ErrDeferred) {
		return Instances{}, err
	}
	if errors.Is(err, errEmptyAncestor) {
		return Instances{}, nil
	}
	if err != nil {
		return Instances{}, err
	}

	concreteDeclAddr := declAddr
	concreteDeclAddr.Module = concreteModule

	lookupAddr := concreteDeclAddr
	lookupAddr.Module = lookupAddr.Module.Declaration()
	if _, ok := s.catalog.Target(lookupAddr); !ok {
		return Instances{}, fmt.Errorf("unknown target %s", lookupAddr)
	}
	if s.source == nil {
		return Instances{}, fmt.Errorf("build solver has no source evaluator for target instance facts")
	}

	// Pass 1: static evaluation (no runtime resolver).
	result, diags := s.source.EvalTargetInstances(ctx, EvalTargetInstancesRequest{
		DeclAddr: concreteDeclAddr,
			})
	if diags.HasErrors() {
		return Instances{}, wrapDiags(diags)
	}
	if !result.Deferred {
		return expandInstances(concreteDeclAddr, result), nil
	}

	// Pass 2: re-evaluate with runtime resolver — goroutine may suspend
	// while RuntimeResolver blocks on upstream engine results.
	if s.engine == nil {
		return Instances{}, fmt.Errorf("target %s instances depend on runtime values not available in inspection mode", concreteDeclAddr)
	}
	runtimeResolver := solverRuntimeResolver{solver: s}
	result, diags = s.source.EvalTargetInstances(ctx, EvalTargetInstancesRequest{
		DeclAddr: concreteDeclAddr,
		Runtime:  runtimeResolver,
			})
	if diags.HasErrors() {
		return Instances{}, wrapDiags(diags)
	}
	if result.Deferred {
		return Instances{}, fmt.Errorf("target %s instances could not be resolved: for_each depends on values not available at build time", concreteDeclAddr)
	}
	return expandInstances(concreteDeclAddr, result), nil
}

func expandInstances(declAddr catalog.Addr, result InstanceResult) Instances {
	addrs := make([]catalog.Addr, 0, len(result.Keys))
	for _, key := range result.Keys {
		addr := declAddr
		addr.Key = key
		addrs = append(addrs, addr)
	}
	return Instances{
		Addrs:                 addrs,
		Digest:                result.Digest,
		Shape:                 result.Shape,
		Refs:                  result.Refs,
		ProviderFunctionCalls: result.ProviderFunctionCalls,
	}
}
