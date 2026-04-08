// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"testing"
	"unique"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/instances"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// newTestEvalData constructs an evalData backed by the given graph and results,
// scoped to the given module. The config is needed for GetResource/GetModule
// to look up resource declarations and module calls.
func newTestEvalData(g *Graph, r *Results, cfg *configs.Config, module addrs.ModuleInstance, repData instances.RepetitionData) *evalData {
	return &evalData{
		graph:     g,
		results:   r,
		config:    cfg,
		module:    module,
		repData:   repData,
		rootDir:   "/root",
		workDir:   "/work",
		workspace: "default",
	}
}

func TestEvalData_GetInputVariable(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		varName   string
		varCfg    *configs.Variable
		value     cty.Value // value in results (NilVal = not set)
		wantValue cty.Value
		wantErr   bool
	}{
		"normal variable": {
			varName:   "region",
			varCfg:    &configs.Variable{Name: "region"},
			value:     cty.StringVal("us-east-1"),
			wantValue: cty.StringVal("us-east-1"),
		},
		"sensitive variable": {
			varName:   "secret",
			varCfg:    &configs.Variable{Name: "secret", Sensitive: true},
			value:     cty.StringVal("hunter2"),
			wantValue: cty.StringVal("hunter2").WithMarks(markSensitive),
		},
		"NilVal returns DynamicVal": {
			varName:   "unset",
			varCfg:    &configs.Variable{Name: "unset"},
			value:     cty.NilVal,
			wantValue: cty.DynamicVal,
		},
		"undeclared variable": {
			varName: "nonexistent",
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			g := &Graph{
				Verts: []Vertex{},
				Vars:  make(map[addrKey]ID),
			}
			r := NewResults(1)

			if tc.varCfg != nil {
				id := ID(len(g.Verts))
				g.Verts = append(g.Verts, Vertex{
					Kind:        KindVariable,
					Module:      addrs.RootModuleInstance,
					Name:        tc.varName,
					VariableCfg: tc.varCfg,
				})
				g.Vars[addrKey{unique.Make(addrs.RootModuleInstance.String()), unique.Make(tc.varName)}] = id
				if tc.value != cty.NilVal {
					r.Values[id] = tc.value
				}
			}

			d := newTestEvalData(g, r, configs.NewEmptyConfig(), addrs.RootModuleInstance, instances.RepetitionData{})
			got, diags := d.GetInputVariable(t.Context(), addrs.InputVariable{Name: tc.varName}, tfdiags.SourceRange{})

			if tc.wantErr {
				if !diags.HasErrors() {
					t.Fatal("expected error, got none")
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected error: %s", diags.Err())
			}
			if !got.RawEquals(tc.wantValue) {
				t.Errorf("got %s, want %s", got.GoString(), tc.wantValue.GoString())
			}
		})
	}
}

func TestEvalData_GetLocalValue(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		localName string
		exists    bool
		value     cty.Value
		wantValue cty.Value
		wantErr   bool
	}{
		"normal local": {
			localName: "name",
			exists:    true,
			value:     cty.StringVal("hello"),
			wantValue: cty.StringVal("hello"),
		},
		"NilVal returns DynamicVal": {
			localName: "pending",
			exists:    true,
			value:     cty.NilVal,
			wantValue: cty.DynamicVal,
		},
		"undeclared local": {
			localName: "nope",
			wantErr:   true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			g := &Graph{
				Verts:  []Vertex{},
				Locals: make(map[addrKey]ID),
			}
			r := NewResults(1)

			if tc.exists {
				id := ID(len(g.Verts))
				g.Verts = append(g.Verts, Vertex{Kind: KindLocal, Module: addrs.RootModuleInstance, Name: tc.localName})
				g.Locals[addrKey{unique.Make(addrs.RootModuleInstance.String()), unique.Make(tc.localName)}] = id
				if tc.value != cty.NilVal {
					r.Values[id] = tc.value
				}
			}

			d := newTestEvalData(g, r, configs.NewEmptyConfig(), addrs.RootModuleInstance, instances.RepetitionData{})
			got, diags := d.GetLocalValue(t.Context(), addrs.LocalValue{Name: tc.localName}, tfdiags.SourceRange{})

			if tc.wantErr {
				if !diags.HasErrors() {
					t.Fatal("expected error, got none")
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected error: %s", diags.Err())
			}
			if !got.RawEquals(tc.wantValue) {
				t.Errorf("got %s, want %s", got.GoString(), tc.wantValue.GoString())
			}
		})
	}
}

