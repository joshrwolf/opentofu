// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"slices"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/providers"
)

var (
	testProvider = addrs.NewDefaultProvider("test")

	testSchema = map[addrs.Provider]providers.ProviderSchema{
		testProvider: {
			Provider: providers.Schema{
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"region": {Type: cty.String, Optional: true},
					},
				},
			},
			ResourceTypes: map[string]providers.Schema{
				"test_instance": {
					Block: &configschema.Block{
						Attributes: map[string]*configschema.Attribute{
							"ami":  {Type: cty.String, Required: true},
							"name": {Type: cty.String, Optional: true},
						},
					},
				},
				"test_network": {
					Block: &configschema.Block{
						Attributes: map[string]*configschema.Attribute{
							"cidr": {Type: cty.String, Required: true},
						},
					},
				},
			},
			DataSources: map[string]providers.Schema{
				"test_ami": {
					Block: &configschema.Block{
						Attributes: map[string]*configschema.Attribute{
							"filter": {Type: cty.String, Optional: true},
						},
					},
				},
			},
		},
	}
)

// testExpr parses a simple HCL expression for use in test vertices.
func testExpr(t *testing.T, src string) hcl.Expression {
	t.Helper()
	expr, diags := hclsyntax.ParseExpression([]byte(src), "test.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parsing expression %q: %s", src, diags.Error())
	}
	return expr
}

// resAddr builds an AbsResourceInstance for a managed resource at root module.
func resAddr(typ, name string, key addrs.InstanceKey) addrs.AbsResourceInstance {
	return addrs.Resource{
		Mode: addrs.ManagedResourceMode,
		Type: typ,
		Name: name,
	}.Instance(key).Absolute(addrs.RootModuleInstance)
}

// dataAddr builds an AbsResourceInstance for a data source at root module.
func dataAddr(typ, name string, key addrs.InstanceKey) addrs.AbsResourceInstance {
	return addrs.Resource{
		Mode: addrs.DataResourceMode,
		Type: typ,
		Name: name,
	}.Instance(key).Absolute(addrs.RootModuleInstance)
}

