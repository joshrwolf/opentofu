// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	buildrun "github.com/opentofu/opentofu/internal/build/run"

	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

type solverRuntimeResolver struct {
	solver *Solver
}

func resolveRuntimeResult(result EvalResult, err error) (cty.Value, bool, tfdiags.Diagnostics) {
	if err != nil {
		return cty.NilVal, false, unwrapDiags(err)
	}
	return result.Value, result.Known, nil
}

func (r solverRuntimeResolver) TargetValue(ctx context.Context, addr catalog.Addr) (cty.Value, bool, tfdiags.Diagnostics) {
	return resolveRuntimeResult(r.solver.targetCollections.Get(ctx, addr))
}

func (r solverRuntimeResolver) OutputValue(ctx context.Context, addr catalog.Addr) (cty.Value, bool, tfdiags.Diagnostics) {
	return resolveRuntimeResult(r.solver.outputs.Get(ctx, addr))
}

func (r solverRuntimeResolver) ModuleValue(ctx context.Context, module catalog.ModulePath) (cty.Value, bool, tfdiags.Diagnostics) {
	return resolveRuntimeResult(r.solver.moduleCollections.Get(ctx, module))
}

func (s *Solver) computeTargetValue(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
	if s.engine == nil {
		return EvalResult{}, fmt.Errorf("target %s requires engine execution", addr)
	}

	spec, err := s.actions.Get(ctx, addr)
	if err != nil {
		return EvalResult{}, err
	}

	result, err := s.engine.Result(ctx, spec.Key)
	if err != nil {
		return EvalResult{}, err
	}

	declAddr := addr
	declAddr.Module = declAddr.Module.Declaration()
	declAddr.Key = catalog.NoKey()
	target, ok := s.catalog.Target(declAddr)
	if !ok {
		return EvalResult{}, fmt.Errorf("unknown target %s", declAddr)
	}

	value, valueDigest, valueDiags := decodeRuntimeTargetValue(spec, target, result.Record)
	if valueDiags.HasErrors() {
		return EvalResult{}, wrapDiags(valueDiags)
	}

	if valueDigest == (digest.Digest{}) {
		valueDigest = result.Record.OutputKey
	}
	return EvalResult{
		Value:    value,
		Digest:   valueDigest,
		Known:    true,
		Volatile: result.Record.Volatile,
	}, nil
}

func decodeRuntimeTargetValue(spec engine.Spec, target *catalog.TargetDecl, record engine.Record) (cty.Value, digest.Digest, tfdiags.Diagnostics) {
	rng := tfdiags.SourceRange{}
	if target != nil {
		rng = diagSourceRange(target.Source)
	}

	switch spec.Runner.Kind {
	case string(catalog.RunnerKindProviderResource), string(catalog.RunnerKindProviderData):
		payload, ok := spec.Runner.Payload.(buildprovider.TargetPayload)
		if !ok {
			return cty.NilVal, digest.Digest{}, sourcelessError("Target " + spec.Name + " does not have a provider-target payload.")
		}
		return buildprovider.DecodeTargetResult(payload.Request, record.Payload, rng)
	case string(catalog.RunnerKindBuiltinRun):
		payload, ok := spec.Runner.Payload.(buildrun.Payload)
		if !ok {
			return cty.NilVal, digest.Digest{}, sourcelessError("Target " + spec.Name + " does not have a build-run payload.")
		}
		return buildrun.DecodeValue(payload.Request, record.Payload, payload.ValueAdapter, payload.ValueData, rng)
	default:
		return cty.NilVal, digest.Digest{}, sourcelessError("Target " + spec.Name + " uses unsupported runtime runner kind " + string(spec.Runner.Kind) + ".")
	}
}

func (s *Solver) computeTargetValueCollection(ctx context.Context, addr catalog.Addr) (EvalResult, error) {
	concreteModule, err := s.resolveModule(ctx, addr.Module)
	if errors.Is(err, ErrDeferred) {
		return EvalResult{}, err
	}
	if err != nil {
		return EvalResult{}, err
	}

	declAddr := addr
	declAddr.Module = concreteModule
	declAddr.Key = catalog.NoKey()

	instances, err := s.instances.Get(ctx, declAddr)
	if err != nil {
		return EvalResult{}, err
	}

	values, digests, err := s.targetCollectionElements(ctx, instances.Addrs)
	if err != nil {
		return EvalResult{}, err
	}
	if len(values) == 0 {
		switch instances.Shape {
		case InstanceShapeList:
			return EvalResult{
				Value:  cty.EmptyTupleVal,
				Digest: collectionDigest(instances.Shape, nil),
				Known:  true,
			}, nil
		case InstanceShapeMap:
			return EvalResult{
				Value:  cty.EmptyObjectVal,
				Digest: collectionDigest(instances.Shape, nil),
				Known:  true,
			}, nil
		case InstanceShapeOptional:
			ty, err := s.targetValueType(ctx, declAddr)
			if err != nil {
				return EvalResult{}, err
			}
			return EvalResult{
				Value:  cty.NullVal(ty),
				Digest: collectionDigest(instances.Shape, nil),
				Known:  true,
			}, nil
		default:
			return EvalResult{}, fmt.Errorf("target %s has empty instances with unsupported shape %s", declAddr, instances.Shape)
		}
	}

	data, buildDiags := buildTargetCollectionValue(instances.Shape, instances.Addrs, values)
	if buildDiags.HasErrors() {
		return EvalResult{}, wrapDiags(buildDiags)
	}
	return EvalResult{
		Value:  data,
		Digest: collectionDigest(instances.Shape, digests),
		Known:  true,
	}, nil
}

