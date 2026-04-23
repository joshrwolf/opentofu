// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type Packages struct {
	Modules               []catalog.ModulePath
	Digest                digest.Digest
	Shape                 InstanceShape
	Refs                  []catalog.Addr
	ProviderFunctionCalls []ProviderFunctionCall
}

func (s *Solver) PackageInstances(ctx context.Context, module catalog.ModulePath) (Packages, tfdiags.Diagnostics) {
	if step, ok := module.LastStep(); ok && step.Key.Kind != catalog.KeyKindNone {
		return Packages{}, sourcelessError("PackageInstances requires a declaration module path without an instance key on the final step.")
	}
	result, err := s.packages.Get(ctx, module)
	return result, unwrapDiags(err)
}

func (s *Solver) computeDeclaredPackageInstances(ctx context.Context, module catalog.ModulePath) (Packages, error) {
	if module.Len() == 0 {
		return Packages{
			Modules: []catalog.ModulePath{catalog.RootModule()},
			Digest:  digest.FromStrings("build-package-instances-v1", "root"),
			Shape:   InstanceShapeSingle,
		}, nil
	}
	concreteParent, err := s.resolveModule(ctx, module.Parent())
	if err != nil {
		return Packages{}, err
	}
	if _, ok := s.catalog.Import(module.Declaration()); !ok {
		return Packages{}, fmt.Errorf("unknown module import %s", catalog.ModuleAddr(module))
	}
	if s.source == nil {
		return Packages{}, fmt.Errorf("build solver has no source evaluator for package instance facts")
	}

	step, _ := module.LastStep()
	evalModule := concreteParent.Child(step.Name, catalog.NoKey())

	// Pass 1: static evaluation.
	result, diags := s.source.EvalImportInstances(ctx, EvalImportInstancesRequest{
		Module:   evalModule,
		DeclAddr: catalog.ModuleAddr(evalModule),
			})
	if diags.HasErrors() {
		return Packages{}, wrapDiags(diags)
	}
	if !result.Deferred {
		return expandPackages(concreteParent, step.Name, result), nil
	}

	// Pass 2: with runtime resolver.
	if s.engine == nil {
		return Packages{}, fmt.Errorf("module %s instances depend on runtime values not available in inspection mode", catalog.ModuleAddr(module))
	}
	runtimeResolver := solverRuntimeResolver{solver: s}
	result, diags = s.source.EvalImportInstances(ctx, EvalImportInstancesRequest{
		Module:   evalModule,
		DeclAddr: catalog.ModuleAddr(evalModule),
		Runtime:  runtimeResolver,
			})
	if diags.HasErrors() {
		return Packages{}, wrapDiags(diags)
	}
	if result.Deferred {
		return Packages{}, fmt.Errorf("module %s instances could not be resolved: for_each depends on values not available at build time", catalog.ModuleAddr(module))
	}
	return expandPackages(concreteParent, step.Name, result), nil
}

func expandPackages(concreteParent catalog.ModulePath, name string, result InstanceResult) Packages {
	modules := make([]catalog.ModulePath, 0, len(result.Keys))
	for _, key := range result.Keys {
		modules = append(modules, concreteParent.Child(name, key))
	}
	slices.SortFunc(modules, func(a, b catalog.ModulePath) int {
		return cmp.Compare(a.Identity(), b.Identity())
	})
	return Packages{
		Modules:               modules,
		Digest:                result.Digest,
		Shape:                 result.Shape,
		Refs:                  result.Refs,
		ProviderFunctionCalls: result.ProviderFunctionCalls,
	}
}

func (s *Solver) resolveModule(ctx context.Context, module catalog.ModulePath) (catalog.ModulePath, error) {
	if module.Len() == 0 {
		return catalog.RootModule(), nil
	}
	step, _ := module.LastStep()
	if step.Key.Kind != catalog.KeyKindNone {
		if err := s.ensureModuleInstance(ctx, module); err != nil {
			return catalog.ModulePath{}, err
		}
		return module, nil
	}
	packages, err := s.packages.Get(ctx, module)
	if err != nil {
		if s.ancestorModuleEmpty(ctx, module) {
			return catalog.ModulePath{}, errEmptyAncestor
		}
		return catalog.ModulePath{}, err
	}
	if len(packages.Modules) == 0 {
		if s.ancestorModuleEmpty(ctx, module) {
			return catalog.ModulePath{}, errEmptyAncestor
		}
		return catalog.ModulePath{}, fmt.Errorf("module %s does not produce any instances in build mode", catalog.ModuleAddr(module))
	}
	if len(packages.Modules) > 1 {
		return catalog.ModulePath{}, fmt.Errorf("module %s expands to multiple instances; use a concrete module instance address", catalog.ModuleAddr(module))
	}
	return packages.Modules[0], nil
}

func (s *Solver) ensureModuleInstance(ctx context.Context, module catalog.ModulePath) error {
	if module.Len() == 0 {
		return nil
	}
	parent := module.Parent()
	step, _ := module.LastStep()
	packages, err := s.packages.Get(ctx, parent.Child(step.Name, catalog.NoKey()))
	if err != nil {
		return err
	}
	for _, candidate := range packages.Modules {
		if candidate.Identity() == module.Identity() {
			return nil
		}
	}
	return fmt.Errorf("unknown module instance %s", catalog.ModuleAddr(module))
}

func (s *Solver) concreteModulesForRef(ctx context.Context, module catalog.ModulePath) ([]catalog.ModulePath, error) {
	if module.Len() == 0 {
		return []catalog.ModulePath{catalog.RootModule()}, nil
	}

	step, _ := module.LastStep()
	if step.Key.Kind != catalog.KeyKindNone {
		if err := s.ensureModuleInstance(ctx, module); err != nil {
			return nil, err
		}
		return []catalog.ModulePath{module}, nil
	}

	packages, err := s.packages.Get(ctx, module)
	if err != nil {
		return nil, err
	}
	return packages.Modules, nil
}

func (s *Solver) ancestorModuleEmpty(ctx context.Context, module catalog.ModulePath) bool {
	partial := catalog.RootModule()
	for i := 0; i < module.Len(); i++ {
		step := module.Step(i)
		partial = partial.Child(step.Name, step.Key)
		if step.Key.Kind != catalog.KeyKindNone {
			continue
		}
		packages, err := s.packages.Get(ctx, partial)
		if err != nil {
			continue
		}
		if len(packages.Modules) == 0 {
			return true
		}
	}
	return false
}