func TestEvalData_GetResource(t *testing.T) {
	t.Parallel()

	// Helper: build a config with a resource declaration.
	cfgWith := func(t *testing.T, src string) *configs.Config {
		t.Helper()
		return testConfig(configs.ModuleFromStringForTesting(t, src), nil)
	}

	tests := map[string]struct {
		config    *configs.Config
		addr      addrs.Resource
		instances []struct {
			key   addrs.InstanceKey
			value cty.Value
		}
		wantValue cty.Value
		wantErr   bool
	}{
		"singleton": {
			config: cfgWith(t, `resource "test_instance" "web" { ami = "abc" }`),
			addr:   addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			instances: []struct {
				key   addrs.InstanceKey
				value cty.Value
			}{
				{addrs.NoKey, cty.StringVal("web-value")},
			},
			wantValue: cty.StringVal("web-value"),
		},
		"singleton NilVal": {
			config: cfgWith(t, `resource "test_instance" "web" { ami = "abc" }`),
			addr:   addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			instances: []struct {
				key   addrs.InstanceKey
				value cty.Value
			}{
				{addrs.NoKey, cty.NilVal},
			},
			wantValue: cty.DynamicVal,
		},
		"count with values": {
			config: cfgWith(t, `
				resource "test_instance" "web" {
					count = 3
					ami = "abc"
				}`),
			addr:   addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			instances: []struct {
				key   addrs.InstanceKey
				value cty.Value
			}{
				{addrs.IntKey(0), cty.StringVal("v0")},
				{addrs.IntKey(1), cty.StringVal("v1")},
				{addrs.IntKey(2), cty.StringVal("v2")},
			},
			wantValue: cty.TupleVal([]cty.Value{
				cty.StringVal("v0"),
				cty.StringVal("v1"),
				cty.StringVal("v2"),
			}),
		},
		"count with NilVal gap": {
			config: cfgWith(t, `
				resource "test_instance" "web" {
					count = 3
					ami = "abc"
				}`),
			addr:   addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			instances: []struct {
				key   addrs.InstanceKey
				value cty.Value
			}{
				{addrs.IntKey(0), cty.StringVal("v0")},
				{addrs.IntKey(1), cty.NilVal}, // gap
				{addrs.IntKey(2), cty.StringVal("v2")},
			},
			wantValue: cty.TupleVal([]cty.Value{
				cty.StringVal("v0"),
				cty.DynamicVal, // filled with DynamicVal
				cty.StringVal("v2"),
			}),
		},
		"count zero": {
			config: cfgWith(t, `
				resource "test_instance" "web" {
					count = 0
					ami = "abc"
				}`),
			addr:   addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			// no instances
			wantValue: cty.EmptyTupleVal,
		},
		// When count evaluation fails during expansion, the resource is
		// registered as a singleton (NoKey) but rc.Count is still set in
		// the config. GetResource must return the singleton wrapped as a
		// single-element tuple, not an empty tuple.
		// When count evaluation fails during expansion, the resource is
		// registered as a singleton (NoKey) but rc.Count is still set.
		// Downstream HCL does resource[0], so GetResource must return
		// a single-element tuple to honor the count contract.
		"count fallback to singleton": {
			config: cfgWith(t, `
				resource "test_instance" "web" {
					count = 1
					ami = "abc"
				}`),
			addr: addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			instances: []struct {
				key   addrs.InstanceKey
				value cty.Value
			}{
				{addrs.NoKey, cty.StringVal("fallback")},
			},
			wantValue: cty.TupleVal([]cty.Value{cty.StringVal("fallback")}),
		},
		"for_each with values": {
			config: cfgWith(t, `
				resource "test_instance" "web" {
					for_each = {}
					ami = "abc"
				}`),
			addr:   addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			instances: []struct {
				key   addrs.InstanceKey
				value cty.Value
			}{
				{addrs.StringKey("a"), cty.StringVal("va")},
				{addrs.StringKey("b"), cty.StringVal("vb")},
			},
			wantValue: cty.ObjectVal(map[string]cty.Value{
				"a": cty.StringVal("va"),
				"b": cty.StringVal("vb"),
			}),
		},
		"for_each empty": {
			config: cfgWith(t, `
				resource "test_instance" "web" {
					for_each = {}
					ami = "abc"
				}`),
			addr:   addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			// no instances
			wantValue: cty.EmptyObjectVal,
		},
		// Pattern: data "external" "cache_lookup" referenced by a resource.
		// Data sources use DataResourceMode but share the ResInst map.
		"data source singleton": {
			config: cfgWith(t, `data "test_ami" "latest" { filter = "ubuntu" }`),
			addr:   addrs.Resource{Mode: addrs.DataResourceMode, Type: "test_ami", Name: "latest"},
			instances: []struct {
				key   addrs.InstanceKey
				value cty.Value
			}{
				{addrs.NoKey, cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal("ami-123")})},
			},
			wantValue: cty.ObjectVal(map[string]cty.Value{"id": cty.StringVal("ami-123")}),
		},

		// Pattern: for_each with NoKey fallback.
		// Same as count fallback — if for_each eval fails during expansion,
		// resource is registered as singleton (NoKey) but rc.ForEach is set.
		"for_each fallback to singleton": {
			config: cfgWith(t, `
				resource "test_instance" "web" {
					for_each = {}
					ami = "abc"
				}`),
			addr:   addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			instances: []struct {
				key   addrs.InstanceKey
				value cty.Value
			}{
				{addrs.NoKey, cty.StringVal("fallback")},
			},
			// Should return the singleton wrapped as a single-element object,
			// not an empty object.
			wantValue: cty.ObjectVal(map[string]cty.Value{"": cty.StringVal("fallback")}),
		},

		// Pattern: for_each resource where one instance was skipped by the
		// walk (NilVal in results). GetResource must substitute DynamicVal
		// for the missing key and still return a valid object.
		"for_each with NilVal instance": {
			config: cfgWith(t, `
				resource "test_instance" "web" {
					for_each = {}
					ami = "abc"
				}`),
			addr: addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "web"},
			instances: []struct {
				key   addrs.InstanceKey
				value cty.Value
			}{
				{addrs.StringKey("a"), cty.StringVal("va")},
				{addrs.StringKey("b"), cty.NilVal}, // skipped by walk
				{addrs.StringKey("c"), cty.StringVal("vc")},
			},
			// NilVal instance should be excluded (not in the object),
			// matching the existing behavior where NilVal keys are skipped.
			wantValue: cty.ObjectVal(map[string]cty.Value{
				"a": cty.StringVal("va"),
				"c": cty.StringVal("vc"),
			}),
		},

		"undeclared resource": {
			config:  cfgWith(t, ``),
			addr:    addrs.Resource{Mode: addrs.ManagedResourceMode, Type: "test_instance", Name: "nope"},
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			g := &Graph{
				Verts:   []Vertex{},
				ResInst: make(map[string][]ID),
			}
			n := len(tc.instances)
			if n == 0 {
				n = 1
			}
			r := NewResults(n)

			absRes := tc.addr.Absolute(addrs.RootModuleInstance)
			for _, inst := range tc.instances {
				id := ID(len(g.Verts))
				instAddr := tc.addr.Instance(inst.key).Absolute(addrs.RootModuleInstance)
				g.Verts = append(g.Verts, Vertex{
					Kind:         KindResource,
					Module:       addrs.RootModuleInstance,
					Name:         tc.addr.String(),
					ResourceAddr: instAddr,
				})
				g.ResInst[absRes.String()] = append(g.ResInst[absRes.String()], id)
				if inst.value != cty.NilVal {
					r.Values[id] = inst.value
				}
			}

			d := newTestEvalData(g, r, tc.config, addrs.RootModuleInstance, instances.RepetitionData{})
			got, diags := d.GetResource(t.Context(), tc.addr, tfdiags.SourceRange{})

			if tc.wantErr {
				if !diags.HasErrors() {
					t.Fatal("expected error, got none")
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected error: %s", diags.Err())
			}
			if !got.RawEquals(tc.wantValue) {
				t.Errorf("got %s, want %s", got.GoString(), tc.wantValue.GoString())
			}
		})
	}
}

