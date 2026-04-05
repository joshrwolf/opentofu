// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"sync/atomic"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/plugins"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
)

func TestContextBuild_basic(t *testing.T) {
	t.Parallel()
	m := testModule(t, "build-basic")

	var applyCount int32
	p := testBuildProvider(t, "aws", &applyCount)

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}, nil),
	})

	state, diags := ctx.Build(t.Context(), m, states.NewState(), &BuildOpts{})
	assertNoErrors(t, diags)

	if count := atomic.LoadInt32(&applyCount); count != 2 {
		t.Fatalf("expected 2 ApplyResourceChange calls, got %d", count)
	}

	mod := state.RootModule()
	if len(mod.Resources) != 2 {
		t.Fatalf("expected 2 resources in state, got %d", len(mod.Resources))
	}

	for _, rs := range mod.Resources {
		for key, inst := range rs.Instances {
			if inst.Current == nil {
				t.Fatalf("resource %s[%s] has no current instance", rs.Addr, key)
			}
			if inst.Current.ContentHash == "" {
				t.Fatalf("resource %s[%s] has no content hash", rs.Addr, key)
			}
		}
	}

	if p.PlanResourceChangeCalled {
		t.Fatal("PlanResourceChange was called, but Build should not use it")
	}
}

func TestContextBuild_cacheHit(t *testing.T) {
	t.Parallel()
	m := testModule(t, "build-basic")

	var applyCount int32
	p := testBuildProvider(t, "aws", &applyCount)

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}, nil),
	})

	state1, diags := ctx.Build(t.Context(), m, states.NewState(), &BuildOpts{})
	assertNoErrors(t, diags)

	if count := atomic.LoadInt32(&applyCount); count != 2 {
		t.Fatalf("expected 2 applies on first run, got %d", count)
	}

	// Second build: same config, same state → full cache hit.
	atomic.StoreInt32(&applyCount, 0)
	state2, diags := ctx.Build(t.Context(), m, state1, &BuildOpts{})
	assertNoErrors(t, diags)

	if count := atomic.LoadInt32(&applyCount); count != 0 {
		t.Fatalf("expected 0 applies on second run (cache hit), got %d", count)
	}
	if len(state2.RootModule().Resources) != 2 {
		t.Fatalf("expected 2 resources after cached build, got %d", len(state2.RootModule().Resources))
	}
}

func TestContextBuild_cacheMissOnConfigChange(t *testing.T) {
	t.Parallel()

	var applyCount int32
	p := testBuildProvider(t, "aws", &applyCount)

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}, nil),
	})

	state1, diags := ctx.Build(t.Context(), testModule(t, "build-basic"), states.NewState(), &BuildOpts{})
	assertNoErrors(t, diags)

	// Second build with changed config → cache miss.
	atomic.StoreInt32(&applyCount, 0)
	_, diags = ctx.Build(t.Context(), testModule(t, "build-changed"), state1, &BuildOpts{})
	assertNoErrors(t, diags)

	if count := atomic.LoadInt32(&applyCount); count == 0 {
		t.Fatal("expected at least 1 apply on second run (config changed), got 0")
	}
}

func TestContextBuild_stateHasContentHash(t *testing.T) {
	t.Parallel()

	var applyCount int32
	p := testBuildProvider(t, "aws", &applyCount)

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}, nil),
	})

	state, diags := ctx.Build(t.Context(), testModule(t, "build-basic"), states.NewState(), &BuildOpts{})
	assertNoErrors(t, diags)

	for _, rs := range state.RootModule().Resources {
		for key, inst := range rs.Instances {
			if len(inst.Current.ContentHash) != 64 {
				t.Errorf("resource %s[%s]: content hash length %d, want 64",
					rs.Addr, key, len(inst.Current.ContentHash))
			}
			if inst.Current.CachedAt.IsZero() {
				t.Errorf("resource %s[%s]: CachedAt is zero", rs.Addr, key)
			}
		}
	}
}