func (s *Solver) computeModuleValueCollection(ctx context.Context, module catalog.ModulePath) (EvalResult, error) {
	packages, err := s.packages.Get(ctx, module)
	if err != nil {
		return EvalResult{}, err
	}

	values, digests, err := s.moduleCollectionElements(ctx, packages.Modules)
	if err != nil {
		return EvalResult{}, err
	}
	if len(values) == 0 {
		switch packages.Shape {
		case InstanceShapeList:
			return EvalResult{
				Value:  cty.EmptyTupleVal,
				Digest: collectionDigest(packages.Shape, nil),
				Known:  true,
			}, nil
		case InstanceShapeMap:
			return EvalResult{
				Value:  cty.EmptyObjectVal,
				Digest: collectionDigest(packages.Shape, nil),
				Known:  true,
			}, nil
		case InstanceShapeOptional:
			return EvalResult{
				Value:  cty.NullVal(cty.DynamicPseudoType),
				Digest: collectionDigest(packages.Shape, nil),
				Known:  true,
			}, nil
		default:
			return EvalResult{}, fmt.Errorf("module %s has empty instances with unsupported shape %s", catalog.ModuleAddr(module), packages.Shape)
		}
	}

	data, buildDiags := buildModuleCollectionValue(packages.Shape, packages.Modules, values)
	if buildDiags.HasErrors() {
		return EvalResult{}, wrapDiags(buildDiags)
	}
	return EvalResult{
		Value:  data,
		Digest: collectionDigest(packages.Shape, digests),
		Known:  true,
	}, nil
}

// targetCollectionElements pre-submits all instance action specs in parallel,
// then collects values in stable order.
func (s *Solver) targetCollectionElements(ctx context.Context, addrs []catalog.Addr) ([]cty.Value, []digest.Digest, error) {
	// Pre-submit: warm the dice cache for all instance action specs in
	// parallel so the engine can execute them concurrently. Errors are
	// not checked here — they are memoized by dice and will surface
	// when targetValues.Get calls actions.Get in the sequential phase.
	if len(addrs) > 1 {
		var wg sync.WaitGroup
		for _, addr := range addrs {
			wg.Go(func() {
				s.actions.Get(ctx, addr)
			})
		}
		wg.Wait()
	}

	values := make([]cty.Value, 0, len(addrs))
	digests := make([]digest.Digest, 0, len(addrs))

	for _, addr := range addrs {
		result, err := s.targetValues.Get(ctx, addr)
		if err != nil {
			return nil, nil, err
		}
		if !result.Known {
			return nil, nil, nil
		}
		values = append(values, result.Value)
		digests = append(digests, result.Digest)
	}

	return values, digests, nil
}

func (s *Solver) moduleCollectionElements(ctx context.Context, modules []catalog.ModulePath) ([]cty.Value, []digest.Digest, error) {
	// Pre-submit: warm the dice cache for all module output evaluations
	// in parallel so the engine can execute their dependencies concurrently.
	if len(modules) > 1 {
		var wg sync.WaitGroup
		for _, module := range modules {
			wg.Go(func() {
				s.moduleInstanceValue(ctx, module)
			})
		}
		wg.Wait()
	}

	values := make([]cty.Value, 0, len(modules))
	digests := make([]digest.Digest, 0, len(modules))

	for _, module := range modules {
		value, valueDigest, err := s.moduleInstanceValue(ctx, module)
		if err != nil {
			return nil, nil, err
		}
		if value == cty.NilVal {
			return nil, nil, nil
		}
		values = append(values, value)
		digests = append(digests, valueDigest)
	}

	return values, digests, nil
}

func (s *Solver) moduleInstanceValue(ctx context.Context, module catalog.ModulePath) (cty.Value, digest.Digest, error) {
	pkg, ok := s.catalog.Package(module.Declaration())
	if !ok {
		return cty.NilVal, digest.Digest{}, fmt.Errorf("unknown module %s", catalog.ModuleAddr(module))
	}

	names := make([]string, 0, len(pkg.Outputs))
	byName := make(map[string]*catalog.OutputDecl, len(pkg.Outputs))
	for _, output := range pkg.Outputs {
		if output == nil {
			continue
		}
		names = append(names, output.Addr.Name)
		byName[output.Addr.Name] = output
	}
	slices.Sort(names)

	values := make(map[string]cty.Value, len(names))
	childDigests := make([]digest.Digest, 0, len(names))
	for _, name := range names {
		output := byName[name]
		addr := output.Addr
		addr.Module = module
		result, err := s.outputs.Get(ctx, addr)
		if err != nil {
			return cty.NilVal, digest.Digest{}, err
		}
		if !result.Known {
			values[name] = cty.DynamicVal
			childDigests = append(childDigests, digest.Digest{})
			continue
		}
		values[name] = result.Value
		childDigests = append(childDigests, result.Digest)
	}

	if len(values) == 0 {
		return cty.EmptyObjectVal, collectionDigest(InstanceShapeSingle, childDigests), nil
	}
	return cty.ObjectVal(values), collectionDigest(InstanceShapeSingle, childDigests), nil
}


