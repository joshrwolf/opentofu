// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
)

func (s *Solver) computeActionSpec(ctx context.Context, addr catalog.Addr) (engine.Spec, error) {
	if !addr.Actionable() {
		return engine.Spec{}, fmt.Errorf("target %s does not lower to an action in build mode", addr)
	}

	concreteModule, err := s.resolveModule(ctx, addr.Module)
	if errors.Is(err, ErrDeferred) {
		return engine.Spec{}, fmt.Errorf("target %s depends on deferred module instances", addr)
	}
	if err != nil {
		return engine.Spec{}, err
	}

	concreteAddr := addr
	concreteAddr.Module = concreteModule

	targetDeclAddr := concreteAddr
	targetDeclAddr.Key = catalog.NoKey()

	lookupAddr := targetDeclAddr
	lookupAddr.Module = lookupAddr.Module.Declaration()
	target, ok := s.catalog.Target(lookupAddr)
	if !ok {
		return engine.Spec{}, fmt.Errorf("unknown build target %s", lookupAddr)
	}
	if !target.ConfigValid {
		return engine.Spec{}, fmt.Errorf("target %s has invalid build configuration and cannot be lowered into an action", lookupAddr)
	}

	if concreteAddr.Key.Kind == catalog.KeyKindNone {
		instances, err := s.instances.Get(ctx, targetDeclAddr)
		if err != nil {
			return engine.Spec{}, err
		}
		if len(instances.Addrs) == 0 {
			return engine.Spec{}, fmt.Errorf("target %s does not produce any instances in build mode", targetDeclAddr)
		}
		if len(instances.Addrs) > 1 || instances.Addrs[0].Key.Kind != catalog.KeyKindNone {
			return engine.Spec{}, fmt.Errorf("target %s expands to one or more concrete instances; use ActionSpecs or a concrete instance address", targetDeclAddr)
		}
	}

	targetConfig, err := s.targets.Get(ctx, concreteAddr)
	if err != nil {
		return engine.Spec{}, err
	}
	if targetConfig.Deferred {
		return engine.Spec{}, fmt.Errorf("target %s depends on deferred values and cannot be lowered into an executable action yet", concreteAddr)
	}

	execDeps, execDepKeys, err := s.collectExecDeps(ctx, concreteAddr, targetConfig)
	if err != nil {
		return engine.Spec{}, err
	}

	afterAddrs, err := s.expandExplicitDeps(ctx, rewriteExplicitDeps(target.ExplicitDeps, target.Addr.Module, concreteAddr.Module))
	if err != nil {
		return engine.Spec{}, err
	}

	after := make([]digest.Digest, 0, len(afterAddrs))
	afterKeys := make([]string, 0, len(afterAddrs))
	afterSeen := make(map[digest.Digest]struct{}, len(afterAddrs))
	for _, dep := range afterAddrs {
		actionSet, err := s.actionSetForTargetRef(ctx, dep)
		if err != nil {
			return engine.Spec{}, err
		}
		for _, spec := range actionSet.Specs {
			if _, ok := afterSeen[spec.Key]; ok {
				continue
			}
			afterSeen[spec.Key] = struct{}{}
			after = append(after, spec.Key)
			afterKeys = append(afterKeys, spec.Key.String())
		}
	}

	switch {
	case targetConfig.Payload != nil:
		payload := targetConfig.Payload
		payload.ValueAdapter = buildrun.NormalizeValueAdapter(payload.ValueAdapter)
		valueDataKey := digest.Digest{}
		if len(payload.ValueData) != 0 {
			valueDataKey = digest.FromBytes(payload.ValueData)
		}
		key, err := digest.FromValue(struct {
			Version string
			Module  catalog.ModulePathKey
			Kind    catalog.TargetKind
			Type    string
			Name    string
			Key     catalog.Key
			Request digest.Digest
			Adapter buildrun.ValueAdapter
			Value   digest.Digest
			Exec    []string
			After   []string
			Locks   []string
		}{
			Version: "build-run-action-v1",
			Module:  concreteAddr.Module.Identity(),
			Kind:    concreteAddr.Kind,
			Type:    concreteAddr.Type,
			Name:    concreteAddr.Name,
			Key:     concreteAddr.Key,
			Request: payload.Request.Key,
			Adapter: payload.ValueAdapter,
			Value:   valueDataKey,
			Exec:    execDepKeys,
			After:   afterKeys,
			Locks:   nil,
		})
		if err != nil {
			return engine.Spec{}, fmt.Errorf("failed to build action key for %s: %w", concreteAddr, err)
		}

		spec := engine.Spec{
			Key:       key,
			Name:      concreteAddr.String(),
			Class:     string(concreteAddr.Kind),
			ExecDeps:  execDeps,
			After:     after,
			Cacheable: false,
			Volatile:  true,
			Runner: engine.RunnerSpec{
				Kind:    string(catalog.RunnerKindBuiltinRun),
				Payload: *payload,
			},
			Source: engineSourceRef(target.Source),
		}
		if regErr := s.registerSpec(ctx, spec); regErr != nil {
			return engine.Spec{}, regErr
		}
		return spec, nil
	case targetConfig.Value != cty.NilVal:
		if s.providers == nil {
			return engine.Spec{}, fmt.Errorf("build solver has no provider session for target capabilities")
		}
		binding, bindingDiags := s.TargetBinding(ctx, concreteAddr.Module, target)
		if bindingDiags.HasErrors() {
			return engine.Spec{}, wrapDiags(bindingDiags)
		}

		runnerKind := runnerKindForTarget(concreteAddr.Kind)
		schema, schemaDiags := s.providers.TargetSchema(ctx, runnerKind, binding.Provider, concreteAddr.Type)
		if schemaDiags.HasErrors() {
			return engine.Spec{}, wrapDiags(schemaDiags)
		}
		if schema == nil {
			return engine.Spec{}, wrapDiags(buildprovider.UnsupportedTargetDiagnostics(runnerKind, binding.Provider, concreteAddr.Type, target.Source))
		}
		targetRequest, requestDiags := buildprovider.BuildTargetRequest(runnerKind, binding.Provider, concreteAddr.Type, schema, targetConfig.Value, diagSourceRange(target.Source))
		if requestDiags.HasErrors() {
			return engine.Spec{}, wrapDiags(requestDiags)
		}

		capability, ok := s.providers.Capability(binding.Provider, runnerKind, concreteAddr.Type)
		if !ok || !capability.Supported {
			return engine.Spec{}, wrapDiags(buildprovider.UnsupportedTargetDiagnostics(runnerKind, binding.Provider, concreteAddr.Type, target.Source))
		}
		policy, policyDiags := capability.ResolvePolicy(*targetRequest, binding)
		if policyDiags.HasErrors() {
			return engine.Spec{}, wrapDiags(policyDiags)
		}

		lockKeys := append([]string(nil), policy.LockKeys...)
		slices.Sort(lockKeys)
		locks := make([]engine.LockKey, 0, len(lockKeys))
		for _, key := range lockKeys {
			locks = append(locks, engine.LockKey(key))
		}

		key, err := digest.FromValue(struct {
			Version    string
			Module     catalog.ModulePathKey
			Kind       catalog.TargetKind
			Type       string
			Name       string
			Key        catalog.Key
			Request    digest.Digest
			Binding    digest.Digest
			Capability string
			Exec       []string
			After      []string
			Locks      []string
		}{
			Version:    "build-action-v1",
			Module:     concreteAddr.Module.Identity(),
			Kind:       concreteAddr.Kind,
			Type:       concreteAddr.Type,
			Name:       concreteAddr.Name,
			Key:        concreteAddr.Key,
			Request:    targetRequest.Key,
			Binding:    binding.ConfigKey,
			Capability: capability.Revision,
			Exec:       execDepKeys,
			After:      afterKeys,
			Locks:      lockKeys,
		})
		if err != nil {
			return engine.Spec{}, fmt.Errorf("failed to build action key for %s: %w", concreteAddr, err)
		}

		spec := engine.Spec{
			Key:       key,
			Name:      concreteAddr.String(),
			Class:     string(concreteAddr.Kind),
			ExecDeps:  execDeps,
			After:     after,
			Locks:     locks,
			Cacheable: policy.Cacheable,
			Volatile:  policy.Volatile,
			Runner: engine.RunnerSpec{
				Kind: string(runnerKind),
				Payload: buildprovider.TargetPayload{
					Binding:   binding,
					TypeName:  concreteAddr.Type,
					ConfigKey: target.ConfigKey,
					Request:   *targetRequest,
				},
			},
			Source: engineSourceRef(target.Source),
		}
		if regErr := s.registerSpec(ctx, spec); regErr != nil {
			return engine.Spec{}, regErr
		}
		return spec, nil
	default:
		if target.ProviderLocal.LocalName != "" {
			_, bindingDiags := s.TargetBinding(ctx, concreteAddr.Module, target)
			if bindingDiags.HasErrors() {
				return engine.Spec{}, wrapDiags(bindingDiags)
			}
		}
		return engine.Spec{}, fmt.Errorf("target %s does not have a lowered executable request", concreteAddr)
	}
}