func TestEvalData_GetModule(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		config *configs.Config
		// childInstances defines output vertices in child module instances.
		childInstances []struct {
			key     addrs.InstanceKey
			outputs map[string]cty.Value
		}
		callName   string
		evalModule addrs.ModuleInstance // scope for evalData (default: root)
		wantValue  cty.Value
		wantErr    bool
	}{
		"singleton module": {
			config: testConfig(
				configs.ModuleFromStringForTesting(t, `module "child" { source = "./child" }`),
				map[string]*configs.Module{
					"child": configs.ModuleFromStringForTesting(t, `output "x" { value = "hello" }`),
				},
			),
			callName: "child",
			childInstances: []struct {
				key     addrs.InstanceKey
				outputs map[string]cty.Value
			}{
				{addrs.NoKey, map[string]cty.Value{"x": cty.StringVal("hello")}},
			},
			wantValue: cty.ObjectVal(map[string]cty.Value{"x": cty.StringVal("hello")}),
		},
		"count module": {
			config: testConfig(
				configs.ModuleFromStringForTesting(t, `module "child" {
					source = "./child"
					count = 2
				}`),
				map[string]*configs.Module{
					"child": configs.ModuleFromStringForTesting(t, `output "x" { value = "hi" }`),
				},
			),
			callName: "child",
			childInstances: []struct {
				key     addrs.InstanceKey
				outputs map[string]cty.Value
			}{
				{addrs.IntKey(0), map[string]cty.Value{"x": cty.StringVal("v0")}},
				{addrs.IntKey(1), map[string]cty.Value{"x": cty.StringVal("v1")}},
			},
			wantValue: cty.TupleVal([]cty.Value{
				cty.ObjectVal(map[string]cty.Value{"x": cty.StringVal("v0")}),
				cty.ObjectVal(map[string]cty.Value{"x": cty.StringVal("v1")}),
			}),
		},
		// When module count evaluation fails during expansion, the module
		// is registered as singleton (NoKey) but the config still has count.
		// GetModule must return the singleton outputs as a single-element
		// tuple, not an empty tuple.
		"count module fallback to singleton": {
			config: testConfig(
				configs.ModuleFromStringForTesting(t, `module "child" {
					source = "./child"
					count = 2
				}`),
				map[string]*configs.Module{
					"child": configs.ModuleFromStringForTesting(t, `output "x" { value = "hi" }`),
				},
			),
			callName: "child",
			childInstances: []struct {
				key     addrs.InstanceKey
				outputs map[string]cty.Value
			}{
				{addrs.NoKey, map[string]cty.Value{"x": cty.StringVal("fallback")}},
			},
			wantValue: cty.TupleVal([]cty.Value{
				cty.ObjectVal(map[string]cty.Value{"x": cty.StringVal("fallback")}),
			}),
		},
		"for_each module": {
			config: testConfig(
				configs.ModuleFromStringForTesting(t, `module "child" {
					source = "./child"
					for_each = {}
				}`),
				map[string]*configs.Module{
					"child": configs.ModuleFromStringForTesting(t, `output "x" { value = "hi" }`),
				},
			),
			callName: "child",
			childInstances: []struct {
				key     addrs.InstanceKey
				outputs map[string]cty.Value
			}{
				{addrs.StringKey("a"), map[string]cty.Value{"x": cty.StringVal("va")}},
				{addrs.StringKey("b"), map[string]cty.Value{"x": cty.StringVal("vb")}},
			},
			wantValue: cty.ObjectVal(map[string]cty.Value{
				"a": cty.ObjectVal(map[string]cty.Value{"x": cty.StringVal("va")}),
				"b": cty.ObjectVal(map[string]cty.Value{"x": cty.StringVal("vb")}),
			}),
		},
		"empty module no instances": {
			config: testConfig(
				configs.ModuleFromStringForTesting(t, `module "child" { source = "./child" }`),
				map[string]*configs.Module{
					"child": configs.ModuleFromStringForTesting(t, `output "x" { value = "hi" }`),
				},
			),
			callName:  "child",
			wantValue: cty.EmptyObjectVal,
		},
		"NilVal output replaced with DynamicVal": {
			config: testConfig(
				configs.ModuleFromStringForTesting(t, `module "child" { source = "./child" }`),
				map[string]*configs.Module{
					"child": configs.ModuleFromStringForTesting(t, `output "x" { value = "hi" }`),
				},
			),
			callName: "child",
			childInstances: []struct {
				key     addrs.InstanceKey
				outputs map[string]cty.Value
			}{
				{addrs.NoKey, map[string]cty.Value{"x": cty.NilVal}},
			},
			wantValue: cty.ObjectVal(map[string]cty.Value{"x": cty.DynamicVal}),
		},
		// Pattern: for_each module with multiple outputs.
		// images-private: module.build has image_ref and repo_tags outputs,
		// accessed via { for k, v in module.build : k => v.image_ref }.
		// GetModule must return an object where keys are for_each keys and
		// values are objects with all output attributes.
		"for_each module multiple outputs": {
			config: testConfig(
				configs.ModuleFromStringForTesting(t, `module "build" {
					source   = "./build"
					for_each = {}
				}`),
				map[string]*configs.Module{
					"build": configs.ModuleFromStringForTesting(t, `
						output "image_ref" { value = "ref" }
						output "repo_tags" { value = "tags" }
					`),
				},
			),
			callName: "build",
			childInstances: []struct {
				key     addrs.InstanceKey
				outputs map[string]cty.Value
			}{
				{addrs.StringKey("web"), map[string]cty.Value{
					"image_ref": cty.StringVal("sha256:web"),
					"repo_tags": cty.StringVal("latest"),
				}},
				{addrs.StringKey("api"), map[string]cty.Value{
					"image_ref": cty.StringVal("sha256:api"),
					"repo_tags": cty.StringVal("v2"),
				}},
			},
			wantValue: cty.ObjectVal(map[string]cty.Value{
				"web": cty.ObjectVal(map[string]cty.Value{
					"image_ref": cty.StringVal("sha256:web"),
					"repo_tags": cty.StringVal("latest"),
				}),
				"api": cty.ObjectVal(map[string]cty.Value{
					"image_ref": cty.StringVal("sha256:api"),
					"repo_tags": cty.StringVal("v2"),
				}),
			}),
		},

		// Pattern: for_each module fallback to singleton.
		// Same bug class as count fallback — if for_each eval fails,
		// module registered as singleton but config has for_each set.
		"for_each module fallback to singleton": {
			config: testConfig(
				configs.ModuleFromStringForTesting(t, `module "child" {
					source   = "./child"
					for_each = {}
				}`),
				map[string]*configs.Module{
					"child": configs.ModuleFromStringForTesting(t, `output "x" { value = "hi" }`),
				},
			),
			callName: "child",
			childInstances: []struct {
				key     addrs.InstanceKey
				outputs map[string]cty.Value
			}{
				{addrs.NoKey, map[string]cty.Value{"x": cty.StringVal("fallback")}},
			},
			wantValue: cty.ObjectVal(map[string]cty.Value{
				"": cty.ObjectVal(map[string]cty.Value{"x": cty.StringVal("fallback")}),
			}),
		},

		// Pattern: module.publish.locked_configs accessed from inside
		// a child module (e.g. images/etcd-iamguarded/main.tf calls
		// module.publish, and publish's output is accessed by sibling
		// module.test). Tests GetModule scoped to a non-root module.
		"nested module output from child scope": {
			config: func() *configs.Config {
				publishMod := configs.ModuleFromStringForTesting(t, `
					output "locked_configs" { value = {} }
				`)
				parentMod := configs.ModuleFromStringForTesting(t, `
					module "publish" { source = "./publish" }
					module "test"    { source = "./test" }
				`)
				cfg := testConfig(parentMod, map[string]*configs.Module{
					"publish": publishMod,
				})
				// Nest under a grandparent so the eval scope is a child module.
				root := &configs.Config{
					Module: configs.ModuleFromStringForTesting(t, `
						module "app" { source = "./app" }
					`),
					Children: map[string]*configs.Config{"app": cfg},
				}
				root.Root = root
				cfg.Root = root
				cfg.Parent = root
				cfg.Path = addrs.Module{"app"}
				return root
			}(),
			callName:   "publish",
			evalModule: addrs.RootModuleInstance.Child("app", addrs.NoKey),
			childInstances: []struct {
				key     addrs.InstanceKey
				outputs map[string]cty.Value
			}{
				{addrs.NoKey, map[string]cty.Value{
					"locked_configs": cty.ObjectVal(map[string]cty.Value{
						"web": cty.StringVal("config-web"),
					}),
				}},
			},
			wantValue: cty.ObjectVal(map[string]cty.Value{
				"locked_configs": cty.ObjectVal(map[string]cty.Value{
					"web": cty.StringVal("config-web"),
				}),
			}),
		},

		"undeclared module": {
			config:   testConfig(configs.ModuleFromStringForTesting(t, ``), nil),
			callName: "nope",
			wantErr:  true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			evalModule := tc.evalModule
			if evalModule == nil {
				evalModule = addrs.RootModuleInstance
			}

			g := &Graph{
				Verts:        []Vertex{},
				Outputs:      make(map[addrKey]ID),
				ChildOutputs: make(map[addrKey][]ID),
			}

			// Count total output vertices needed.
			total := 0
			for _, inst := range tc.childInstances {
				total += len(inst.outputs)
			}
			if total == 0 {
				total = 1
			}
			r := NewResults(total)

			// Build output vertices for each child instance.
			for _, inst := range tc.childInstances {
				childAddr := evalModule.Child(tc.callName, inst.key)
				for outName, val := range inst.outputs {
					id := ID(len(g.Verts))
					g.Verts = append(g.Verts, Vertex{
						Kind:   KindOutput,
						Module: childAddr,
						Name:   outName,
						OutputCfg: &configs.Output{
							Name: outName,
						},
					})
					g.Outputs[addrKey{unique.Make(childAddr.String()), unique.Make(outName)}] = id

					// Build ChildOutputs index.
					parent := childAddr[:len(childAddr)-1]
					coKey := addrKey{unique.Make(parent.String()), unique.Make(tc.callName)}
					g.ChildOutputs[coKey] = append(g.ChildOutputs[coKey], id)

					if val != cty.NilVal {
						r.Values[id] = val
					}
				}
			}

			d := newTestEvalData(g, r, tc.config, evalModule, instances.RepetitionData{})
			got, diags := d.GetModule(t.Context(), addrs.ModuleCall{Name: tc.callName}, tfdiags.SourceRange{})

			if tc.wantErr {
				if !diags.HasErrors() {
					t.Fatal("expected error, got none")
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected error: %s", diags.Err())
			}
			if !got.RawEquals(tc.wantValue) {
				t.Errorf("got %s, want %s", got.GoString(), tc.wantValue.GoString())
			}
		})
	}
}

