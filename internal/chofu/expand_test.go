// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/instances"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// testConfig builds a configs.Config tree from a root module and optional
// child modules keyed by call name.
func testConfig(root *configs.Module, children map[string]*configs.Module) *configs.Config {
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

func TestExpand(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config   *configs.Config
		schemas  map[addrs.Provider]providers.ProviderSchema
		rootVars map[string]cty.Value
		filter   *TargetFilter

		wantVertsByKind map[VertexKind]int              // expected count per kind
		check           func(t *testing.T, verts []Vertex) // optional per-case vertex inspection
		wantErr         bool
	}{
		"empty module": {
			config:  testConfig(configs.ModuleFromStringForTesting(t, ""), nil),
			schemas: testSchema,
		},

		"root variables and locals": {
			config: testConfig(configs.ModuleFromStringForTesting(t, `
				variable "env" { default = "dev" }
				locals { name = "hello-${var.env}" }
				output "greeting" { value = local.name }
			`), nil),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1,
				KindLocal:    1,
				KindOutput:   1,
			},
		},

		"resource with provider": {
			config: testConfig(configs.ModuleFromStringForTesting(t, `
				provider "test" { region = "us-east-1" }
				resource "test_instance" "web" { ami = "abc" }
			`), nil),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindProviderConfig: 1,
				KindResource:       1,
			},
		},

		"resource with count": {
			config: testConfig(configs.ModuleFromStringForTesting(t, `
				resource "test_instance" "web" {
					count = 3
					ami   = "abc"
				}
			`), nil),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindResource: 3,
			},
		},

		"resource with for_each": {
			config: testConfig(configs.ModuleFromStringForTesting(t, `
				variable "images" {
					default = { "web" = "abc", "api" = "def" }
				}
				resource "test_instance" "this" {
					for_each = var.images
					ami      = each.value
				}
			`), nil),
			schemas:  testSchema,
			rootVars: map[string]cty.Value{},
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1,
				KindResource: 2,
			},
		},

		"child module expansion": {
			config: testConfig(
				configs.ModuleFromStringForTesting(t, `
					variable "env" { default = "prod" }
					module "child" {
						source = "./child"
						env    = var.env
					}
				`),
				map[string]*configs.Module{
					"child": configs.ModuleFromStringForTesting(t, `
						variable "env" {}
						output "result" { value = var.env }
					`),
				},
			),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 2, // root env + child env
				KindOutput:   1, // child result
			},
		},

		"multiple child modules expand in parallel": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					variable "x" { default = "ok" }
					output "y" { value = var.x }
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						module "a" { source = "./a" }
						module "b" { source = "./b" }
						module "c" { source = "./c" }
					`),
					map[string]*configs.Module{
						"a": childMod,
						"b": childMod,
						"c": childMod,
					},
				)
			}(),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 3, // one per child instance
				KindOutput:   3,
			},
		},

		"module for_each driven by local map": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					variable "name" {}
					output "image_ref" { value = "sha256:${var.name}" }
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						variable "configs" {
							default = {
								"web" = "config-web"
								"api" = "config-api"
							}
						}
						locals { locked = var.configs }
						module "build" {
							source   = "./build"
							for_each = local.locked
							name     = each.key
						}
					`),
					map[string]*configs.Module{"build": childMod},
				)
			}(),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 3, // root configs + child name × 2
				KindLocal:    1, // locked
				KindOutput:   2, // image_ref × 2 instances
			},
		},

		"module count driven by local boolean": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					output "result" { value = "built" }
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						variable "enabled" { default = true }
						locals { needs_build = var.enabled ? 1 : 0 }
						module "this" {
							source = "./build"
							count  = local.needs_build
						}
					`),
					map[string]*configs.Module{"this": childMod},
				)
			}(),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1,
				KindLocal:    1,
				KindOutput:   1, // count=1, one instance
			},
		},

		"module count zero produces no children": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					output "result" { value = "built" }
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						variable "enabled" { default = false }
						locals { needs_build = var.enabled ? 1 : 0 }
						module "this" {
							source = "./build"
							count  = local.needs_build
						}
					`),
					map[string]*configs.Module{"this": childMod},
				)
			}(),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1,
				KindLocal:    1,
				KindOutput:   0, // count=0, no instances
			},
		},

		// Pattern: resource count depends on a value that isn't known at
		// expansion time (e.g., a data source result or an unset variable).
		// images-private has: count = local.needs_build ? 1 : 0 where
		// needs_build depends on a data source. During expansion,
		// GetResource returns DynamicVal → count is unknown → evaluation
		// error. The expansion should fall back to a singleton instance
		// rather than producing zero vertices.
		"resource count depending on unknown falls back to singleton": {
			config: testConfig(configs.ModuleFromStringForTesting(t, `
				variable "count_val" {
					type = number
				}
				resource "test_instance" "conditional" {
					count = var.count_val
					ami   = "abc"
				}
			`), nil),
			schemas: testSchema,
			rootVars: map[string]cty.Value{
				"count_val": cty.UnknownVal(cty.Number),
			},
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1,
				KindResource: 1, // singleton fallback
			},
		},

		// Pattern: module call count depends on a value not known at
		// expansion time. The module call should fall back to singleton
		// and still expand its children.
		"module count depending on unknown falls back to singleton": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					variable "x" { default = "ok" }
					output "y" { value = var.x }
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						variable "enabled" { type = number }
						module "conditional" {
							source = "./child"
							count  = var.enabled
						}
					`),
					map[string]*configs.Module{"conditional": childMod},
				)
			}(),
			schemas: testSchema,
			rootVars: map[string]cty.Value{
				"enabled": cty.UnknownVal(cty.Number),
			},
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 2, // root enabled + child x
				KindOutput:   1, // child y — children are expanded
			},
			check: func(t *testing.T, verts []Vertex) {
				t.Helper()
				for _, v := range verts {
					if v.Kind == KindVariable && !v.Module.IsRoot() && v.VariableVal == cty.NilVal {
						t.Errorf("fallback child variable %q has NilVal VariableVal", v.Name)
					}
				}
			},
		},

		// Same as above but for for_each.
		"module for_each depending on unknown falls back to singleton": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					variable "x" { default = "ok" }
					output "y" { value = var.x }
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						variable "items" { type = map(string) }
						module "conditional" {
							source   = "./child"
							for_each = var.items
						}
					`),
					map[string]*configs.Module{"conditional": childMod},
				)
			}(),
			schemas: testSchema,
			rootVars: map[string]cty.Value{
				"items": cty.UnknownVal(cty.Map(cty.String)),
			},
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 2, // root items + child x
				KindOutput:   1, // child y — children are expanded
			},
		},

		"data source expansion": {
			config: testConfig(configs.ModuleFromStringForTesting(t, `
				data "test_ami" "latest" { filter = "ubuntu" }
			`), nil),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindDataSource: 1,
			},
		},

		"target filter prunes unrelated modules during expansion": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					variable "x" { default = "ok" }
					output "y" { value = var.x }
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						module "a" { source = "./a" }
						module "b" { source = "./b" }
						module "c" { source = "./c" }
					`),
					map[string]*configs.Module{
						"a": childMod,
						"b": childMod,
						"c": childMod,
					},
				)
			}(),
			schemas: testSchema,
			filter:  NewTargetFilter([]addrs.Targetable{addrs.Module{"b"}}),
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1, // only module.b's variable
				KindOutput:   1, // only module.b's output
			},
		},

		// Verify that child module variable vertices carry the value
		// resolved from the parent module call, not just the config default.
		// The walk uses VariableVal instead of falling back to the default.
		"child variable vertices carry parent-passed values": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					variable "env" { default = "fallback" }
					output "result" { value = var.env }
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						variable "env" { default = "prod" }
						module "child" {
							source = "./child"
							env    = var.env
						}
					`),
					map[string]*configs.Module{"child": childMod},
				)
			}(),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 2,
				KindOutput:   1,
			},
			check: func(t *testing.T, verts []Vertex) {
				t.Helper()
				for _, v := range verts {
					if v.Kind != KindVariable || v.Module.IsRoot() {
						continue
					}
					// Child variable vertices must have VariableVal set from the
					// parent module call. Root variables are different — they come
					// from CLI/tfvars and use the default in the walk.
					if v.VariableVal == cty.NilVal {
						t.Errorf("child variable %q in %s has NilVal VariableVal", v.Name, v.Module)
					}
				}
			},
		},

		// Regression: each.key in module call arguments must resolve to the
		// child instance's for_each key, not panic or fall back to defaults.
		"module call arguments resolve each.key per instance": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					variable "key" {}
					output "result" { value = var.key }
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						variable "items" {
							default = { "a" = "1", "b" = "2" }
						}
						module "child" {
							source   = "./child"
							for_each = var.items
							key      = each.key
						}
					`),
					map[string]*configs.Module{"child": childMod},
				)
			}(),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1 + 2, // root items + child key × 2
				KindOutput:   2,     // child result × 2
			},
			check: func(t *testing.T, verts []Vertex) {
				t.Helper()
				var keys []string
				for _, v := range verts {
					if v.Kind == KindVariable && v.Name == "key" && !v.Module.IsRoot() {
						if v.VariableVal == cty.NilVal {
							t.Error("child variable 'key' has NilVal — each.key was not resolved")
						} else if v.VariableVal.IsKnown() {
							keys = append(keys, v.VariableVal.AsString())
						}
					}
				}
				if len(keys) != 2 {
					t.Errorf("expected 2 resolved keys, got %d: %v", len(keys), keys)
				}
			},
		},

		// Regression: optional() attributes in variable type constraints must
		// be filled in as null after type conversion, not left absent.
		"module call arguments apply optional type conversion": {
			config: func() *configs.Config {
				childMod := configs.ModuleFromStringForTesting(t, `
					variable "cfg" {
						type = object({
							name = string
							tag  = optional(string)
						})
					}
					output "tag" {
						value = var.cfg.tag != null ? var.cfg.tag : "latest"
					}
				`)
				return testConfig(
					configs.ModuleFromStringForTesting(t, `
						module "child" {
							source = "./child"
							cfg    = { name = "nginx" }
						}
					`),
					map[string]*configs.Module{"child": childMod},
				)
			}(),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1,
				KindOutput:   1,
			},
			check: func(t *testing.T, verts []Vertex) {
				t.Helper()
				for _, v := range verts {
					if v.Kind != KindVariable || v.Module.IsRoot() {
						continue
					}
					if v.VariableVal == cty.NilVal {
						t.Fatal("child variable 'cfg' has NilVal")
					}
					// After type conversion, the object must have both
					// attributes — "tag" should be null, not absent.
					ty := v.VariableVal.Type()
					if !ty.HasAttribute("tag") {
						t.Error("optional attribute 'tag' missing after type conversion")
					}
				}
			},
		},

		// Regression test: parallel sibling module calls where a parent has
		// for_each must not panic. Before the fix, ExpandModule walked ALL
		// ancestor instances — including siblings that hadn't registered
		// their child calls yet.
		"parallel for_each siblings with shared child calls": {
			config: func() *configs.Config {
				grandchild := configs.ModuleFromStringForTesting(t, `
					variable "name" {}
					output "result" { value = var.name }
				`)
				child := configs.ModuleFromStringForTesting(t, `
					variable "key" {}
					module "inner" {
						source = "./inner"
						name   = var.key
					}
				`)
				root := configs.ModuleFromStringForTesting(t, `
					variable "items" {
						default = { "a" = "1", "b" = "2", "c" = "3" }
					}
					module "build" {
						source   = "./build"
						for_each = var.items
						key      = each.key
					}
					module "test" {
						source   = "./build"
						for_each = var.items
						key      = each.key
					}
				`)
				cfg := &configs.Config{
					Module:   root,
					Children: map[string]*configs.Config{},
				}
				cfg.Root = cfg
				for _, name := range []string{"build", "test"} {
					childCfg := &configs.Config{
						Module:   child,
						Root:     cfg,
						Parent:   cfg,
						Path:     addrs.Module{name},
						Children: map[string]*configs.Config{},
					}
					gcCfg := &configs.Config{
						Module:   grandchild,
						Root:     cfg,
						Parent:   childCfg,
						Path:     addrs.Module{name, "inner"},
						Children: map[string]*configs.Config{},
					}
					childCfg.Children["inner"] = gcCfg
					cfg.Children[name] = childCfg
				}
				return cfg
			}(),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1 + 3 + 3 + 3 + 3, // root items + build×3 key + test×3 key + inner×3 name + inner×3 name
				KindOutput:   6,                   // inner result × 3 build instances + 3 test instances
			},
		},

		"deep local chain evaluates during expansion": {
			config: testConfig(configs.ModuleFromStringForTesting(t, `
				variable "input" { default = "val" }
				locals {
					a = var.input
					b = local.a
					c = local.b
				}
				resource "test_instance" "web" {
					count = local.c == "val" ? 1 : 0
					ami   = local.c
				}
			`), nil),
			schemas: testSchema,
			wantVertsByKind: map[VertexKind]int{
				KindVariable: 1,
				KindLocal:    3,
				KindResource: 1, // count resolved to 1
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			expander := instances.NewExpander()
			rootVars := tc.rootVars
			if rootVars == nil {
				rootVars = map[string]cty.Value{}
			}
			filter := tc.filter
			if filter == nil {
				filter = NewTargetFilter(nil)
			}

			verts, diags := Expand(
				t.Context(),
				tc.config,
				tc.schemas,
				expander,
				filter,
				rootVars,
				"default",
				t.TempDir(),
			)

			if tc.wantErr {
				if !diags.HasErrors() {
					t.Fatal("expected errors, got none")
				}
				return
			}
			// Expansion errors are non-fatal, so just log them.
			for _, d := range diags {
				if d.Severity() == tfdiags.Error {
					t.Logf("expansion diagnostic: %s", d.Description().Summary)
				}
			}

			// Count vertices by kind.
			gotCounts := make(map[VertexKind]int)
			for _, v := range verts {
				gotCounts[v.Kind]++
			}

			for kind, want := range tc.wantVertsByKind {
				got := gotCounts[kind]
				if got != want {
					t.Errorf("kind %d: got %d vertices, want %d", kind, got, want)
				}
			}

			// Verify all vertices have Name set.
			for i, v := range verts {
				if v.Name == "" {
					t.Errorf("vertex %d (kind=%d) has empty Name", i, v.Kind)
				}
			}

			// Verify resource/data vertices have ResourceAddr set.
			for i, v := range verts {
				if v.Kind == KindResource || v.Kind == KindDataSource {
					if v.ResourceAddr.Resource.Resource.Type == "" {
						t.Errorf("vertex %d (%s) has empty ResourceAddr", i, v.Name)
					}
				}
			}

			if tc.check != nil {
				tc.check(t, verts)
			}
		})
	}
}