// collectExecDeps walks the target config's refs and computes action specs for
// each dependency. This replaces the memoized ValueDeps queries — the logic runs
// inline inside computeActionSpec. Each dep's action spec is registered and
// submitted to the engine as a side effect.
func (s *Solver) collectExecDeps(ctx context.Context, addr catalog.Addr, config EvalResult) ([]digest.Digest, []string, error) {
	collector := newExecDepCollector()

	for _, ref := range config.Refs {
		if err := s.collectExecDepForRef(ctx, collector, ref); err != nil {
			return nil, nil, err
		}
	}

	if err := s.collectProviderFunctionExecDeps(ctx, collector, config.ProviderFunctionCalls, addr); err != nil {
		return nil, nil, err
	}

	keys, keyStrs := collector.finalize()
	return keys, keyStrs, nil
}

type execDepCollector struct {
	specs []engine.Spec
	seen  map[digest.Digest]struct{}
}

func newExecDepCollector() *execDepCollector {
	return &execDepCollector{
		seen: make(map[digest.Digest]struct{}),
	}
}

func (c *execDepCollector) add(spec engine.Spec) {
	if _, ok := c.seen[spec.Key]; ok {
		return
	}
	c.seen[spec.Key] = struct{}{}
	c.specs = append(c.specs, spec)
}