func TestEvalData_GetCountAttr(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		attr    string
		repData instances.RepetitionData
		want    cty.Value
		wantErr bool
	}{
		"index available": {
			attr:    "index",
			repData: instances.RepetitionData{CountIndex: cty.NumberIntVal(3)},
			want:    cty.NumberIntVal(3),
		},
		"index unavailable returns unknown": {
			attr:    "index",
			repData: instances.RepetitionData{},
			want:    cty.UnknownVal(cty.Number),
		},
		"unsupported attr": {
			attr:    "bogus",
			repData: instances.RepetitionData{},
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newTestEvalData(&Graph{}, NewResults(0), configs.NewEmptyConfig(), addrs.RootModuleInstance, tc.repData)
			got, diags := d.GetCountAttr(t.Context(), addrs.CountAttr{Name: tc.attr}, tfdiags.SourceRange{})
			if tc.wantErr {
				if !diags.HasErrors() {
					t.Fatal("expected error, got none")
				}
			}
			if tc.want != cty.NilVal && !got.RawEquals(tc.want) {
				t.Errorf("got %s, want %s", got.GoString(), tc.want.GoString())
			}
		})
	}
}

func TestEvalData_GetForEachAttr(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		attr    string
		repData instances.RepetitionData
		want    cty.Value
		wantErr bool
	}{
		"key available": {
			attr:    "key",
			repData: instances.RepetitionData{EachKey: cty.StringVal("web")},
			want:    cty.StringVal("web"),
		},
		"value available": {
			attr:    "value",
			repData: instances.RepetitionData{EachValue: cty.StringVal("config")},
			want:    cty.StringVal("config"),
		},
		"key unavailable returns unknown": {
			attr:    "key",
			repData: instances.RepetitionData{},
			want:    cty.UnknownVal(cty.String),
		},
		"value unavailable returns unknown": {
			attr:    "value",
			repData: instances.RepetitionData{},
			want:    cty.DynamicVal,
		},
		// Pattern: each.value is a complex object, not just a string.
		// images-private: for_each = local.locked_configs where each
		// value is { tags = [...], latest = true, ... }.
		"value is complex object": {
			attr: "value",
			repData: instances.RepetitionData{
				EachValue: cty.ObjectVal(map[string]cty.Value{
					"tags":   cty.ListVal([]cty.Value{cty.StringVal("latest"), cty.StringVal("v1")}),
					"latest": cty.True,
				}),
			},
			want: cty.ObjectVal(map[string]cty.Value{
				"tags":   cty.ListVal([]cty.Value{cty.StringVal("latest"), cty.StringVal("v1")}),
				"latest": cty.True,
			}),
		},
		"unsupported attr": {
			attr:    "bogus",
			repData: instances.RepetitionData{},
			wantErr: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newTestEvalData(&Graph{}, NewResults(0), configs.NewEmptyConfig(), addrs.RootModuleInstance, tc.repData)
			got, diags := d.GetForEachAttr(t.Context(), addrs.ForEachAttr{Name: tc.attr}, tfdiags.SourceRange{})
			if tc.wantErr {
				if !diags.HasErrors() {
					t.Fatal("expected error, got none")
				}
			}
			if tc.want != cty.NilVal && !got.RawEquals(tc.want) {
				t.Errorf("got %s, want %s", got.GoString(), tc.want.GoString())
			}
		})
	}
}

