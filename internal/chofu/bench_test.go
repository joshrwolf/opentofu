// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/instances"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// benchSchema is a minimal schema for benchmark vertices.
var benchSchema = map[addrs.Provider]providers.ProviderSchema{
	addrs.NewDefaultProvider("bench"): {
		Provider: providers.Schema{
			Block: &configschema.Block{},
		},
		ResourceTypes: map[string]providers.Schema{
			"bench_build": {
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"config": {Type: cty.String, Optional: true},
					},
				},
			},
			"bench_test": {
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"digest": {Type: cty.String, Optional: true},
					},
				},
			},
			"bench_tag": {
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"tags": {Type: cty.Map(cty.String), Optional: true},
					},
				},
			},
		},
		DataSources: map[string]providers.Schema{
			"bench_cache": {
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"key": {Type: cty.String, Optional: true},
					},
				},
			},
		},
	},
}

var benchProvider = addrs.NewDefaultProvider("bench")

// buildImageSubtree generates vertices for a single image module that
// mirrors the images-private pattern:
//
//	variable "target_repository"
//	local "config" = var.target_repository
//	local "needs_build" = local.config
//	data "bench_cache" "lookup" (depends on local.needs_build via config body)
//	resource "bench_build" "this" (depends on local.config, provider)
//	resource "bench_test" "this" (depends on bench_build.this, provider)
//	resource "bench_tag" "this" (depends on bench_test.this via depends_on, provider)
//	output "image_ref" = bench_build.this
//	output "test_status" = bench_test.this
//
// Total: 9 vertices per image, with a linear dependency chain plus
// provider edges and cross-references.
func buildImageSubtree(t testing.TB, idx int) []Vertex {
	mod := addrs.RootModuleInstance.Child(fmt.Sprintf("image%d", idx), addrs.NoKey)
	prefix := fmt.Sprintf("img%d", idx)

	buildAddr := addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "bench_build",
		Name: "this",
	}.Instance(addrs.NoKey).Absolute(mod)

	testAddr := addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "bench_test",
		Name: "this",
	}.Instance(addrs.NoKey).Absolute(mod)

	tagAddr := addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: "bench_tag",
		Name: "this",
	}.Instance(addrs.NoKey).Absolute(mod)

	cacheAddr := addrs.Resource{
		Mode: addrs.DataResourceMode,
		Type: "bench_cache",
		Name: "lookup",
	}.Instance(addrs.NoKey).Absolute(mod)

	return []Vertex{
		{Kind: KindVariable, Module: mod, Name: prefix + "/target_repository"},
		{Kind: KindLocal, Module: mod, Name: prefix + "/config",
			LocalExpr: testExprB(t, "var.target_repository")},
		{Kind: KindLocal, Module: mod, Name: prefix + "/needs_build",
			LocalExpr: testExprB(t, "local.config")},
		{Kind: KindDataSource, Module: mod, Name: prefix + "/bench_cache.lookup",
			ResourceAddr: cacheAddr, ProviderAddr: benchProvider,
			ResourceCfg: &configs.Resource{
				Mode: addrs.DataResourceMode, Type: "bench_cache", Name: "lookup",
				Config: testBodyB(t, `key = local.needs_build`),
			}},
		{Kind: KindResource, Module: mod, Name: prefix + "/bench_build.this",
			ResourceAddr: buildAddr, ProviderAddr: benchProvider,
			ResourceCfg: &configs.Resource{
				Mode: addrs.ManagedResourceMode, Type: "bench_build", Name: "this",
				Config: testBodyB(t, `config = local.config`),
			}},
		{Kind: KindResource, Module: mod, Name: prefix + "/bench_test.this",
			ResourceAddr: testAddr, ProviderAddr: benchProvider,
			ResourceCfg: &configs.Resource{
				Mode: addrs.ManagedResourceMode, Type: "bench_test", Name: "this",
				Config: testBodyB(t, `digest = bench_build.this.config`),
			}},
		{Kind: KindResource, Module: mod, Name: prefix + "/bench_tag.this",
			ResourceAddr: tagAddr, ProviderAddr: benchProvider,
			ResourceCfg: &configs.Resource{
				Mode: addrs.ManagedResourceMode, Type: "bench_tag", Name: "this",
			}},
		{Kind: KindOutput, Module: mod, Name: prefix + "/image_ref",
			OutputCfg: &configs.Output{Name: "image_ref",
				Expr: testExprB(t, "bench_build.this.config")}},
		{Kind: KindOutput, Module: mod, Name: prefix + "/test_status",
			OutputCfg: &configs.Output{Name: "test_status",
				Expr: testExprB(t, "bench_test.this.digest")}},
	}
}

