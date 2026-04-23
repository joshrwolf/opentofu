// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"context"
	"fmt"
	"sync"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/dice"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type Solver struct {
	session   *dice.Session
	catalog   *catalog.Catalog
	source    Evaluator
	providers *buildprovider.Session
	engine    *engine.Engine

	packages          *dice.Computation[catalog.ModulePathKey, catalog.ModulePath, Packages]
	actions           *dice.Computation[catalog.AddrKey, catalog.Addr, engine.Spec]
	actionSets        *dice.Computation[catalog.AddrKey, catalog.Addr, ActionSet]
	bindings          *dice.Computation[catalog.ProviderKey, bindingArg, buildprovider.Binding]
	instances         *dice.Computation[catalog.AddrKey, catalog.Addr, Instances]
	targetEvals       *dice.Computation[catalog.AddrKey, catalog.Addr, EvalResult]
	targets           *dice.Computation[catalog.AddrKey, catalog.Addr, EvalResult]
	outputEvals       *dice.Computation[catalog.AddrKey, catalog.Addr, EvalResult]
	outputs           *dice.Computation[catalog.AddrKey, catalog.Addr, EvalResult]
	targetValues      *dice.Computation[catalog.AddrKey, catalog.Addr, EvalResult]
	targetCollections *dice.Computation[catalog.AddrKey, catalog.Addr, EvalResult]
	moduleCollections *dice.Computation[catalog.ModulePathKey, catalog.ModulePath, EvalResult]

	specMu    sync.Mutex
	specIndex map[digest.Digest]engine.Spec
}

type bindingArg struct {
	Module catalog.ModulePath
	Local  addrs.LocalProviderConfig
}

type Config struct {
	Providers *buildprovider.Session
	Engine    *engine.Engine
}

func New(session *dice.Session, cat *catalog.Catalog, eval Evaluator, cfg Config) *Solver {
	s := &Solver{
		session:   session,
		catalog:   cat,
		source:    eval,
		providers: cfg.Providers,
		engine:    cfg.Engine,
		specIndex: map[digest.Digest]engine.Spec{},
	}
	s.initComputations(session)
	return s
}

func (s *Solver) initComputations(session *dice.Session) {
	s.packages = dice.Register(session, "build.packages.",
		s.computeDeclaredPackageInstances, catalog.ModulePath.Identity, catalog.ModulePath.String)
	s.actions = dice.Register(session, "build.action.",
		s.computeActionSpec, catalog.Addr.Identity, catalog.Addr.String)
	s.actionSets = dice.Register(session, "build.actions.",
		s.computeDeclaredActionSpecs, catalog.Addr.Identity, catalog.Addr.String)
	s.bindings = dice.Register(session, "build.binding.",
		func(ctx context.Context, arg bindingArg) (buildprovider.Binding, error) {
			return s.computeBinding(ctx, arg.Module, arg.Local)
		},
		func(a bindingArg) catalog.ProviderKey {
			return catalog.ProviderKey{
				Module:    a.Module.Identity(),
				LocalName: a.Local.LocalName,
				Alias:     a.Local.Alias,
			}
		},
		func(a bindingArg) string {
			return a.Module.String() + "." + a.Local.LocalName + "." + a.Local.Alias
		})
	s.instances = dice.Register(session, "build.instances.",
		s.computeDeclaredTargetInstances, catalog.Addr.Identity, catalog.Addr.String)
	s.targetEvals = dice.Register(session, "build.target_eval.",
		s.computeTargetEval, catalog.Addr.Identity, catalog.Addr.String)
	s.targets = dice.Register(session, "build.target.",
		s.computeTargetConfig, catalog.Addr.Identity, catalog.Addr.String)
	s.outputEvals = dice.Register(session, "build.outputeval.",
		s.computeOutputEval, catalog.Addr.Identity, catalog.Addr.String)
	s.outputs = dice.Register(session, "build.output.",
		s.computeOutputValue, catalog.Addr.Identity, catalog.Addr.String)
	s.targetValues = dice.Register(session, "build.targetvalue.",
		s.computeTargetValue, catalog.Addr.Identity, catalog.Addr.String)
	s.targetCollections = dice.Register(session, "build.targetcollection.",
		s.computeTargetValueCollection, catalog.Addr.Identity, catalog.Addr.String)
	s.moduleCollections = dice.Register(session, "build.modulecollection.",
		s.computeModuleValueCollection, catalog.ModulePath.Identity, catalog.ModulePath.String)
}