func TestEvalData_GetPathAttr(t *testing.T) {
	t.Parallel()

	childMod := addrs.RootModuleInstance.Child("child", addrs.NoKey)
	cfg := testConfig(
		configs.ModuleFromStringForTesting(t, `module "child" { source = "./child" }`),
		map[string]*configs.Module{
			"child": configs.ModuleFromStringForTesting(t, ``),
		},
	)

	tests := map[string]struct {
		attr   string
		module addrs.ModuleInstance
		want   cty.Value
	}{
		"cwd":              {attr: "cwd", module: addrs.RootModuleInstance, want: cty.StringVal("/work")},
		"root":             {attr: "root", module: addrs.RootModuleInstance, want: cty.StringVal("/root")},
		"module at root":   {attr: "module", module: addrs.RootModuleInstance, want: cty.StringVal(cfg.Module.SourceDir)},
		"module at child":  {attr: "module", module: childMod, want: cty.StringVal(cfg.Children["child"].Module.SourceDir)},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newTestEvalData(&Graph{}, NewResults(0), cfg, tc.module, instances.RepetitionData{})
			got, diags := d.GetPathAttr(t.Context(), addrs.PathAttr{Name: tc.attr}, tfdiags.SourceRange{})
			if diags.HasErrors() {
				t.Fatalf("unexpected error: %s", diags.Err())
			}
			if !got.RawEquals(tc.want) {
				t.Errorf("got %s, want %s", got.GoString(), tc.want.GoString())
			}
		})
	}
}