// buildBenchGraph constructs a graph mimicking images-private:
// one provider config + n independent image subtrees.
func buildBenchGraph(t testing.TB, images int) []Vertex {
	// Root provider config vertex.
	verts := []Vertex{
		{
			Kind:         KindProviderConfig,
			Module:       addrs.RootModuleInstance,
			Name:         "bench",
			ProviderAddr: benchProvider,
		},
	}
	for i := range images {
		verts = append(verts, buildImageSubtree(t, i)...)
	}
	return verts
}

func BenchmarkBuildGraph(b *testing.B) {
	for _, images := range []int{100, 500, 1800} {
		b.Run(fmt.Sprintf("images=%d", images), func(b *testing.B) {
			verts := buildBenchGraph(b, images)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				g, _, diags := BuildGraph(verts, benchSchema)
				if diags.HasErrors() {
					b.Fatal(diags.Err())
				}
				_ = g
			}
		})
	}
}

func BenchmarkWalk(b *testing.B) {
	for _, images := range []int{100, 500, 1800} {
		// Build the graph once, reuse across iterations.
		verts := buildBenchGraph(b, images)
		g, _, diags := BuildGraph(verts, benchSchema)
		if diags.HasErrors() {
			b.Fatal(diags.Err())
		}

		for _, workers := range []int{1, runtime.GOMAXPROCS(0)} {
			name := fmt.Sprintf("images=%d/workers=%d", images, workers)
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					results := NewResults(len(g.Verts))
					walkDiags := Walk(b.Context(), g, results, workers, func(_ context.Context, id ID, v *Vertex) tfdiags.Diagnostics {
						results.Values[id] = cty.True
						return nil
					})
					if walkDiags.HasErrors() {
						b.Fatal(walkDiags.Err())
					}
				}
			})
		}
	}
}

func BenchmarkExpand(b *testing.B) {
	// Build a config tree that mirrors images-private: N independent child
	// modules at root, each with a variable, local chain, and a resource
	// with count driven by a local.
	childSrc := `
		variable "name" { default = "img" }
		locals {
			config = var.name
			needs_build = local.config != "" ? 1 : 0
		}
		resource "bench_build" "this" {
			count  = local.needs_build
			config = local.config
		}
		output "ref" { value = "done" }
	`

	for _, images := range []int{100, 500, 1800} {
		// Build config tree once per size.
		childMod := configs.ModuleFromStringForTesting(b, childSrc)

		var rootBuf strings.Builder
		children := make(map[string]*configs.Module, images)
		for i := range images {
			name := fmt.Sprintf("img%d", i)
			fmt.Fprintf(&rootBuf, "module %q { source = \"./%s\" }\n", name, name)
			children[name] = childMod
		}
		rootMod := configs.ModuleFromStringForTesting(b, rootBuf.String())
		cfg := testConfigB(b, rootMod, children)

		b.Run(fmt.Sprintf("images=%d", images), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				expander := instances.NewExpander()
				_, diags := Expand(b.Context(), cfg, benchSchema, expander, NewTargetFilter(nil), nil, "default", b.TempDir())
				if diags.HasErrors() {
					b.Fatal(diags.Err())
				}
			}
		})
	}
}