func (s *Solver) Stats() []dice.Stats {
	return []dice.Stats{
		s.packages.Stats(),
		s.actions.Stats(),
		s.actionSets.Stats(),
		s.bindings.Stats(),
		s.instances.Stats(),
		s.targetEvals.Stats(),
		s.targets.Stats(),
		s.outputEvals.Stats(),
		s.outputs.Stats(),
		s.targetValues.Stats(),
		s.targetCollections.Stats(),
		s.moduleCollections.Stats(),
	}
}

func (s *Solver) registerSpec(ctx context.Context, spec engine.Spec) error {
	if spec.Key == (digest.Digest{}) {
		return fmt.Errorf("cannot register spec with zero key")
	}
	s.specMu.Lock()
	if _, ok := s.specIndex[spec.Key]; ok {
		s.specMu.Unlock()
		return nil
	}
	s.specIndex[spec.Key] = spec
	s.specMu.Unlock()
	if s.engine != nil {
		if _, err := s.engine.Submit(ctx, spec); err != nil {
			return err
		}
	}
	return nil
}

func (s *Solver) ActionSpec(ctx context.Context, addr catalog.Addr) (engine.Spec, tfdiags.Diagnostics) {
	spec, err := s.actions.Get(ctx, addr)
	if err == nil {
		if regErr := s.registerSpec(ctx, spec); regErr != nil {
			return spec, unwrapDiags(regErr)
		}
	}
	return spec, unwrapDiags(err)
}

func (s *Solver) Binding(ctx context.Context, module catalog.ModulePath, local addrs.LocalProviderConfig) (buildprovider.Binding, tfdiags.Diagnostics) {
	binding, err := s.bindings.Get(ctx, bindingArg{Module: module, Local: local})
	return binding, unwrapDiags(err)
}

func (s *Solver) OutputValue(ctx context.Context, addr catalog.Addr) (EvalResult, tfdiags.Diagnostics) {
	result, err := s.outputs.Get(ctx, addr)
	return result, unwrapDiags(err)
}

func (s *Solver) TargetConfig(ctx context.Context, addr catalog.Addr) (EvalResult, tfdiags.Diagnostics) {
	result, err := s.targets.Get(ctx, addr)
	return result, unwrapDiags(err)
}

func (s *Solver) OutputEval(ctx context.Context, addr catalog.Addr) (EvalResult, tfdiags.Diagnostics) {
	result, err := s.outputEvals.Get(ctx, addr)
	return result, unwrapDiags(err)
}

func (s *Solver) TargetValue(ctx context.Context, addr catalog.Addr) (EvalResult, tfdiags.Diagnostics) {
	result, err := s.targetValues.Get(ctx, addr)
	return result, unwrapDiags(err)
}

func (s *Solver) TargetValueCollection(ctx context.Context, addr catalog.Addr) (EvalResult, tfdiags.Diagnostics) {
	if addr.Key.Kind != catalog.KeyKindNone {
		return EvalResult{}, sourcelessError("TargetValueCollection requires a declaration address without an instance key.")
	}
	result, err := s.targetCollections.Get(ctx, addr)
	return result, unwrapDiags(err)
}

func (s *Solver) ModuleValueCollection(ctx context.Context, module catalog.ModulePath) (EvalResult, tfdiags.Diagnostics) {
	if step, ok := module.LastStep(); ok && step.Key.Kind != catalog.KeyKindNone {
		return EvalResult{}, sourcelessError("ModuleValueCollection requires a declaration module path without an instance key on the final step.")
	}
	result, err := s.moduleCollections.Get(ctx, module)
	return result, unwrapDiags(err)
}

func sourcelessError(detail string) tfdiags.Diagnostics {
	return tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
		tfdiags.Error,
		"Build solve error",
		detail,
	))
}