func TestEvalData_GetTerraformAttr(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		attr    string
		want    cty.Value
		wantErr bool
	}{
		"workspace":  {attr: "workspace", want: cty.StringVal("default")},
		"env":        {attr: "env", want: cty.StringVal("default")},
		"applying":   {attr: "applying", want: cty.False},
		"unsupported": {attr: "bogus", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := newTestEvalData(&Graph{}, NewResults(0), configs.NewEmptyConfig(), addrs.RootModuleInstance, instances.RepetitionData{})
			got, diags := d.GetTerraformAttr(t.Context(), addrs.TerraformAttr{Name: tc.attr}, tfdiags.SourceRange{})
			if tc.wantErr {
				if !diags.HasErrors() {
					t.Fatal("expected error, got none")
				}
				return
			}
			if diags.HasErrors() {
				t.Fatalf("unexpected error: %s", diags.Err())
			}
			if !got.RawEquals(tc.want) {
				t.Errorf("got %s, want %s", got.GoString(), tc.want.GoString())
			}
		})
	}
}

func TestStubProviderFunction(t *testing.T) {
	t.Parallel()

	// stubProviderFunction should return a real function that produces
	// DynamicVal, not an error. This lets expressions like
	// provider::oci::parse(...) propagate unknowns through the graph.
	fn, diags := stubProviderFunction(
		t.Context(),
		addrs.ProviderFunction{
			ProviderName: "oci",
			Function:     "parse",
		},
		tfdiags.SourceRange{},
	)
	if diags.HasErrors() {
		t.Fatalf("expected no errors, got: %s", diags.Err())
	}
	if fn == nil {
		t.Fatal("expected a function, got nil")
	}

	// Call the returned function — it should accept anything and return DynamicVal.
	result, err := fn.Call([]cty.Value{cty.StringVal("test-input")})
	if err != nil {
		t.Fatalf("function call failed: %s", err)
	}
	if !result.RawEquals(cty.DynamicVal) {
		t.Errorf("got %s, want cty.DynamicVal", result.GoString())
	}
}