// BenchmarkWalkPipeline benchmarks the walk with a realistic images-private
// topology: N independent 3-vertex chains (build→test→tagger) rather than
// N fully independent vertices. This measures worker pool contention when
// parallelism is limited by short dependency chains.
func BenchmarkWalkPipeline(b *testing.B) {
	for _, images := range []int{100, 500, 1800} {
		g := pipelineGraph(images)

		for _, workers := range []int{1, runtime.GOMAXPROCS(0)} {
			name := fmt.Sprintf("images=%d/workers=%d", images, workers)
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					results := NewResults(len(g.Verts))
					diags := Walk(b.Context(), g, results, workers, func(_ context.Context, id ID, v *Vertex) tfdiags.Diagnostics {
						results.Values[id] = cty.True
						return nil
					})
					if diags.HasErrors() {
						b.Fatal(diags.Err())
					}
				}
			})
		}
	}
}

// pipelineGraph builds N independent 3-vertex chains (build→test→tagger),
// each depending on a shared provider vertex at index 0.
// Total vertices: 1 + N*3.
func pipelineGraph(n int) *Graph {
	total := 1 + n*3
	g := &Graph{
		Verts:   make([]Vertex, total),
		Deps:    make([][]ID, total),
		RevDeps: make([][]ID, total),
	}
	// Vertex 0: shared provider config.
	g.Verts[0] = Vertex{Kind: KindProviderConfig, Module: addrs.RootModuleInstance, Name: "provider"}

	for i := range n {
		base := 1 + i*3
		build := ID(base)
		test := ID(base + 1)
		tag := ID(base + 2)

		g.Verts[base] = Vertex{Kind: KindResource, Module: addrs.RootModuleInstance, Name: fmt.Sprintf("build%d", i)}
		g.Verts[base+1] = Vertex{Kind: KindResource, Module: addrs.RootModuleInstance, Name: fmt.Sprintf("test%d", i)}
		g.Verts[base+2] = Vertex{Kind: KindResource, Module: addrs.RootModuleInstance, Name: fmt.Sprintf("tag%d", i)}

		// build depends on provider
		g.Deps[build] = []ID{0}
		g.RevDeps[0] = append(g.RevDeps[0], build)
		// test depends on build
		g.Deps[test] = []ID{build}
		g.RevDeps[build] = append(g.RevDeps[build], test)
		// tag depends on test
		g.Deps[tag] = []ID{test}
		g.RevDeps[test] = append(g.RevDeps[test], tag)
	}
	return g
}

// testConfigB builds a configs.Config tree for benchmarks.
func testConfigB(tb testing.TB, root *configs.Module, children map[string]*configs.Module) *configs.Config {
	tb.Helper()
	cfg := &configs.Config{
		Module:   root,
		Children: make(map[string]*configs.Config, len(children)),
	}
	cfg.Root = cfg
	for name, childMod := range children {
		childCfg := &configs.Config{
			Module:   childMod,
			Root:     cfg,
			Parent:   cfg,
			Path:     addrs.Module{name},
			Children: map[string]*configs.Config{},
		}
		cfg.Children[name] = childCfg
	}
	return cfg
}

// testExprB is like testExpr but for benchmarks (uses testing.TB).
func testExprB(tb testing.TB, src string) hcl.Expression {
	tb.Helper()
	expr, diags := hclsyntax.ParseExpression([]byte(src), "bench.tf", hcl.InitialPos)
	if diags.HasErrors() {
		tb.Fatalf("parsing expression %q: %s", src, diags.Error())
	}
	return expr
}

// testBodyB is like testBody but for benchmarks.
func testBodyB(tb testing.TB, src string) hcl.Body {
	tb.Helper()
	f, diags := hclsyntax.ParseConfig([]byte(src), "bench.tf", hcl.InitialPos)
	if diags.HasErrors() {
		tb.Fatalf("parsing body %q: %s", src, diags.Error())
	}
	return f.Body
}
