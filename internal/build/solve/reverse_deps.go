// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"cmp"
	"context"
	"slices"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type ReverseDepsResult struct {
	Addrs    []catalog.Addr
	Complete bool
	Reason   tfdiags.Diagnostics
}

type reverseDepsWalkState struct {
	Complete bool
	Reason   tfdiags.Diagnostics
}

func (s *Solver) ReverseDeps(ctx context.Context, addr catalog.Addr) (ReverseDepsResult, tfdiags.Diagnostics) {

	switch addr.Kind {
	case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
		return s.computeTargetReverseDeps(ctx, addr)
	case catalog.TargetKindOutput, catalog.TargetKindModule:
		return s.computeReferenceReverseDeps(ctx, addr)
	default:
		return ReverseDepsResult{Complete: true}, nil
	}
}

func (s *Solver) computeTargetReverseDeps(ctx context.Context, addr catalog.Addr) (ReverseDepsResult, tfdiags.Diagnostics) {
	keys, diags := s.reverseDepTargetKeys(ctx, addr)
	if diags.HasErrors() {
		return ReverseDepsResult{}, diags
	}
	if len(keys) == 0 {
		return ReverseDepsResult{Complete: true}, nil
	}

	var ret []catalog.Addr
	seen := map[catalog.AddrKey]struct{}{}
	walkState, walkDiags := s.walkConcreteQueryAddrs(ctx, func(candidate catalog.Addr) bool {
		if candidate.Identity() == addr.Identity() {
			return true
		}

		if !candidate.Actionable() {
			return true
		}

		spec, specDiags := s.ActionSpec(ctx, candidate)
		diags = diags.Append(specDiags)
		if specDiags.HasErrors() {
			return true
		}
		if !actionKeysContainSpec(spec, keys) {
			return true
		}
		if _, ok := seen[candidate.Identity()]; ok {
			return true
		}
		seen[candidate.Identity()] = struct{}{}
		ret = append(ret, candidate)
		return true
	})
	diags = diags.Append(walkDiags)
	if diags.HasErrors() {
		return ReverseDepsResult{}, diags
	}

	slices.SortFunc(ret, func(a, b catalog.Addr) int {
		return cmp.Compare(a.String(), b.String())
	})
	return ReverseDepsResult{
		Addrs:    ret,
		Complete: walkState.Complete,
		Reason:   walkState.Reason,
	}, nil
}

func (s *Solver) reverseDepTargetKeys(ctx context.Context, addr catalog.Addr) (map[digest.Digest]struct{}, tfdiags.Diagnostics) {
	keys := map[digest.Digest]struct{}{}

	if addr.Key.Kind == catalog.KeyKindNone {
		actionSet, diags := s.ActionSpecs(ctx, addr)
		if diags.HasErrors() {
			return nil, diags
		}
		for _, spec := range actionSet.Specs {
			keys[spec.Key] = struct{}{}
		}
		return keys, nil
	}

	spec, diags := s.ActionSpec(ctx, addr)
	if diags.HasErrors() {
		return nil, diags
	}
	keys[spec.Key] = struct{}{}
	return keys, nil
}

func actionKeysContainSpec(spec engine.Spec, keys map[digest.Digest]struct{}) bool {
	if spec.Key == (digest.Digest{}) {
		return false
	}
	if _, ok := keys[spec.Key]; ok {
		return true
	}
	for _, dep := range spec.ExecDeps {
		if _, ok := keys[dep]; ok {
			return true
		}
	}
	return false
}