func TestEvalResource_PlannedUnknownObject(t *testing.T) {
	t.Parallel()

	// When evalResource produces a value for a resource, computed attributes
	// (not present in config) should be unknown — not null. This matches
	// upstream's plan phase and lets downstream expressions like
	// apko_build.this.sboms propagate unknowns instead of erroring on null.

	schema := &configschema.Block{
		Attributes: map[string]*configschema.Attribute{
			"ami":       {Type: cty.String, Required: true},
			"id":        {Type: cty.String, Computed: true},
			"image_ref": {Type: cty.String, Computed: true},
		},
	}

	mod := configs.ModuleFromStringForTesting(t, `
		resource "test_instance" "web" {
			ami = "abc-123"
		}
	`)
	rc := mod.ManagedResources["test_instance.web"]

	v := &Vertex{
		Kind:         KindResource,
		Module:       addrs.RootModuleInstance,
		Name:         "test_instance.web",
		ResourceCfg:  rc,
		Schema:       schema,
		ProviderAddr: addrs.NewDefaultProvider("test"),
		ResourceAddr: resAddr("test_instance", "web", addrs.NoKey),
	}

	graph := &Graph{
		Verts:   []Vertex{*v},
		Deps:    [][]ID{nil},
		RevDeps: [][]ID{nil},
	}
	results := NewResults(1)

	wc := &walkContext{
		graph:     graph,
		results:   results,
		pc:        newProviderCache(nil),
		config:    testConfig(mod, nil),
		hashIndex: NewHashIndex(),
		workDir:   t.TempDir(),
		workspace: "default",
		dryRun:    true,
	}

	data := &evalData{
		graph:     graph,
		results:   results,
		config:    wc.config,
		module:    addrs.RootModuleInstance,
		rootDir:   t.TempDir(),
		workDir:   wc.workDir,
		workspace: "default",
	}
	scope := &lang.Scope{
		Data:              data,
		ParseRef:          addrs.ParseRef,
		BaseDir:           data.workDir,
		ProviderFunctions: stubProviderFunction,
	}

	diags := wc.evalResource(t.Context(), scope, v, 0)
	if diags.HasErrors() {
		t.Fatalf("unexpected errors: %s", diags.Err())
	}

	val := results.Values[0]
	if val == cty.NilVal {
		t.Fatal("result is NilVal")
	}

	// "ami" was set in config — should be known.
	ami := val.GetAttr("ami")
	if !ami.IsKnown() {
		t.Error("ami should be known")
	}

	// "id" and "image_ref" are computed-only — should be unknown, not null.
	id := val.GetAttr("id")
	if id.IsNull() {
		t.Error("computed attribute 'id' should be unknown, not null")
	}
	if id.IsKnown() {
		t.Error("computed attribute 'id' should be unknown")
	}

	imageRef := val.GetAttr("image_ref")
	if imageRef.IsNull() {
		t.Error("computed attribute 'image_ref' should be unknown, not null")
	}
	if imageRef.IsKnown() {
		t.Error("computed attribute 'image_ref' should be unknown")
	}
}