func buildTargetCollectionValue(shape InstanceShape, addrs []catalog.Addr, values []cty.Value) (cty.Value, tfdiags.Diagnostics) {
	switch shape {
	case InstanceShapeSingle:
		if len(values) != 1 {
			return cty.NilVal, sourcelessError("Build target collection did not resolve to exactly one singleton value.")
		}
		return values[0], nil
	case InstanceShapeOptional:
		if len(values) == 0 {
			return cty.NilVal, nil
		}
		if len(values) != 1 {
			return cty.NilVal, sourcelessError("Build target collection resolved to more than one optional value.")
		}
		return values[0], nil
	case InstanceShapeList:
		order := make([]int, 0, len(addrs))
		for i := range addrs {
			order = append(order, i)
		}
		slices.SortFunc(order, func(i, j int) int {
			return cmp.Compare(addrs[i].Key.Int, addrs[j].Key.Int)
		})
		ret := make([]cty.Value, 0, len(order))
		for _, idx := range order {
			ret = append(ret, values[idx])
		}
		return cty.TupleVal(ret), nil
	case InstanceShapeMap:
		ret := make(map[string]cty.Value, len(addrs))
		for i, addr := range addrs {
			ret[addr.Key.Str] = values[i]
		}
		if len(ret) == 0 {
			return cty.EmptyObjectVal, nil
		}
		return cty.ObjectVal(ret), nil
	default:
		return cty.NilVal, sourcelessError("Unsupported build target collection shape " + string(shape) + ".")
	}
}

func buildModuleCollectionValue(shape InstanceShape, modules []catalog.ModulePath, values []cty.Value) (cty.Value, tfdiags.Diagnostics) {
	switch shape {
	case InstanceShapeSingle:
		if len(values) != 1 {
			return cty.NilVal, sourcelessError("Build module collection did not resolve to exactly one singleton value.")
		}
		return values[0], nil
	case InstanceShapeOptional:
		if len(values) == 0 {
			return cty.NilVal, nil
		}
		if len(values) != 1 {
			return cty.NilVal, sourcelessError("Build module collection resolved to more than one optional value.")
		}
		return values[0], nil
	case InstanceShapeList:
		order := make([]int, 0, len(modules))
		for i := range modules {
			order = append(order, i)
		}
		slices.SortFunc(order, func(i, j int) int {
			stepI, _ := modules[i].LastStep()
			stepJ, _ := modules[j].LastStep()
			return cmp.Compare(stepI.Key.Int, stepJ.Key.Int)
		})
		ret := make([]cty.Value, 0, len(order))
		for _, idx := range order {
			ret = append(ret, values[idx])
		}
		return cty.TupleVal(ret), nil
	case InstanceShapeMap:
		ret := make(map[string]cty.Value, len(modules))
		for i, module := range modules {
			step, _ := module.LastStep()
			ret[step.Key.Str] = values[i]
		}
		if len(ret) == 0 {
			return cty.EmptyObjectVal, nil
		}
		return cty.ObjectVal(ret), nil
	default:
		return cty.NilVal, sourcelessError("Unsupported build module collection shape " + string(shape) + ".")
	}
}

func (s *Solver) targetValueType(ctx context.Context, declAddr catalog.Addr) (cty.Type, error) {
	if s.providers == nil {
		return cty.DynamicPseudoType, nil
	}
	lookupAddr := declAddr
	lookupAddr.Module = lookupAddr.Module.Declaration()
	target, ok := s.catalog.Target(lookupAddr)
	if !ok {
		return cty.DynamicPseudoType, fmt.Errorf("unknown target %s", lookupAddr)
	}
	binding, diags := s.TargetBinding(ctx, declAddr.Module, target)
	if diags.HasErrors() {
		return cty.DynamicPseudoType, wrapDiags(diags)
	}
	schema, schemaDiags := s.providers.TargetSchema(ctx, runnerKindForTarget(declAddr.Kind), binding.Provider, declAddr.Type)
	if schemaDiags.HasErrors() {
		return cty.DynamicPseudoType, wrapDiags(schemaDiags)
	}
	if schema == nil || schema.Block == nil {
		return cty.DynamicPseudoType, nil
	}
	return schema.Block.ImpliedType(), nil
}

func collectionDigest(shape InstanceShape, digests []digest.Digest) digest.Digest {
	parts := make([]string, 0, len(digests)+2)
	parts = append(parts, "build-runtime-collection-v1", string(shape))
	for _, d := range digests {
		parts = append(parts, d.String())
	}
	return digest.FromStrings(parts...)
}

func diagSourceRange(src catalog.SourceRef) tfdiags.SourceRange {
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
