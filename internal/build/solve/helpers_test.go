// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/build/catalog"
)

func TestBuildTargetCollectionValue(t *testing.T) {
	root := catalog.RootModule()
	tests := []struct {
		name    string
		shape   InstanceShape
		addrs   []catalog.Addr
		values  []cty.Value
		want    cty.Value
		wantErr bool
	}{
		{
			name:   "single",
			shape:  InstanceShapeSingle,
			addrs:  []catalog.Addr{catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.NoKey())},
			values: []cty.Value{cty.StringVal("hello")},
			want:   cty.StringVal("hello"),
		},
		{
			name:    "single requires exactly one",
			shape:   InstanceShapeSingle,
			addrs:   []catalog.Addr{},
			values:  []cty.Value{},
			wantErr: true,
		},
		{
			name:   "optional with value",
			shape:  InstanceShapeOptional,
			addrs:  []catalog.Addr{catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.NoKey())},
			values: []cty.Value{cty.StringVal("hello")},
			want:   cty.StringVal("hello"),
		},
		{
			name:   "optional empty",
			shape:  InstanceShapeOptional,
			addrs:  []catalog.Addr{},
			values: []cty.Value{},
			want:   cty.NilVal,
		},
		{
			name:  "list sorted by int key",
			shape: InstanceShapeList,
			addrs: []catalog.Addr{
				catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.IntKey(1)),
				catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.IntKey(0)),
			},
			values: []cty.Value{cty.StringVal("second"), cty.StringVal("first")},
			want:   cty.TupleVal([]cty.Value{cty.StringVal("first"), cty.StringVal("second")}),
		},
		{
			name:  "map keyed by string",
			shape: InstanceShapeMap,
			addrs: []catalog.Addr{
				catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.StringKey("alpha")),
				catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.StringKey("beta")),
			},
			values: []cty.Value{cty.StringVal("a"), cty.StringVal("b")},
			want: cty.ObjectVal(map[string]cty.Value{
				"alpha": cty.StringVal("a"),
				"beta":  cty.StringVal("b"),
			}),
		},
		{
			name:    "unsupported shape",
			shape:   InstanceShape("unknown"),
			addrs:   []catalog.Addr{},
			values:  []cty.Value{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, diags := buildTargetCollectionValue(tt.shape, tt.addrs, tt.values)
			if (diags.HasErrors()) != tt.wantErr {
				t.Fatalf("buildTargetCollectionValue() error = %v, wantErr %v", diags.Err(), tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.want == cty.NilVal {
				if got != cty.NilVal {
					t.Fatalf("expected NilVal, got %s", got.GoString())
				}
				return
			}
			if !got.RawEquals(tt.want) {
				t.Fatalf("wrong value: got %s want %s", got.GoString(), tt.want.GoString())
			}
		})
	}
}

