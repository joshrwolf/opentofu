// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"context"
	"fmt"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/engine"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func (s *Solver) expandExplicitDeps(ctx context.Context, deps []catalog.Addr) ([]catalog.Addr, error) {
	seen := make(map[catalog.AddrKey]struct{})
	visiting := make(map[catalog.AddrKey]struct{})
	ret := make([]catalog.Addr, 0, len(deps))

	for _, dep := range deps {
		if err := s.expandExplicitDep(ctx, dep, seen, visiting, &ret); err != nil {
			return nil, err
		}
	}
	return ret, nil
}

func (s *Solver) expandExplicitDep(ctx context.Context, dep catalog.Addr, seen, visiting map[catalog.AddrKey]struct{}, ret *[]catalog.Addr) error {
	if _, ok := visiting[dep.Identity()]; ok {
		return nil
	}
	visiting[dep.Identity()] = struct{}{}
	defer delete(visiting, dep.Identity())

	switch dep.Kind {
	case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
		lookupAddr := dep
		lookupAddr.Module = lookupAddr.Module.Declaration()
		lookupAddr.Key = catalog.NoKey()
		target, ok := s.catalog.Target(lookupAddr)
		if !ok {
			return fmt.Errorf("Unknown explicit dependency %s.", dep)
		}
		if !target.Addr.Actionable() {
			return nil
		}
		if _, ok := seen[dep.Identity()]; ok {
			return nil
		}
		seen[dep.Identity()] = struct{}{}
		*ret = append(*ret, dep)
		return nil
	case catalog.TargetKindModule:
		modules, err := s.concreteModulesForRef(ctx, dep.Module)
		if err != nil {
			return err
		}
		for _, module := range modules {
			declModule := module.Declaration()
			targets := s.catalog.ActionableTargetsInModule(declModule, true)
			for _, target := range targets {
				targetAddr := rewriteModulePrefix(target.Addr, declModule, module)
				if _, ok := seen[targetAddr.Identity()]; ok {
					continue
				}
				seen[targetAddr.Identity()] = struct{}{}
				*ret = append(*ret, targetAddr)
			}
		}
		return nil
	case catalog.TargetKindOutput:
		lookupAddr := dep
		lookupAddr.Module = lookupAddr.Module.Declaration()
		output, ok := s.catalog.Output(lookupAddr)
		if !ok {
			return fmt.Errorf("Unknown explicit output dependency %s.", dep)
		}
		for _, child := range rewriteExplicitDeps(output.ExplicitDeps, output.Addr.Module, dep.Module) {
			if err := s.expandExplicitDep(ctx, child, seen, visiting, ret); err != nil {
				return err
			}
		}
		return nil
	default:
		return nil
	}
}

func runnerKindForTarget(kind catalog.TargetKind) catalog.RunnerKind {
	if kind == catalog.TargetKindData {
		return catalog.RunnerKindProviderData
	}
	return catalog.RunnerKindProviderResource
}

func rewriteModulePrefix(addr catalog.Addr, declPrefix, concretePrefix catalog.ModulePath) catalog.Addr {
	if addr.Module.Identity() == declPrefix.Identity() {
		addr.Module = concretePrefix
		return addr
	}

	rewritten := concretePrefix
	for i := declPrefix.Len(); i < addr.Module.Len(); i++ {
		step := addr.Module.Step(i)
		rewritten = rewritten.Child(step.Name, step.Key)
	}
	addr.Module = rewritten
	return addr
}

func rewriteExplicitDeps(deps []catalog.Addr, declPrefix, concretePrefix catalog.ModulePath) []catalog.Addr {
	if len(deps) == 0 || declPrefix.Identity() == concretePrefix.Identity() {
		return deps
	}
	ret := make([]catalog.Addr, 0, len(deps))
	for _, dep := range deps {
		ret = append(ret, rewriteModulePrefix(dep, declPrefix, concretePrefix))
	}
	return ret
}

func engineSourceRef(src catalog.SourceRef) engine.SourceRef {
	return engine.SourceRef{
		Filename: src.Filename,
		Start:    engine.SourcePos{Line: src.Start.Line, Column: src.Start.Column, Byte: src.Start.Byte},
		End:      engine.SourcePos{Line: src.End.Line, Column: src.End.Column, Byte: src.End.Byte},
	}
}

func engineSourceRefFromDiags(rng tfdiags.SourceRange, fallback catalog.SourceRef) engine.SourceRef {
	if rng.Filename == "" {
		return engineSourceRef(fallback)
	}
	return engine.SourceRef{
		Filename: rng.Filename,
		Start:    engine.SourcePos{Line: rng.Start.Line, Column: rng.Start.Column, Byte: rng.Start.Byte},
		End:      engine.SourcePos{Line: rng.End.Line, Column: rng.End.Column, Byte: rng.End.Byte},
	}
}