func TestContextBuild_hookCalledOnCacheHit(t *testing.T) {
	t.Parallel()

	var applyCount int32
	p := testBuildProvider(t, "aws", &applyCount)

	var postApplyCalls int32
	hook := &testBuildHook{
		postApplyFn: func(_ addrs.AbsResourceInstance, _ states.Generation, _ cty.Value, _ error) (HookAction, error) {
			atomic.AddInt32(&postApplyCalls, 1)
			return HookActionContinue, nil
		},
	}

	ctx := testContext2(t, &ContextOpts{
		Hooks: []Hook{hook},
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}, nil),
	})

	state1, diags := ctx.Build(t.Context(), testModule(t, "build-basic"), states.NewState(), &BuildOpts{})
	assertNoErrors(t, diags)

	if n := atomic.LoadInt32(&postApplyCalls); n < 2 {
		t.Fatalf("expected >= 2 PostApply on first build, got %d", n)
	}

	// Second build — cache hits should still fire hooks.
	atomic.StoreInt32(&postApplyCalls, 0)
	_, diags = ctx.Build(t.Context(), testModule(t, "build-basic"), state1, &BuildOpts{})
	assertNoErrors(t, diags)

	if n := atomic.LoadInt32(&postApplyCalls); n < 2 {
		t.Fatalf("expected >= 2 PostApply on cached build, got %d", n)
	}
}

func TestContextBuild_skipRoles(t *testing.T) {
	t.Parallel()

	var buildCount, testCount int32
	p := testBuildProviderWithRoles(t, "aws", &buildCount, &testCount)

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}, nil),
	})

	state, diags := ctx.Build(t.Context(), testModule(t, "build-basic"), states.NewState(), &BuildOpts{
		SkipRoles: []providers.ResourceRole{providers.RoleTest},
	})
	assertNoErrors(t, diags)

	// Both resources are RoleBuild (not RoleTest), so both execute.
	if len(state.RootModule().Resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(state.RootModule().Resources))
	}
	if n := atomic.LoadInt32(&buildCount); n != 2 {
		t.Fatalf("expected 2 build applies, got %d", n)
	}
}

func TestContextBuild_onlyRoles(t *testing.T) {
	t.Parallel()

	var buildCount, testCount int32
	p := testBuildProviderWithRoles(t, "aws", &buildCount, &testCount)

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}, nil),
	})

	state, diags := ctx.Build(t.Context(), testModule(t, "build-basic"), states.NewState(), &BuildOpts{
		OnlyRoles: []providers.ResourceRole{providers.RoleBuild},
	})
	assertNoErrors(t, diags)

	if len(state.RootModule().Resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(state.RootModule().Resources))
	}
	if n := atomic.LoadInt32(&buildCount); n != 2 {
		t.Fatalf("expected 2 build applies, got %d", n)
	}
}

func TestContextBuild_outputTarget(t *testing.T) {
	t.Parallel()

	var applyCount int32
	p := testBuildProvider(t, "aws", &applyCount)

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}, nil),
	})

	// Target "image_ref" which depends only on aws_instance.build.
	state, diags := ctx.Build(t.Context(), testModule(t, "build-outputs"), states.NewState(), &BuildOpts{
		OutputTargets: []string{"image_ref"},
	})
	assertNoErrors(t, diags)

	if count := atomic.LoadInt32(&applyCount); count != 1 {
		t.Fatalf("expected 1 apply (only build resource), got %d", count)
	}
	if _, ok := state.RootModule().Resources["aws_instance.build"]; !ok {
		t.Fatal("expected aws_instance.build in state")
	}
	if len(state.RootModule().Resources) != 1 {
		t.Fatalf("expected 1 resource in state, got %d", len(state.RootModule().Resources))
	}
}