func TestBuildModuleCollectionValue(t *testing.T) {
	root := catalog.RootModule()
	tests := []struct {
		name    string
		shape   InstanceShape
		modules []catalog.ModulePath
		values  []cty.Value
		want    cty.Value
		wantErr bool
	}{
		{
			name:    "single",
			shape:   InstanceShapeSingle,
			modules: []catalog.ModulePath{root.Child("a", catalog.NoKey())},
			values:  []cty.Value{cty.StringVal("hello")},
			want:    cty.StringVal("hello"),
		},
		{
			name:    "single requires exactly one",
			shape:   InstanceShapeSingle,
			modules: []catalog.ModulePath{},
			values:  []cty.Value{},
			wantErr: true,
		},
		{
			name:  "list sorted by int key",
			shape: InstanceShapeList,
			modules: []catalog.ModulePath{
				root.Child("a", catalog.IntKey(1)),
				root.Child("a", catalog.IntKey(0)),
			},
			values: []cty.Value{cty.StringVal("second"), cty.StringVal("first")},
			want:   cty.TupleVal([]cty.Value{cty.StringVal("first"), cty.StringVal("second")}),
		},
		{
			name:  "map keyed by string",
			shape: InstanceShapeMap,
			modules: []catalog.ModulePath{
				root.Child("a", catalog.StringKey("x")),
				root.Child("a", catalog.StringKey("y")),
			},
			values: []cty.Value{cty.StringVal("xv"), cty.StringVal("yv")},
			want: cty.ObjectVal(map[string]cty.Value{
				"x": cty.StringVal("xv"),
				"y": cty.StringVal("yv"),
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, diags := buildModuleCollectionValue(tt.shape, tt.modules, tt.values)
			if (diags.HasErrors()) != tt.wantErr {
				t.Fatalf("buildModuleCollectionValue() error = %v, wantErr %v", diags.Err(), tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !got.RawEquals(tt.want) {
				t.Fatalf("wrong value: got %s want %s", got.GoString(), tt.want.GoString())
			}
		})
	}
}

func TestRewriteModulePrefix(t *testing.T) {
	root := catalog.RootModule()
	tests := []struct {
		name           string
		addr           catalog.Addr
		declPrefix     catalog.ModulePath
		concretePrefix catalog.ModulePath
		wantModule     catalog.ModulePath
	}{
		{
			name:           "same prefix is identity",
			addr:           catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.NoKey()),
			declPrefix:     root,
			concretePrefix: root,
			wantModule:     root,
		},
		{
			name:           "rewrite to keyed child",
			addr:           catalog.ResourceAddr(root.Child("child", catalog.NoKey()), catalog.TargetKindResource, "t", "a", catalog.NoKey()),
			declPrefix:     root.Child("child", catalog.NoKey()),
			concretePrefix: root.Child("child", catalog.StringKey("x")),
			wantModule:     root.Child("child", catalog.StringKey("x")),
		},
		{
			name:           "preserves deeper steps",
			addr:           catalog.ResourceAddr(root.Child("a", catalog.NoKey()).Child("b", catalog.NoKey()), catalog.TargetKindResource, "t", "x", catalog.NoKey()),
			declPrefix:     root.Child("a", catalog.NoKey()),
			concretePrefix: root.Child("a", catalog.IntKey(0)),
			wantModule:     root.Child("a", catalog.IntKey(0)).Child("b", catalog.NoKey()),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteModulePrefix(tt.addr, tt.declPrefix, tt.concretePrefix)
			if got.Module.Identity() != tt.wantModule.Identity() {
				t.Fatalf("wrong module: got %s want %s", got.Module, tt.wantModule)
			}
		})
	}
}

func TestRefMatchesSelected(t *testing.T) {
	root := catalog.RootModule()
	tests := []struct {
		name     string
		ref      catalog.Addr
		selected catalog.Addr
		want     bool
	}{
		{
			name:     "exact match",
			ref:      catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.NoKey()),
			selected: catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.NoKey()),
			want:     true,
		},
		{
			name:     "declaration ref matches specific instance",
			ref:      catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.NoKey()),
			selected: catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.StringKey("x")),
			want:     true,
		},
		{
			name:     "different name",
			ref:      catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.NoKey()),
			selected: catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "b", catalog.NoKey()),
			want:     false,
		},
		{
			name:     "different kind",
			ref:      catalog.ResourceAddr(root, catalog.TargetKindResource, "t", "a", catalog.NoKey()),
			selected: catalog.ResourceAddr(root, catalog.TargetKindData, "t", "a", catalog.NoKey()),
			want:     false,
		},
		{
			name:     "different type",
			ref:      catalog.ResourceAddr(root, catalog.TargetKindResource, "t1", "a", catalog.NoKey()),
			selected: catalog.ResourceAddr(root, catalog.TargetKindResource, "t2", "a", catalog.NoKey()),
			want:     false,
		},
		{
			name:     "module declaration matches module",
			ref:      catalog.ModuleAddr(root.Child("child", catalog.NoKey())),
			selected: catalog.ModuleAddr(root.Child("child", catalog.NoKey())),
			want:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refMatchesSelected(tt.ref, tt.selected); got != tt.want {
				t.Fatalf("refMatchesSelected() = %v, want %v", got, tt.want)
			}
		})
	}
}