func (c *execDepCollector) finalize() ([]digest.Digest, []string) {
	keys := make([]digest.Digest, 0, len(c.specs))
	for _, spec := range c.specs {
		keys = append(keys, spec.Key)
	}
	slices.SortFunc(keys, func(a, b digest.Digest) int {
		return bytes.Compare(a[:], b[:])
	})
	strs := make([]string, 0, len(keys))
	for _, k := range keys {
		strs = append(strs, k.String())
	}
	return keys, strs
}

func (s *Solver) collectExecDepForRef(ctx context.Context, collector *execDepCollector, ref catalog.Addr) error {
	switch ref.Kind {
	case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
		if ref.Key.Kind == catalog.KeyKindNone {
			return s.collectExecDepForTargetSet(ctx, collector, ref)
		}
		spec, err := s.actions.Get(ctx, ref)
		if err != nil {
			return err
		}
		if regErr := s.registerSpec(ctx, spec); regErr != nil {
			return regErr
		}
		collector.add(spec)
		return nil
	case catalog.TargetKindOutput:
		return nil
	case catalog.TargetKindModule:
		modules, err := s.concreteModulesForRef(ctx, ref.Module)
		if err != nil {
			return err
		}
		for _, module := range modules {
			if err := s.collectExecDepsForModule(ctx, collector, module); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported exec dependency %s", ref)
	}
}

func (s *Solver) collectExecDepForTargetSet(ctx context.Context, collector *execDepCollector, ref catalog.Addr) error {
	instances, err := s.instances.Get(ctx, ref)
	if err != nil {
		return err
	}
	for _, instance := range instances.Addrs {
		spec, err := s.actions.Get(ctx, instance)
		if err != nil {
			return err
		}
		if regErr := s.registerSpec(ctx, spec); regErr != nil {
			return regErr
		}
		collector.add(spec)
	}
	return nil
}

func (s *Solver) collectExecDepsForModule(ctx context.Context, collector *execDepCollector, module catalog.ModulePath) error {
	pkg, ok := s.catalog.Package(module.Declaration())
	if !ok {
		return fmt.Errorf("unknown module %s for exec dep collection", catalog.ModuleAddr(module))
	}
	for _, output := range pkg.Outputs {
		outputAddr := output.Addr
		outputAddr.Module = module
		result, err := s.outputEvals.Get(ctx, outputAddr)
		if err != nil {
			return err
		}
		for _, ref := range result.Refs {
			if err := s.collectExecDepForRef(ctx, collector, ref); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Solver) collectProviderFunctionExecDeps(ctx context.Context, collector *execDepCollector, calls []ProviderFunctionCall, addr catalog.Addr) error {
	querySafety := buildprovider.DefaultQuerySafetyRegistry()
	for _, call := range calls {
		if querySafety.IsQuerySafe(call.Ref.Function) {
			continue
		}
		if call.Request == nil {
			return fmt.Errorf("provider function %s in %s could not be lowered into an executable build request from pure or query-safe inputs", call.Ref.Function, addr)
		}

		declAddr := addr
		declAddr.Module = declAddr.Module.Declaration()
		declAddr.Key = catalog.NoKey()
		target, ok := s.catalog.Target(declAddr)
		if !ok {
			continue
		}
		binding, bindingDiags := s.TargetBinding(ctx, addr.Module, target)
		if bindingDiags.HasErrors() {
			return wrapDiags(bindingDiags)
		}
		spec := s.ProviderFunctionSpec(addr, target.Source, binding, call)
		if regErr := s.registerSpec(ctx, spec); regErr != nil {
			return regErr
		}
		collector.add(spec)
	}
	return nil
}
