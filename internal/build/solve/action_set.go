// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"bytes"
	"context"
	"fmt"
	"slices"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/engine"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type ActionSet struct {
	Specs []engine.Spec
}

func (s *Solver) ActionSpecs(ctx context.Context, addr catalog.Addr) (ActionSet, tfdiags.Diagnostics) {
	if !addr.Actionable() {
		return ActionSet{}, sourcelessError("Target " + addr.String() + " does not lower to actions in build mode.")
	}
	if addr.Key.Kind != catalog.KeyKindNone {
		return ActionSet{}, sourcelessError("ActionSpecs requires a declaration address without an instance key.")
	}
	result, err := s.actionSets.Get(ctx, addr)
	return result, unwrapDiags(err)
}

func (s *Solver) computeDeclaredActionSpecs(ctx context.Context, declAddr catalog.Addr) (ActionSet, error) {
	instances, err := s.instances.Get(ctx, declAddr)
	if err != nil {
		return ActionSet{}, err
	}

	ret := ActionSet{
		Specs: make([]engine.Spec, 0, len(instances.Addrs)),
	}
	for _, addr := range instances.Addrs {
		spec, err := s.actions.Get(ctx, addr)
		if err != nil {
			return ActionSet{}, err
		}
		if regErr := s.registerSpec(ctx, spec); regErr != nil {
			return ActionSet{}, regErr
		}
		ret.Specs = append(ret.Specs, spec)
	}

	slices.SortFunc(ret.Specs, func(a, b engine.Spec) int {
		return bytes.Compare(a.Key[:], b.Key[:])
	})
	return ret, nil
}

func (s *Solver) actionSetForTargetRef(ctx context.Context, addr catalog.Addr) (ActionSet, error) {
	if !addr.Actionable() {
		return ActionSet{}, fmt.Errorf("target reference %s does not lower to build actions", addr)
	}
	if addr.Key.Kind == catalog.KeyKindNone {
		result, err := s.actionSets.Get(ctx, addr)
		return result, err
	}

	spec, err := s.actions.Get(ctx, addr)
	if err != nil {
		return ActionSet{}, err
	}
	if regErr := s.registerSpec(ctx, spec); regErr != nil {
		return ActionSet{}, regErr
	}
	return ActionSet{Specs: []engine.Spec{spec}}, nil
}