func TestBuildGraph(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		verts   []Vertex
		schemas map[addrs.Provider]providers.ProviderSchema

		wantVars      int
		wantLocals    int
		wantOutputs   int
		wantProviders int
		wantResInst   int // number of keys in ResInst
		wantEdges     int // total edge count
		wantCycleErr  bool

		// checkDeps verifies specific dependency relationships.
		checkDeps []depCheck

		// targets, if set, runs FilterTargets after BuildGraph and
		// asserts the filtered graph has exactly wantFiltered vertices.
		targets      []addrs.Targetable
		wantFiltered int
	}{
		"empty graph": {
			verts:   nil,
			schemas: testSchema,
		},

		"variables only": {
			verts: []Vertex{
				{Kind: KindVariable, Module: addrs.RootModuleInstance, Name: "region"},
				{Kind: KindVariable, Module: addrs.RootModuleInstance, Name: "env"},
			},
			schemas:  testSchema,
			wantVars: 2,
		},

		"local depends on variable": {
			verts: []Vertex{
				{Kind: KindVariable, Module: addrs.RootModuleInstance, Name: "env"},
				{
					Kind:      KindLocal,
					Module:    addrs.RootModuleInstance,
					Name:      "name",
					LocalExpr: testExpr(t, "var.env"),
				},
			},
			schemas:    testSchema,
			wantVars:   1,
			wantLocals: 1,
			wantEdges:  1,
			checkDeps: []depCheck{
				{from: "name", dependsOn: "env"},
			},
		},

		"output depends on local": {
			verts: []Vertex{
				{
					Kind:      KindLocal,
					Module:    addrs.RootModuleInstance,
					Name:      "greeting",
					LocalExpr: testExpr(t, `"hello"`),
				},
				{
					Kind:   KindOutput,
					Module: addrs.RootModuleInstance,
					Name:   "out",
					OutputCfg: &configs.Output{
						Name: "out",
						Expr: testExpr(t, "local.greeting"),
					},
				},
			},
			schemas:     testSchema,
			wantLocals:  1,
			wantOutputs: 1,
			wantEdges:   1,
			checkDeps: []depCheck{
				{from: "out", dependsOn: "greeting"},
			},
		},

		"resource depends on provider config": {
			verts: []Vertex{
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
					ProviderBody: nil, // no config body
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
					},
				},
			},
			schemas:       testSchema,
			wantProviders: 1,
			wantResInst:   1,
			wantEdges:     1, // resource → provider
			checkDeps: []depCheck{
				{from: "test_instance.web", dependsOn: "test"},
			},
		},

		"resource with HCL reference to another resource": {
			verts: []Vertex{
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
					ProviderBody: nil,
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_network.vpc",
					ResourceAddr: resAddr("test_network", "vpc", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode:   addrs.ManagedResourceMode,
						Type:   "test_network",
						Name:   "vpc",
						Config: testBody(t, `cidr = "10.0.0.0/16"`),
					},
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode:   addrs.ManagedResourceMode,
						Type:   "test_instance",
						Name:   "web",
						Config: testBody(t, `ami = test_network.vpc.id`),
					},
				},
			},
			schemas:       testSchema,
			wantProviders: 1,
			wantResInst:   2,
			wantEdges:     3, // vpc→provider, web→provider, web→vpc
			checkDeps: []depCheck{
				{from: "test_instance.web", dependsOn: "test_network.vpc"},
				{from: "test_instance.web", dependsOn: "test"},
				{from: "test_network.vpc", dependsOn: "test"},
			},
		},

		"data source gets schema attached": {
			verts: []Vertex{
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
					ProviderBody: nil,
				},
				{
					Kind:         KindDataSource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_ami.latest",
					ResourceAddr: dataAddr("test_ami", "latest", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.DataResourceMode,
						Type: "test_ami",
						Name: "latest",
					},
				},
			},
			schemas:       testSchema,
			wantProviders: 1,
			wantResInst:   1,
			wantEdges:     1, // data→provider
		},

		"cycle detection": {
			verts: []Vertex{
				{
					Kind:      KindLocal,
					Module:    addrs.RootModuleInstance,
					Name:      "a",
					LocalExpr: testExpr(t, "local.b"),
				},
				{
					Kind:      KindLocal,
					Module:    addrs.RootModuleInstance,
					Name:      "b",
					LocalExpr: testExpr(t, "local.a"),
				},
			},
			schemas:      testSchema,
			wantLocals:   2,
			wantEdges:    2,
			wantCycleErr: true,
		},

		"schema attachment populates block": {
			verts: []Vertex{
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
					},
				},
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
				},
			},
			schemas:       testSchema,
			wantProviders: 1,
			wantResInst:   1,
			wantEdges:     1,
		},

		"resource without schema gets only provider edge": {
			verts: []Vertex{
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "unknown",
					ProviderAddr: addrs.NewDefaultProvider("unknown"),
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "unknown_thing.foo",
					ResourceAddr: resAddr("unknown_thing", "foo", addrs.NoKey),
					ProviderAddr: addrs.NewDefaultProvider("unknown"),
					ResourceCfg: &configs.Resource{
						Mode:   addrs.ManagedResourceMode,
						Type:   "unknown_thing",
						Name:   "foo",
						Config: testBody(t, `name = "hi"`),
					},
				},
			},
			// No schema for "unknown" provider — resource should get zero
			// HCL reference edges but still get the provider config edge.
			schemas:       testSchema,
			wantProviders: 1,
			wantResInst:   1,
			wantEdges:     1, // only provider edge, no ref edges
			checkDeps: []depCheck{
				{from: "unknown_thing.foo", dependsOn: "unknown"},
			},
		},

		"depends_on creates edge": {
			verts: []Vertex{
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_network.vpc",
					ResourceAddr: resAddr("test_network", "vpc", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_network",
						Name: "vpc",
					},
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
						DependsOn: []hcl.Traversal{
							{
								hcl.TraverseRoot{Name: "test_network"},
								hcl.TraverseAttr{Name: "vpc"},
							},
						},
					},
				},
			},
			schemas:       testSchema,
			wantProviders: 1,
			wantResInst:   2,
			wantEdges:     3, // web→vpc (depends_on), web→provider, vpc→provider
			checkDeps: []depCheck{
				{from: "test_instance.web", dependsOn: "test_network.vpc"},
				{from: "test_instance.web", dependsOn: "test"},
			},
		},

		"provider config depends on variable via body": {
			verts: []Vertex{
				{Kind: KindVariable, Module: addrs.RootModuleInstance, Name: "region"},
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
					ProviderBody: testBody(t, `region = var.region`),
				},
			},
			schemas:       testSchema,
			wantVars:      1,
			wantProviders: 1,
			wantEdges:     1, // provider→variable
			checkDeps: []depCheck{
				{from: "test", dependsOn: "region"},
			},
		},

		"child module output vertices": {
			verts: func() []Vertex {
				child := addrs.RootModuleInstance.Child("network", addrs.NoKey)
				return []Vertex{
					{
						Kind:   KindOutput,
						Module: child,
						Name:   "vpc_id",
						OutputCfg: &configs.Output{
							Name: "vpc_id",
							Expr: testExpr(t, `"vpc-123"`),
						},
					},
					{
						Kind:      KindLocal,
						Module:    addrs.RootModuleInstance,
						Name:      "net_id",
						LocalExpr: testExpr(t, "module.network.vpc_id"),
					},
				}
			}(),
			schemas:     testSchema,
			wantOutputs: 1,
			wantLocals:  1,
			wantEdges:   1, // local→child output
			checkDeps: []depCheck{
				{from: "net_id", dependsOn: "vpc_id"},
			},
		},

		"aliased provider lookup": {
			verts: []Vertex{
				{
					Kind:          KindProviderConfig,
					Module:        addrs.RootModuleInstance,
					Name:          "test.alt",
					ProviderAddr:  testProvider,
					ProviderAlias: "alt",
				},
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
					},
				},
			},
			schemas:       testSchema,
			wantProviders: 2,
			wantResInst:   1,
			wantEdges:     1, // web→default provider (not aliased)
			checkDeps: []depCheck{
				{from: "test_instance.web", dependsOn: "test"},
			},
		},

		"for_each string key instances grouped": {
			verts: []Vertex{
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.StringKey("a")),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
					},
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.StringKey("b")),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
					},
				},
			},
			schemas:     testSchema,
			wantResInst: 1, // one key, two IDs
		},

		"multiple instances of same resource": {
			verts: []Vertex{
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.IntKey(0)),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
					},
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.IntKey(1)),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
					},
				},
			},
			schemas:     testSchema,
			wantResInst: 1, // one key, two IDs
		},

		// Pattern: provider::oci::parse(var.digest) in a local.
		// The local should depend on var.digest but NOT create an edge
		// for the provider function reference.
		"local with provider function ignores function dep": {
			verts: []Vertex{
				{Kind: KindVariable, Module: addrs.RootModuleInstance, Name: "digest"},
				{
					Kind:   KindLocal,
					Module: addrs.RootModuleInstance,
					Name:   "parsed",
					// provider::oci::parse(var.digest) — the parser extracts
					// both a ProviderFunction ref and an InputVariable ref.
					// We can only test the variable part since provider function
					// traversals need a real provider namespace prefix. But we
					// can test that var.digest is resolved and the local gets
					// exactly one edge.
					LocalExpr: testExpr(t, "var.digest"),
				},
			},
			schemas:    testSchema,
			wantVars:   1,
			wantLocals: 1,
			wantEdges:  1,
			checkDeps: []depCheck{
				{from: "parsed", dependsOn: "digest"},
			},
		},

		// Pattern: chain of 5 locals (a→b→c→d→e).
		// Mirrors images-private's 4-7 deep local chains like:
		// target_repository → parts → group → metadata → readme_content
		"deep local chain": {
			verts: []Vertex{
				{Kind: KindVariable, Module: addrs.RootModuleInstance, Name: "input"},
				{Kind: KindLocal, Module: addrs.RootModuleInstance, Name: "a", LocalExpr: testExpr(t, "var.input")},
				{Kind: KindLocal, Module: addrs.RootModuleInstance, Name: "b", LocalExpr: testExpr(t, "local.a")},
				{Kind: KindLocal, Module: addrs.RootModuleInstance, Name: "c", LocalExpr: testExpr(t, "local.b")},
				{Kind: KindLocal, Module: addrs.RootModuleInstance, Name: "d", LocalExpr: testExpr(t, "local.c")},
				{Kind: KindLocal, Module: addrs.RootModuleInstance, Name: "e", LocalExpr: testExpr(t, "local.d")},
			},
			schemas:    testSchema,
			wantVars:   1,
			wantLocals: 5,
			wantEdges:  5, // input←a←b←c←d←e
			checkDeps: []depCheck{
				{from: "a", dependsOn: "input"},
				{from: "b", dependsOn: "a"},
				{from: "c", dependsOn: "b"},
				{from: "d", dependsOn: "c"},
				{from: "e", dependsOn: "d"},
			},
		},

		// Pattern: module.build["airflow-2"] outputs referenced by root local.
		// Tests cross-module output resolution with for_each-expanded child
		// module instances. module.build resolves to all outputs of child
		// "build" instances.
		"for_each module output cross-reference": {
			verts: func() []Vertex {
				buildV1 := addrs.RootModuleInstance.Child("build", addrs.StringKey("v1"))
				testV1 := addrs.RootModuleInstance.Child("test", addrs.StringKey("v1"))
				return []Vertex{
					{
						Kind:   KindOutput,
						Module: buildV1,
						Name:   "image_ref",
						OutputCfg: &configs.Output{
							Name: "image_ref",
							Expr: testExpr(t, `"sha256:abc"`),
						},
					},
					{
						Kind:   KindOutput,
						Module: testV1,
						Name:   "result",
						OutputCfg: &configs.Output{
							Name: "result",
							Expr: testExpr(t, `"pass"`),
						},
					},
					{
						Kind:      KindLocal,
						Module:    addrs.RootModuleInstance,
						Name:      "all_images",
						LocalExpr: testExpr(t, "module.build"),
					},
				}
			}(),
			schemas:     testSchema,
			wantOutputs: 2,
			wantLocals:  1,
			wantEdges:   1, // all_images → build["v1"].image_ref
			checkDeps: []depCheck{
				{from: "all_images", dependsOn: "image_ref"},
			},
		},

		// Pattern: depends_on = [module.test] where module.test has
		// for_each-expanded instances. The resource should depend on
		// all output vertices of all instances of that module.
		// FilterTargets: target a single resource, expect it + its
		// provider dependency to survive.
		"filter keeps resource and its provider": {
			verts: []Vertex{
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
					},
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_network.vpc",
					ResourceAddr: resAddr("test_network", "vpc", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_network",
						Name: "vpc",
					},
				},
			},
			schemas: testSchema,
			targets: []addrs.Targetable{
				addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "test_instance",
					Name: "web",
				}.Absolute(addrs.RootModuleInstance),
			},
			wantFiltered: 2, // web + provider (vpc pruned)
		},

		// FilterTargets: target a resource that depends on another resource,
		// expect both + provider to survive (transitive dep inclusion).
		"filter includes transitive dependencies": {
			verts: []Vertex{
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_network.vpc",
					ResourceAddr: resAddr("test_network", "vpc", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode:   addrs.ManagedResourceMode,
						Type:   "test_network",
						Name:   "vpc",
						Config: testBody(t, `cidr = "10.0.0.0/16"`),
					},
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode:   addrs.ManagedResourceMode,
						Type:   "test_instance",
						Name:   "web",
						Config: testBody(t, `ami = test_network.vpc.id`),
					},
				},
			},
			schemas: testSchema,
			targets: []addrs.Targetable{
				addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "test_instance",
					Name: "web",
				}.Absolute(addrs.RootModuleInstance),
			},
			wantFiltered: 3, // web + vpc (transitive dep) + provider
		},

		// FilterTargets: target a child module, expect all its contents
		// plus root provider to survive.
		"filter keeps child module contents": {
			verts: func() []Vertex {
				child := addrs.RootModuleInstance.Child("build", addrs.NoKey)
				return []Vertex{
					{
						Kind:         KindProviderConfig,
						Module:       addrs.RootModuleInstance,
						Name:         "test",
						ProviderAddr: testProvider,
					},
					{Kind: KindVariable, Module: child, Name: "name"},
					{
						Kind:   KindOutput,
						Module: child,
						Name:   "ref",
						OutputCfg: &configs.Output{
							Name: "ref",
							Expr: testExpr(t, "var.name"),
						},
					},
					// Unrelated root resource — should be pruned.
					{
						Kind:         KindResource,
						Module:       addrs.RootModuleInstance,
						Name:         "test_instance.other",
						ResourceAddr: resAddr("test_instance", "other", addrs.NoKey),
						ProviderAddr: testProvider,
						ResourceCfg: &configs.Resource{
							Mode: addrs.ManagedResourceMode,
							Type: "test_instance",
							Name: "other",
						},
					},
				}
			}(),
			schemas: testSchema,
			targets: []addrs.Targetable{
				addrs.Module{"build"},
			},
			wantFiltered: 3, // provider + child var + child output (root resource pruned)
		},

		// FilterTargets: target a ModuleInstance (with key) — generalizeTarget
		// should convert to Module for config-level matching.
		"filter with module instance key": {
			verts: func() []Vertex {
				buildV1 := addrs.RootModuleInstance.Child("build", addrs.StringKey("v1"))
				buildV2 := addrs.RootModuleInstance.Child("build", addrs.StringKey("v2"))
				return []Vertex{
					{
						Kind:         KindProviderConfig,
						Module:       addrs.RootModuleInstance,
						Name:         "test",
						ProviderAddr: testProvider,
					},
					{Kind: KindVariable, Module: buildV1, Name: "name"},
					{
						Kind:   KindOutput,
						Module: buildV1,
						Name:   "ref",
						OutputCfg: &configs.Output{
							Name: "ref",
							Expr: testExpr(t, "var.name"),
						},
					},
					{Kind: KindVariable, Module: buildV2, Name: "name"},
					{
						Kind:   KindOutput,
						Module: buildV2,
						Name:   "ref",
						OutputCfg: &configs.Output{
							Name: "ref",
							Expr: testExpr(t, "var.name"),
						},
					},
				}
			}(),
			schemas: testSchema,
			// Target module.build["v1"] — both instances should be kept
			// because generalizeTarget drops the key, targeting module.build.
			targets: []addrs.Targetable{
				addrs.RootModuleInstance.Child("build", addrs.StringKey("v1")),
			},
			wantFiltered: 5, // provider + 2 vars + 2 outputs (both instances)
		},

		// FilterTargets: target a resource inside a child module.
		"filter targets resource in child module": {
			verts: func() []Vertex {
				child := addrs.RootModuleInstance.Child("app", addrs.NoKey)
				return []Vertex{
					{
						Kind:         KindProviderConfig,
						Module:       addrs.RootModuleInstance,
						Name:         "test",
						ProviderAddr: testProvider,
					},
					{Kind: KindVariable, Module: child, Name: "ami"},
					{
						Kind:         KindResource,
						Module:       child,
						Name:         "test_instance.web",
						ResourceAddr: addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"}.Instance(addrs.NoKey).Absolute(child),
						ProviderAddr: testProvider,
						ResourceCfg: &configs.Resource{
							Mode:   addrs.ManagedResourceMode,
							Type:   "test_instance",
							Name:   "web",
							Config: testBody(t, `ami = var.ami`),
						},
					},
					// Unrelated resource in same child — should be pruned.
					{
						Kind:         KindResource,
						Module:       child,
						Name:         "test_network.vpc",
						ResourceAddr: addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_network", Name: "vpc"}.Instance(addrs.NoKey).Absolute(child),
						ProviderAddr: testProvider,
						ResourceCfg: &configs.Resource{
							Mode: addrs.ManagedResourceMode,
							Type: "test_network",
							Name: "vpc",
						},
					},
				}
			}(),
			schemas: testSchema,
			targets: []addrs.Targetable{
				addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"}.Absolute(
					addrs.RootModuleInstance.Child("app", addrs.NoKey),
				),
			},
			wantFiltered: 3, // provider + var.ami (transitive dep) + test_instance.web
		},

		// FilterTargets: empty filter returns the graph unchanged.
		"filter inactive passthrough": {
			verts: []Vertex{
				{Kind: KindVariable, Module: addrs.RootModuleInstance, Name: "x"},
				{Kind: KindVariable, Module: addrs.RootModuleInstance, Name: "y"},
			},
			schemas:      testSchema,
			targets:      []addrs.Targetable{}, // empty = inactive
			wantFiltered: 2,                     // passthrough
			wantVars:     2,
		},

		// FilterTargets: target that matches nothing keeps only provider configs.
		"filter with no matching target": {
			verts: []Vertex{
				{
					Kind:         KindProviderConfig,
					Module:       addrs.RootModuleInstance,
					Name:         "test",
					ProviderAddr: testProvider,
				},
				{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         "test_instance.web",
					ResourceAddr: resAddr("test_instance", "web", addrs.NoKey),
					ProviderAddr: testProvider,
					ResourceCfg: &configs.Resource{
						Mode: addrs.ManagedResourceMode,
						Type: "test_instance",
						Name: "web",
					},
				},
			},
			schemas: testSchema,
			targets: []addrs.Targetable{
				addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "nonexistent"}.Absolute(addrs.RootModuleInstance),
			},
			wantFiltered: 1, // only provider config survives
		},

		// Pattern: root module outputs that aggregate child modules.
		// images-private has output "summary_X" { value = module.X.summary }
		// for each of 1,881 images. When targeting module.a, root outputs
		// referencing module.b must be excluded — they are NOT part of the
		// target, even though the root module "contains" the target.
		"filter excludes root outputs referencing non-targeted modules": {
			verts: func() []Vertex {
				childA := addrs.RootModuleInstance.Child("a", addrs.NoKey)
				childB := addrs.RootModuleInstance.Child("b", addrs.NoKey)
				return []Vertex{
					{
						Kind:         KindProviderConfig,
						Module:       addrs.RootModuleInstance,
						Name:         "test",
						ProviderAddr: testProvider,
					},
					// Child module a
					{Kind: KindVariable, Module: childA, Name: "x"},
					{
						Kind:   KindOutput,
						Module: childA,
						Name:   "out",
						OutputCfg: &configs.Output{
							Name: "out",
							Expr: testExpr(t, "var.x"),
						},
					},
					// Child module b
					{Kind: KindVariable, Module: childB, Name: "x"},
					{
						Kind:   KindOutput,
						Module: childB,
						Name:   "out",
						OutputCfg: &configs.Output{
							Name: "out",
							Expr: testExpr(t, "var.x"),
						},
					},
					// Root outputs referencing child modules — these should be
					// excluded when targeting module.a because the root module
					// is broader than the target.
					{
						Kind:   KindOutput,
						Module: addrs.RootModuleInstance,
						Name:   "summary_a",
						OutputCfg: &configs.Output{
							Name: "summary_a",
							Expr: testExpr(t, "module.a"),
						},
					},
					{
						Kind:   KindOutput,
						Module: addrs.RootModuleInstance,
						Name:   "summary_b",
						OutputCfg: &configs.Output{
							Name: "summary_b",
							Expr: testExpr(t, "module.b"),
						},
					},
				}
			}(),
			schemas: testSchema,
			targets: []addrs.Targetable{
				addrs.Module{"a"},
			},
			// Provider + child a's var + child a's output = 3.
			// Root outputs and child b are excluded.
			wantFiltered: 3,
		},

		"depends_on targets for_each module": {
			verts: func() []Vertex {
				testV1 := addrs.RootModuleInstance.Child("test", addrs.StringKey("v1"))
				testV2 := addrs.RootModuleInstance.Child("test", addrs.StringKey("v2"))
				return []Vertex{
					{
						Kind:   KindOutput,
						Module: testV1,
						Name:   "status",
						OutputCfg: &configs.Output{
							Name: "status",
							Expr: testExpr(t, `"pass"`),
						},
					},
					{
						Kind:   KindOutput,
						Module: testV2,
						Name:   "status",
						OutputCfg: &configs.Output{
							Name: "status",
							Expr: testExpr(t, `"pass"`),
						},
					},
					{
						Kind:         KindProviderConfig,
						Module:       addrs.RootModuleInstance,
						Name:         "test",
						ProviderAddr: testProvider,
					},
					{
						Kind:         KindResource,
						Module:       addrs.RootModuleInstance,
						Name:         "test_instance.tagger",
						ResourceAddr: resAddr("test_instance", "tagger", addrs.NoKey),
						ProviderAddr: testProvider,
						ResourceCfg: &configs.Resource{
							Mode: addrs.ManagedResourceMode,
							Type: "test_instance",
							Name: "tagger",
							DependsOn: []hcl.Traversal{
								{
									hcl.TraverseRoot{Name: "module"},
									hcl.TraverseAttr{Name: "test"},
								},
							},
						},
					},
				}
			}(),
			schemas:       testSchema,
			wantOutputs:   2,
			wantProviders: 1,
			wantResInst:   1,
			wantEdges:     3, // tagger→provider, tagger→test["v1"].status, tagger→test["v2"].status
			checkDeps: []depCheck{
				{from: "test_instance.tagger", dependsOn: "test"},
			},
		},

		// Module expand/close gate vertices enforce ordering.
		// Expand gates variables (which read re-evaluated module call args).
		// Close gates externally-observable content (outputs, resources, data).
		// Other child content (locals) reaches expand transitively through
		// variable → local → output HCL ref edges.
		"module expand gates variables, close gates outputs": {
			verts: func() []Vertex {
				childModule := addrs.RootModuleInstance.Child("tagger", addrs.NoKey)
				return []Vertex{
					{
						Kind:   KindResource,
						Module: addrs.RootModuleInstance,
						Name:   "test_instance.build",
						ResourceCfg: &configs.Resource{
							Mode: addrs.ManagedResourceMode,
							Type: "test_instance",
							Name: "build",
							Config: testBody(t, `ami = "abc"`),
						},
						ProviderAddr: testProvider,
						ResourceAddr: resAddr("test_instance", "build", addrs.NoKey),
					},
					{
						Kind:   KindModuleExpand,
						Module: childModule,
						Name:   childModule.String() + " (expand)",
						ModuleCallExprs: []hcl.Expression{
							testExpr(t, "test_instance.build.ami"),
						},
					},
					{
						Kind:   KindModuleClose,
						Module: childModule,
						Name:   childModule.String() + " (close)",
					},
					{
						Kind:        KindVariable,
						Module:      childModule,
						Name:        "digest",
						VariableCfg: &configs.Variable{Name: "digest"},
					},
					{
						Kind:   KindOutput,
						Module: childModule,
						Name:   "result",
						OutputCfg: &configs.Output{
							Name: "result",
							Expr: testExpr(t, "var.digest"),
						},
					},
					{
						Kind:         KindProviderConfig,
						Module:       addrs.RootModuleInstance,
						Name:         "test",
						ProviderAddr: testProvider,
					},
				}
			}(),
			schemas:       testSchema,
			wantVars:      1,
			wantOutputs:   1,
			wantProviders: 1,
			wantResInst:   1,
			checkDeps: []depCheck{
				// Expand depends on the parent's build resource.
				{from: childExpandName("tagger"), dependsOn: "test_instance.build"},
				// Only variables depend on expand (not outputs/locals).
				{from: "digest", dependsOn: childExpandName("tagger")},
				// Output depends on variable via HCL ref (not on expand directly).
				{from: "result", dependsOn: "digest"},
				// Close depends on output (externally observable), not variable.
				{from: childCloseName("tagger"), dependsOn: "result"},
			},
		},

		// Close depends on child resources and data sources (side effects)
		// in addition to outputs. Variables and locals are excluded.
		"close gates resources and data sources": {
			verts: func() []Vertex {
				childModule := addrs.RootModuleInstance.Child("app", addrs.NoKey)
				return []Vertex{
					{Kind: KindModuleExpand, Module: childModule, Name: childModule.String() + " (expand)"},
					{Kind: KindModuleClose, Module: childModule, Name: childModule.String() + " (close)"},
					{Kind: KindVariable, Module: childModule, Name: "name", VariableCfg: &configs.Variable{Name: "name"}},
					{Kind: KindLocal, Module: childModule, Name: "computed", LocalExpr: testExpr(t, "var.name")},
					{
						Kind: KindOutput, Module: childModule, Name: "out",
						OutputCfg: &configs.Output{Name: "out", Expr: testExpr(t, "local.computed")},
					},
					{
						Kind: KindResource, Module: childModule, Name: "test_instance.server",
						ResourceCfg:  &configs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "server", Config: testBody(t, `ami = var.name`)},
						ProviderAddr: testProvider,
						ResourceAddr: addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "server"}.Instance(addrs.NoKey).Absolute(childModule),
					},
					{
						Kind: KindDataSource, Module: childModule, Name: "test_ami.latest",
						ResourceCfg:  &configs.Resource{Mode: addrs.DataResourceMode, Type: "test_ami", Name: "latest"},
						ProviderAddr: testProvider,
						ResourceAddr: addrs.Resource{Mode: addrs.DataResourceMode, Type: "test_ami", Name: "latest"}.Instance(addrs.NoKey).Absolute(childModule),
					},
					{Kind: KindProviderConfig, Module: addrs.RootModuleInstance, Name: "test", ProviderAddr: testProvider},
				}
			}(),
			schemas:       testSchema,
			wantVars:      1,
			wantLocals:    1,
			wantOutputs:   1,
			wantProviders: 1,
			wantResInst:   2,
			checkDeps: []depCheck{
				// Variable depends on expand.
				{from: "name", dependsOn: childExpandName("app")},
				// Close depends on output, resource, and data (externally observable).
				{from: childCloseName("app"), dependsOn: "out"},
				{from: childCloseName("app"), dependsOn: "test_instance.server"},
				{from: childCloseName("app"), dependsOn: "test_ami.latest"},
			},
		},

		// A child local that references only path.module (no variable dep)
		// should NOT get an expand gate edge. It has no transitive path to
		// expand, which is correct — it doesn't need module call args.
		"local without variable dep has no expand edge": {
			verts: func() []Vertex {
				childModule := addrs.RootModuleInstance.Child("lib", addrs.NoKey)
				return []Vertex{
					{Kind: KindModuleExpand, Module: childModule, Name: childModule.String() + " (expand)"},
					{Kind: KindModuleClose, Module: childModule, Name: childModule.String() + " (close)"},
					{
						Kind: KindLocal, Module: childModule, Name: "static_path",
						LocalExpr: testExpr(t, `"hardcoded"`),
					},
					{
						Kind: KindOutput, Module: childModule, Name: "path",
						OutputCfg: &configs.Output{Name: "path", Expr: testExpr(t, "local.static_path")},
					},
				}
			}(),
			schemas:     testSchema,
			wantLocals:  1,
			wantOutputs: 1,
			wantEdges:   2, // output→local (hcl ref), close→output
			checkDeps: []depCheck{
				{from: "path", dependsOn: "static_path"},
				{from: childCloseName("lib"), dependsOn: "path"},
			},
		},

		// Close does NOT depend on variables or locals — they are internal.
		// This verifies the optimization is correct.
		"close excludes variables and locals": {
			verts: func() []Vertex {
				childModule := addrs.RootModuleInstance.Child("inner", addrs.NoKey)
				return []Vertex{
					{Kind: KindModuleExpand, Module: childModule, Name: childModule.String() + " (expand)"},
					{Kind: KindModuleClose, Module: childModule, Name: childModule.String() + " (close)"},
					{Kind: KindVariable, Module: childModule, Name: "x", VariableCfg: &configs.Variable{Name: "x"}},
					{Kind: KindLocal, Module: childModule, Name: "y", LocalExpr: testExpr(t, "var.x")},
				}
			}(),
			schemas:    testSchema,
			wantVars:   1,
			wantLocals: 1,
			wantEdges:  2, // var→expand, local→var
			checkDeps: []depCheck{
				{from: "x", dependsOn: childExpandName("inner")},
				{from: "y", dependsOn: "x"},
				// Close has NO deps — no outputs/resources/data in this module.
			},
		},

		// Module depends_on propagates through the expand vertex.
		"module depends_on gates child content": {
			verts: func() []Vertex {
				testModule := addrs.RootModuleInstance.Child("test", addrs.NoKey)
				taggerModule := addrs.RootModuleInstance.Child("tagger", addrs.NoKey)
				return []Vertex{
					// test module's output
					{Kind: KindOutput, Module: testModule, Name: "status",
						OutputCfg: &configs.Output{Name: "status", Expr: testExpr(t, `"ok"`)}},
					// test module's expand/close
					{Kind: KindModuleExpand, Module: testModule, Name: testModule.String() + " (expand)"},
					{Kind: KindModuleClose, Module: testModule, Name: testModule.String() + " (close)"},
					// tagger module with depends_on = [module.test]
					{Kind: KindModuleExpand, Module: taggerModule,
						Name: taggerModule.String() + " (expand)",
						ModuleDependsOn: []hcl.Traversal{
							{hcl.TraverseRoot{Name: "module"}, hcl.TraverseAttr{Name: "test"}},
						},
					},
					{Kind: KindModuleClose, Module: taggerModule, Name: taggerModule.String() + " (close)"},
					// tagger's resource — must wait for test to complete
					{Kind: KindVariable, Module: taggerModule, Name: "tags",
						VariableCfg: &configs.Variable{Name: "tags"}},
				}
			}(),
			schemas:     testSchema,
			wantVars:    1,
			wantOutputs: 1,
			checkDeps: []depCheck{
				// tagger expand depends on test close (via depends_on = [module.test])
				{from: childExpandName("tagger"), dependsOn: childCloseName("test")},
				// tagger's variable depends on tagger's expand
				{from: "tags", dependsOn: childExpandName("tagger")},
			},
		},

		// Pattern: resource body references a child module's output.
		// data.oci_exec_test { digest = module.this.image_ref }
		"resource references child module output": {
			verts: func() []Vertex {
				childModule := addrs.RootModuleInstance.Child("this", addrs.NoKey)
				return []Vertex{
					{
						Kind:   KindOutput,
						Module: childModule,
						Name:   "image_ref",
						OutputCfg: &configs.Output{
							Name: "image_ref",
							Expr: testExpr(t, `"sha256:abc"`),
						},
					},
					{
						Kind:   KindResource,
						Module: addrs.RootModuleInstance,
						Name:   "test_instance.checker",
						ResourceCfg: &configs.Resource{
							Mode:   addrs.ManagedResourceMode,
							Type:   "test_instance",
							Name:   "checker",
							Config: testBody(t, `ami = module.this.image_ref`),
						},
						ProviderAddr: testProvider,
						ResourceAddr: resAddr("test_instance", "checker", addrs.NoKey),
					},
					{
						Kind:         KindProviderConfig,
						Module:       addrs.RootModuleInstance,
						Name:         "test",
						ProviderAddr: testProvider,
					},
				}
			}(),
			schemas:       testSchema,
			wantOutputs:   1,
			wantProviders: 1,
			wantResInst:   1,
			wantEdges:     2, // checker→this.image_ref, checker→provider
			checkDeps: []depCheck{
				{from: "test_instance.checker", dependsOn: "image_ref"},
			},
		},

		// Pattern: output with depends_on referencing both a module and a data source.
		// depends_on = [module.structure_test, data.test_ami.latest]
		"depends_on with mixed module and data source": {
			verts: func() []Vertex {
				childModule := addrs.RootModuleInstance.Child("structure_test", addrs.NoKey)
				return []Vertex{
					{
						Kind:   KindOutput,
						Module: childModule,
						Name:   "result",
						OutputCfg: &configs.Output{
							Name: "result",
							Expr: testExpr(t, `"ok"`),
						},
					},
					{
						Kind:         KindDataSource,
						Module:       addrs.RootModuleInstance,
						Name:         "test_ami.latest",
						ResourceCfg:  &configs.Resource{Mode: addrs.DataResourceMode, Type: "test_ami", Name: "latest", Config: testBody(t, `filter = "ubuntu"`)},
						ProviderAddr: testProvider,
						ResourceAddr: dataAddr("test_ami", "latest", addrs.NoKey),
					},
					{
						Kind:   KindResource,
						Module: addrs.RootModuleInstance,
						Name:   "test_instance.guarded",
						ResourceCfg: &configs.Resource{
							Mode:   addrs.ManagedResourceMode,
							Type:   "test_instance",
							Name:   "guarded",
							Config: testBody(t, `ami = "abc"`),
							DependsOn: []hcl.Traversal{
								{hcl.TraverseRoot{Name: "module"}, hcl.TraverseAttr{Name: "structure_test"}},
								{hcl.TraverseRoot{Name: "data"}, hcl.TraverseAttr{Name: "test_ami"}, hcl.TraverseAttr{Name: "latest"}},
							},
						},
						ProviderAddr: testProvider,
						ResourceAddr: resAddr("test_instance", "guarded", addrs.NoKey),
					},
					{
						Kind:         KindProviderConfig,
						Module:       addrs.RootModuleInstance,
						Name:         "test",
						ProviderAddr: testProvider,
					},
				}
			}(),
			schemas:       testSchema,
			wantOutputs:   1,
			wantProviders: 1,
			wantResInst:   2,
			wantEdges:     4, // guarded→provider, guarded→structure_test.result, guarded→test_ami.latest, latest→provider
			checkDeps: []depCheck{
				{from: "test_instance.guarded", dependsOn: "result"},
				{from: "test_instance.guarded", dependsOn: "test_ami.latest"},
			},
		},

		// Pattern: local filters over module output.
		// local.x = module.build["key"].image_ref
		"local depends on specific module call instance output": {
			verts: func() []Vertex {
				childModule := addrs.RootModuleInstance.Child("build", addrs.StringKey("v1"))
				return []Vertex{
					{
						Kind:   KindOutput,
						Module: childModule,
						Name:   "image_ref",
						OutputCfg: &configs.Output{
							Name: "image_ref",
							Expr: testExpr(t, `"sha256:abc"`),
						},
					},
					{
						Kind:      KindLocal,
						Module:    addrs.RootModuleInstance,
						Name:      "latest",
						LocalExpr: testExpr(t, `module.build["v1"].image_ref`),
					},
				}
			}(),
			schemas:    testSchema,
			wantLocals: 1,
			wantOutputs: 1,
			wantEdges:  1, // local.latest→build["v1"].image_ref
			checkDeps: []depCheck{
				{from: "latest", dependsOn: "image_ref"},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			g, _, diags := BuildGraph(tc.verts, tc.schemas)

			if tc.wantCycleErr {
				if !diags.HasErrors() {
					t.Fatal("expected cycle error, got none")
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected errors: %s", diags.Err())
			}

			// Apply target filter if specified.
			if tc.targets != nil {
				g = FilterTargets(g, NewTargetFilter(tc.targets))
				if got := len(g.Verts); got != tc.wantFiltered {
					t.Errorf("FilterTargets: got %d vertices, want %d", got, tc.wantFiltered)
					for i, v := range g.Verts {
						t.Logf("  [%d] kind=%d name=%s", i, v.Kind, v.Name)
					}
				}
			}

			// Count assertions apply to the final graph (post-filter if filtered).
			if tc.targets == nil {
				if got := len(g.Vars); got != tc.wantVars {
					t.Errorf("Vars: got %d, want %d", got, tc.wantVars)
				}
				if got := len(g.Locals); got != tc.wantLocals {
					t.Errorf("Locals: got %d, want %d", got, tc.wantLocals)
				}
				if got := len(g.Outputs); got != tc.wantOutputs {
					t.Errorf("Outputs: got %d, want %d", got, tc.wantOutputs)
				}
				if got := len(g.Providers); got != tc.wantProviders {
					t.Errorf("Providers: got %d, want %d", got, tc.wantProviders)
				}
				if got := len(g.ResInst); got != tc.wantResInst {
					t.Errorf("ResInst keys: got %d, want %d", got, tc.wantResInst)
				}

				edges := 0
				for _, deps := range g.Deps {
					edges += len(deps)
				}
				if tc.wantEdges != 0 && edges != tc.wantEdges {
					t.Errorf("edges: got %d, want %d", edges, tc.wantEdges)
				}
			}

			// Verify reverse edges are consistent with forward edges.
			for v, deps := range g.Deps {
				for _, dep := range deps {
					if !slices.Contains(g.RevDeps[dep], ID(v)) {
						t.Errorf("RevDeps[%d] missing %d (forward edge exists)", dep, v)
					}
				}
			}

			// Verify schema attachment: all resource/data/provider vertices
			// with a known provider should have a non-nil Schema.
			for i := range g.Verts {
				v := &g.Verts[i]
				switch v.Kind {
				case KindResource, KindDataSource:
					if v.ResourceCfg != nil {
						if _, ok := tc.schemas[v.ProviderAddr]; ok {
							if v.Schema == nil {
								t.Errorf("vertex %d (%s): expected schema to be attached", i, v.Name)
							}
						}
					}
				case KindProviderConfig:
					if _, ok := tc.schemas[v.ProviderAddr]; ok {
						if v.Schema == nil {
							t.Errorf("vertex %d (%s): expected provider schema to be attached", i, v.Name)
						}
					}
				}
			}

			// Check specific dependency relationships (against final graph).
			for _, dc := range tc.checkDeps {
				fromID := findVertexByName(g, dc.from)
				toID := findVertexByName(g, dc.dependsOn)
				if fromID == -1 {
					t.Errorf("checkDeps: vertex %q not found", dc.from)
					continue
				}
				if toID == -1 {
					t.Errorf("checkDeps: vertex %q not found", dc.dependsOn)
					continue
				}
				if !slices.Contains(g.Deps[fromID], toID) {
					t.Errorf("expected %q (id=%d) to depend on %q (id=%d), but no edge found\n  deps of %d: %v",
						dc.from, fromID, dc.dependsOn, toID, fromID, g.Deps[fromID])
				}
			}
		})
	}
}

type depCheck struct {
	from      string
	dependsOn string
}

func findVertexByName(g *Graph, name string) ID {
	for i, v := range g.Verts {
		if v.Name == name {
			return ID(i)
		}
	}
	return -1
}

func childExpandName(callName string) string {
	return addrs.RootModuleInstance.Child(callName, addrs.NoKey).String() + " (expand)"
}

func childCloseName(callName string) string {
	return addrs.RootModuleInstance.Child(callName, addrs.NoKey).String() + " (close)"
}

// testBody parses an HCL body from a string, suitable for configs.Resource.Config.
func testBody(t *testing.T, src string) hcl.Body {
	t.Helper()
	f, diags := hclsyntax.ParseConfig([]byte(src), "test.tf", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parsing body %q: %s", src, diags.Error())
	}
	return f.Body
}