func TestContextBuild_outputTargetFull(t *testing.T) {
	t.Parallel()

	var applyCount int32
	p := testBuildProvider(t, "aws", &applyCount)

	ctx := testContext2(t, &ContextOpts{
		Plugins: plugins.NewLibrary(map[addrs.Provider]providers.Factory{
			addrs.NewDefaultProvider("aws"): testProviderFuncFixed(p),
		}, nil),
	})

	// Target "tags" which transitively depends on all 3 resources.
	state, diags := ctx.Build(t.Context(), testModule(t, "build-outputs"), states.NewState(), &BuildOpts{
		OutputTargets: []string{"tags"},
	})
	assertNoErrors(t, diags)

	if count := atomic.LoadInt32(&applyCount); count != 3 {
		t.Fatalf("expected 3 applies (build + test + tag), got %d", count)
	}
	if len(state.RootModule().Resources) != 3 {
		t.Fatalf("expected 3 resources, got %d", len(state.RootModule().Resources))
	}
}

// --- Test helpers ---

// testBuildProvider creates a mock provider for build tests.
func testBuildProvider(t *testing.T, prefix string, applyCount *int32) *MockProvider {
	t.Helper()

	p := new(MockProvider)
	p.GetProviderSchemaResponse = &providers.GetProviderSchemaResponse{
		ResourceTypes: map[string]providers.Schema{
			prefix + "_instance": {
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"id":   {Type: cty.String, Computed: true},
						"ami":  {Type: cty.String, Optional: true},
						"num":  {Type: cty.Number, Optional: true},
						"foo":  {Type: cty.String, Optional: true},
						"dep":  {Type: cty.String, Optional: true},
						"type": {Type: cty.String, Computed: true},
					},
				},
			},
		},
		Provider: providers.Schema{Block: &configschema.Block{}},
	}

	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
		atomic.AddInt32(applyCount, 1)

		vals := req.PlannedState.AsValueMap()
		if vals == nil {
			vals = map[string]cty.Value{}
		}
		if id, ok := vals["id"]; !ok || id.IsNull() || !id.IsKnown() {
			vals["id"] = cty.StringVal("build-" + req.TypeName)
		}
		if ty, ok := vals["type"]; ok && (ty.IsNull() || !ty.IsKnown()) {
			vals["type"] = cty.StringVal(req.TypeName)
		}

		return providers.ApplyResourceChangeResponse{NewState: cty.ObjectVal(vals)}
	}

	p.PlanResourceChangeFn = func(req providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
		return providers.PlanResourceChangeResponse{PlannedState: req.ProposedNewState}
	}

	return p
}

// testBuildProviderWithRoles wraps testBuildProvider with BuildMetaProvider support.
func testBuildProviderWithRoles(t *testing.T, prefix string, buildCount, testCount *int32) *mockBuildMetaProviderWrapper {
	t.Helper()
	return &mockBuildMetaProviderWrapper{
		MockProvider: testBuildProvider(t, prefix, buildCount),
		meta: map[string]providers.ResourceMeta{
			prefix + "_instance": {
				Role:        providers.RoleBuild,
				CachePolicy: providers.CachePolicy{Mode: providers.CacheByInputs},
			},
		},
	}
}

type mockBuildMetaProviderWrapper struct {
	*MockProvider
	meta map[string]providers.ResourceMeta
}

func (m *mockBuildMetaProviderWrapper) GetResourceMeta() map[string]providers.ResourceMeta {
	return m.meta
}

type testBuildHook struct {
	NilHook
	postApplyFn func(addrs.AbsResourceInstance, states.Generation, cty.Value, error) (HookAction, error)
}

func (h *testBuildHook) PostApply(addr addrs.AbsResourceInstance, gen states.Generation, newState cty.Value, err error) (HookAction, error) {
	if h.postApplyFn != nil {
		return h.postApplyFn(addr, gen, newState, err)
	}
	return HookActionContinue, nil
}
