// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"context"
	"sync"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	commandtesting "github.com/opentofu/opentofu/internal/command/testing"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type fakeEvaluator struct {
	importInstancesFn func(context.Context, EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics)
	targetInstancesFn func(context.Context, EvalTargetInstancesRequest) (InstanceResult, tfdiags.Diagnostics)
	providerFn        func(context.Context, EvalProviderRequest) (EvalResult, tfdiags.Diagnostics)
	targetFn          func(context.Context, EvalTargetRequest) (EvalResult, tfdiags.Diagnostics)
	outputFn          func(context.Context, EvalOutputRequest) (EvalResult, tfdiags.Diagnostics)
}

func (e fakeEvaluator) EvalImportInstances(ctx context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
	if e.importInstancesFn != nil {
		return e.importInstancesFn(ctx, req)
	}
	return InstanceResult{Keys: []catalog.Key{catalog.NoKey()}, Shape: InstanceShapeSingle}, nil
}

func (e fakeEvaluator) EvalTargetInstances(ctx context.Context, req EvalTargetInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
	if e.targetInstancesFn != nil {
		return e.targetInstancesFn(ctx, req)
	}
	return InstanceResult{Keys: []catalog.Key{catalog.NoKey()}, Shape: InstanceShapeSingle}, nil
}

func (e fakeEvaluator) EvalProvider(ctx context.Context, req EvalProviderRequest) (EvalResult, tfdiags.Diagnostics) {
	if e.providerFn != nil {
		return e.providerFn(ctx, req)
	}
	config := cty.EmptyObjectVal
	if req.Schema != nil && req.Schema.Block != nil {
		config = req.Schema.Block.EmptyValue()
	}
	return EvalResult{Value: config, Digest: digest.FromStrings("provider-config"), Known: true}, nil
}

func (e fakeEvaluator) EvalTarget(ctx context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
	if e.targetFn != nil {
		return e.targetFn(ctx, req)
	}
	return EvalResult{
		Payload: &buildrun.Payload{
			Request:      buildrun.Request{Key: digest.FromStrings("fake-request", req.Addr.String())},
			ValueAdapter: buildrun.ValueAdapterRawV1,
		},
		Digest: digest.FromStrings("target-config", req.Addr.String()),
		Known:  true,
	}, nil
}

func (e fakeEvaluator) EvalOutput(ctx context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
	if e.outputFn != nil {
		return e.outputFn(ctx, req)
	}
	return EvalResult{Value: cty.EmptyObjectVal, Digest: digest.FromStrings("output", req.Addr.String()), Known: true}, nil
}


type countingEvaluator struct {
	base Evaluator

	mu          sync.Mutex
	targetCalls map[catalog.AddrKey]int
	outputCalls map[catalog.AddrKey]int
}

func (e *countingEvaluator) EvalImportInstances(ctx context.Context, req EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
	return e.base.EvalImportInstances(ctx, req)
}

func (e *countingEvaluator) EvalTargetInstances(ctx context.Context, req EvalTargetInstancesRequest) (InstanceResult, tfdiags.Diagnostics) {
	return e.base.EvalTargetInstances(ctx, req)
}

func (e *countingEvaluator) EvalProvider(ctx context.Context, req EvalProviderRequest) (EvalResult, tfdiags.Diagnostics) {
	return e.base.EvalProvider(ctx, req)
}

func (e *countingEvaluator) EvalTarget(ctx context.Context, req EvalTargetRequest) (EvalResult, tfdiags.Diagnostics) {
	e.mu.Lock()
	if e.targetCalls == nil {
		e.targetCalls = make(map[catalog.AddrKey]int)
	}
	e.targetCalls[req.Addr.Identity()]++
	e.mu.Unlock()
	return e.base.EvalTarget(ctx, req)
}

func (e *countingEvaluator) EvalOutput(ctx context.Context, req EvalOutputRequest) (EvalResult, tfdiags.Diagnostics) {
	e.mu.Lock()
	if e.outputCalls == nil {
		e.outputCalls = make(map[catalog.AddrKey]int)
	}
	e.outputCalls[req.Addr.Identity()]++
	e.mu.Unlock()
	return e.base.EvalOutput(ctx, req)
}


func rootPkg(targets []*catalog.TargetDecl, outputs []*catalog.OutputDecl, children map[string]*catalog.Package) *catalog.Package {
	pkg := &catalog.Package{
		Name:     "root",
		Module:   catalog.RootModule(),
		Children: children,
	}
	if pkg.Children == nil {
		pkg.Children = map[string]*catalog.Package{}
	}
	pkg.Targets = targets
	pkg.Outputs = outputs
	return pkg
}

func childPkg(parent *catalog.Package, name string, targets []*catalog.TargetDecl, outputs []*catalog.OutputDecl, providers []*catalog.ProviderDecl) *catalog.Package {
	module := parent.Module.Child(name, catalog.NoKey())
	imp := catalog.Import{
		Name:         name,
		Module:       parent.Module,
		TargetModule: module,
	}
	parent.Imports = append(parent.Imports, imp)

	child := &catalog.Package{
		Name:      name,
		Module:    module,
		Parent:    parent,
		Targets:   targets,
		Outputs:   outputs,
		Providers: providers,
		Children:  map[string]*catalog.Package{},
	}
	child.ParentImport = &parent.Imports[len(parent.Imports)-1]
	parent.Children[name] = child
	if parent.ChildOrder == nil {
		parent.ChildOrder = []string{}
	}
	parent.ChildOrder = append(parent.ChildOrder, name)
	return child
}

func knownTargetResult(addr catalog.Addr) EvalResult {
	return EvalResult{
		Payload: &buildrun.Payload{
			Request:      buildrun.Request{Key: digest.FromStrings("fake-request", addr.String())},
			ValueAdapter: buildrun.ValueAdapterRawV1,
		},
		Digest: digest.FromStrings("target-config", addr.String()),
		Known:  true,
	}
}

func targetDecl(module catalog.ModulePath, kind catalog.TargetKind, typeName, name string) *catalog.TargetDecl {
	addr := catalog.ResourceAddr(module, kind, typeName, name, catalog.NoKey())
	return &catalog.TargetDecl{
		Addr:          addr,
		Driver:        "test",
		ConfigKey:     digest.FromStrings("target", addr.String()),
		ConfigValid:   true,
		ProviderLocal: addrs.LocalProviderConfig{LocalName: "test"},
	}
}

func outputDecl(module catalog.ModulePath, name string) *catalog.OutputDecl {
	return &catalog.OutputDecl{
		Addr: catalog.OutputAddr(module, name),
	}
}

func newTestProviderSession(t *testing.T) *buildprovider.Session {
	t.Helper()
	provider := commandtesting.NewProvider(nil)
	return buildprovider.NewSession(buildprovider.SessionConfig{
		Factories: map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("test"):                                    providers.FactoryFixed(provider.Provider),
			addrs.NewProvider(addrs.DefaultProviderRegistryHost, "foo", "test"): providers.FactoryFixed(provider.Provider),
		},
	})
}

func newTestEngineWithRecords(t *testing.T, records map[digest.Digest]engine.Record) *engine.Engine {
	t.Helper()
	return engine.New(engine.Config{
		Context: t.Context(),
		Runner: engine.RunnerFunc(func(_ context.Context, spec engine.Spec) (engine.RunResult, error) {
			if rec, ok := records[spec.Key]; ok {
				return engine.RunResult{
					OutputKey: rec.OutputKey,
					Payload:   rec.Payload,
				}, nil
			}
			return engine.RunResult{Payload: []byte(spec.Name)}, nil
		}),
	})
}