func (s *Solver) computeReferenceReverseDeps(ctx context.Context, addr catalog.Addr) (ReverseDepsResult, tfdiags.Diagnostics) {
	var ret []catalog.Addr
	seen := map[catalog.AddrKey]struct{}{}
	var diags tfdiags.Diagnostics

	walkState, walkDiags := s.walkConcreteQueryAddrs(ctx, func(candidate catalog.Addr) bool {
		if candidate.Identity() == addr.Identity() {
			return true
		}

		var refs []catalog.Addr
		switch candidate.Kind {
		case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
			result, resultDiags := s.TargetConfig(ctx, candidate)
			diags = diags.Append(resultDiags)
			if resultDiags.HasErrors() {
				return true
			}
			refs = result.Refs
		case catalog.TargetKindOutput:
			result, resultDiags := s.OutputEval(ctx, candidate)
			diags = diags.Append(resultDiags)
			if resultDiags.HasErrors() {
				return true
			}
			refs = result.Refs
		default:
			return true
		}

		if !refsContainSelected(refs, addr) {
			return true
		}
		if _, ok := seen[candidate.Identity()]; ok {
			return true
		}
		seen[candidate.Identity()] = struct{}{}
		ret = append(ret, candidate)
		return true
	})
	diags = diags.Append(walkDiags)
	if diags.HasErrors() {
		return ReverseDepsResult{}, diags
	}

	slices.SortFunc(ret, func(a, b catalog.Addr) int {
		return cmp.Compare(a.String(), b.String())
	})
	return ReverseDepsResult{
		Addrs:    ret,
		Complete: walkState.Complete,
		Reason:   walkState.Reason,
	}, nil
}

func refsContainSelected(refs []catalog.Addr, selected catalog.Addr) bool {
	for _, ref := range refs {
		if refMatchesSelected(ref, selected) {
			return true
		}
	}
	return false
}

func refMatchesSelected(ref, selected catalog.Addr) bool {
	if ref.Identity() == selected.Identity() {
		return true
	}

	switch selected.Kind {
	case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
		if ref.Kind != selected.Kind || ref.Type != selected.Type || ref.Name != selected.Name {
			return false
		}
		if ref.Module.Declaration().Identity() != selected.Module.Declaration().Identity() {
			return false
		}
		if selected.Key.Kind == catalog.KeyKindNone {
			return true
		}
		return ref.Key.Kind == catalog.KeyKindNone
	case catalog.TargetKindModule:
		if ref.Kind != catalog.TargetKindModule {
			return false
		}
		if ref.Module.Declaration().Identity() != selected.Module.Declaration().Identity() {
			return false
		}
		if step, ok := selected.Module.LastStep(); ok && step.Key.Kind != catalog.KeyKindNone {
			last, lastOK := ref.Module.LastStep()
			return !lastOK || last.Key.Kind == catalog.KeyKindNone
		}
		return true
	default:
		return false
	}
}

func (s *Solver) walkConcreteQueryAddrs(ctx context.Context, yield func(catalog.Addr) bool) (reverseDepsWalkState, tfdiags.Diagnostics) {
	if s.catalog == nil || s.catalog.Root == nil || yield == nil {
		return reverseDepsWalkState{Complete: true}, nil
	}

	state := reverseDepsWalkState{Complete: true}
	var diags tfdiags.Diagnostics
	var walk func(*catalog.Package, catalog.ModulePath) bool
	walk = func(pkg *catalog.Package, module catalog.ModulePath) bool {
		for _, target := range pkg.Targets {
			if !target.Addr.Actionable() {
				continue
			}

			declAddr := target.Addr
			declAddr.Module = module
			declAddr.Key = catalog.NoKey()

			instances, instanceDiags := s.TargetInstances(ctx, declAddr)
			if instanceDiags.HasErrors() {
				state.Complete = false
				continue
			}
			for _, instance := range instances.Addrs {
				if !yield(instance) {
					return false
				}
			}
		}

		for _, output := range pkg.Outputs {
			concrete := output.Addr
			concrete.Module = module
			if !yield(concrete) {
				return false
			}
		}

		for _, name := range pkg.ChildOrder {
			child := pkg.Children[name]
			childDeclModule := module.Child(name, catalog.NoKey())
			packages, packageDiags := s.PackageInstances(ctx, childDeclModule)
			if packageDiags.HasErrors() {
				state.Complete = false
				continue
			}
			for _, childModule := range packages.Modules {
				if !walk(child, childModule) {
					return false
				}
			}
		}

		return true
	}

	walk(s.catalog.Root, catalog.RootModule())
	return state, diags
}

