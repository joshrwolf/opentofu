// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package embedded

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/msgpack"

	"github.com/opentofu/opentofu/internal/providers"
)

// mockServer implements tfprotov6.ProviderServer for testing.
type mockServer struct {
	tfprotov6.ProviderServer

	schema     *tfprotov6.GetProviderSchemaResponse
	configured bool
	applied    *tfprotov6.ApplyResourceChangeRequest
}

func (m *mockServer) GetProviderSchema(_ context.Context, _ *tfprotov6.GetProviderSchemaRequest) (*tfprotov6.GetProviderSchemaResponse, error) {
	return m.schema, nil
}

func (m *mockServer) GetResourceIdentitySchemas(_ context.Context, _ *tfprotov6.GetResourceIdentitySchemasRequest) (*tfprotov6.GetResourceIdentitySchemasResponse, error) {
	return &tfprotov6.GetResourceIdentitySchemasResponse{}, nil
}

func (m *mockServer) ValidateProviderConfig(_ context.Context, _ *tfprotov6.ValidateProviderConfigRequest) (*tfprotov6.ValidateProviderConfigResponse, error) {
	return &tfprotov6.ValidateProviderConfigResponse{}, nil
}

func (m *mockServer) ConfigureProvider(_ context.Context, _ *tfprotov6.ConfigureProviderRequest) (*tfprotov6.ConfigureProviderResponse, error) {
	m.configured = true
	return &tfprotov6.ConfigureProviderResponse{}, nil
}

func (m *mockServer) ValidateResourceConfig(_ context.Context, _ *tfprotov6.ValidateResourceConfigRequest) (*tfprotov6.ValidateResourceConfigResponse, error) {
	return &tfprotov6.ValidateResourceConfigResponse{}, nil
}

func (m *mockServer) ApplyResourceChange(_ context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	m.applied = req

	// Echo back the planned state as new state, simulating a successful apply.
	return &tfprotov6.ApplyResourceChangeResponse{
		NewState: req.PlannedState,
		Private:  req.PlannedPrivate,
	}, nil
}

func (m *mockServer) ReadDataSource(_ context.Context, req *tfprotov6.ReadDataSourceRequest) (*tfprotov6.ReadDataSourceResponse, error) {
	// Return a simple computed state.
	ty := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"id":    tftypes.String,
		"input": tftypes.String,
	}}
	val := tftypes.NewValue(ty, map[string]tftypes.Value{
		"id":    tftypes.NewValue(tftypes.String, "computed-id"),
		"input": tftypes.NewValue(tftypes.String, "hello"),
	})
	dv, _ := tfprotov6.NewDynamicValue(ty, val)
	return &tfprotov6.ReadDataSourceResponse{
		State: &dv,
	}, nil
}

func (m *mockServer) StopProvider(_ context.Context, _ *tfprotov6.StopProviderRequest) (*tfprotov6.StopProviderResponse, error) {
	return &tfprotov6.StopProviderResponse{}, nil
}

func newTestSchema() *tfprotov6.GetProviderSchemaResponse {
	return &tfprotov6.GetProviderSchemaResponse{
		Provider: &tfprotov6.Schema{
			Block: &tfprotov6.SchemaBlock{
				Attributes: []*tfprotov6.SchemaAttribute{
					{Name: "token", Type: tftypes.String, Optional: true, Sensitive: true},
				},
			},
		},
		ResourceSchemas: map[string]*tfprotov6.Schema{
			"test_resource": {
				Version: 1,
				Block: &tfprotov6.SchemaBlock{
					Attributes: []*tfprotov6.SchemaAttribute{
						{Name: "id", Type: tftypes.String, Computed: true},
						{Name: "name", Type: tftypes.String, Required: true},
						{Name: "tags", Type: tftypes.Map{ElementType: tftypes.String}, Optional: true},
					},
				},
			},
		},
		DataSourceSchemas: map[string]*tfprotov6.Schema{
			"test_data": {
				Block: &tfprotov6.SchemaBlock{
					Attributes: []*tfprotov6.SchemaAttribute{
						{Name: "id", Type: tftypes.String, Computed: true},
						{Name: "input", Type: tftypes.String, Required: true},
					},
				},
			},
		},
	}
}

func TestGetProviderSchema(t *testing.T) {
	server := &mockServer{schema: newTestSchema()}
	p := NewProvider(server)
	ctx := context.Background()

	resp := p.GetProviderSchema(ctx)
	if resp.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %s", resp.Diagnostics.Err())
	}

	// Provider config schema
	if resp.Provider.Block == nil {
		t.Fatal("expected provider block schema")
	}
	tokenAttr, ok := resp.Provider.Block.Attributes["token"]
	if !ok {
		t.Fatal("expected 'token' attribute in provider schema")
	}
	if !tokenAttr.Sensitive {
		t.Error("expected 'token' to be sensitive")
	}
	if tokenAttr.Type != cty.String {
		t.Errorf("expected 'token' type cty.String, got %s", tokenAttr.Type.FriendlyName())
	}

	// Resource schema
	res, ok := resp.ResourceTypes["test_resource"]
	if !ok {
		t.Fatal("expected 'test_resource' in resource types")
	}
	if res.Version != 1 {
		t.Errorf("expected version 1, got %d", res.Version)
	}
	nameAttr := res.Block.Attributes["name"]
	if nameAttr == nil || !nameAttr.Required {
		t.Error("expected required 'name' attribute")
	}
	tagsAttr := res.Block.Attributes["tags"]
	if tagsAttr == nil || !tagsAttr.Optional {
		t.Error("expected optional 'tags' attribute")
	}
	if !tagsAttr.Type.IsMapType() {
		t.Errorf("expected map type for 'tags', got %s", tagsAttr.Type.FriendlyName())
	}

	// Data source schema
	ds, ok := resp.DataSources["test_data"]
	if !ok {
		t.Fatal("expected 'test_data' in data sources")
	}
	if ds.Block.Attributes["input"] == nil {
		t.Error("expected 'input' attribute in data source schema")
	}

	// Schema caching: second call should return same result.
	resp2 := p.GetProviderSchema(ctx)
	if resp2.Diagnostics.HasErrors() {
		t.Fatal("unexpected errors on second schema call")
	}
}

func TestConfigureProvider(t *testing.T) {
	server := &mockServer{schema: newTestSchema()}
	p := NewProvider(server)
	ctx := context.Background()

	resp := p.ConfigureProvider(ctx, providers.ConfigureProviderRequest{
		TerraformVersion: "1.0.0",
		Config: cty.ObjectVal(map[string]cty.Value{
			"token": cty.StringVal("secret"),
		}),
	})
	if resp.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %s", resp.Diagnostics.Err())
	}
	if !server.configured {
		t.Error("expected server.ConfigureProvider to be called")
	}
}

func TestApplyResourceChange(t *testing.T) {
	server := &mockServer{schema: newTestSchema()}
	p := NewProvider(server)
	ctx := context.Background()

	schema := p.GetProviderSchema(ctx)
	resTy := schema.ResourceTypes["test_resource"].Block.ImpliedType()

	planned := cty.ObjectVal(map[string]cty.Value{
		"id":   cty.StringVal("new-id"),
		"name": cty.StringVal("foo"),
		"tags": cty.MapVal(map[string]cty.Value{
			"env": cty.StringVal("prod"),
		}),
	})

	resp := p.ApplyResourceChange(ctx, providers.ApplyResourceChangeRequest{
		TypeName:       "test_resource",
		PriorState:     cty.NullVal(resTy),
		PlannedState:   planned,
		Config:         planned,
		PlannedPrivate: []byte("private-data"),
	})
	if resp.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %s", resp.Diagnostics.Err())
	}

	// Verify the server received the request.
	if server.applied == nil {
		t.Fatal("expected ApplyResourceChange to be called on server")
	}
	if server.applied.TypeName != "test_resource" {
		t.Errorf("expected TypeName 'test_resource', got %q", server.applied.TypeName)
	}

	// Verify the response round-tripped correctly.
	if resp.NewState.IsNull() {
		t.Fatal("expected non-null new state")
	}
	nameVal := resp.NewState.GetAttr("name")
	if nameVal.AsString() != "foo" {
		t.Errorf("expected name 'foo', got %q", nameVal.AsString())
	}
	if string(resp.Private) != "private-data" {
		t.Errorf("expected private data round-trip, got %q", resp.Private)
	}
}

func TestReadDataSource(t *testing.T) {
	server := &mockServer{schema: newTestSchema()}
	p := NewProvider(server)
	ctx := context.Background()

	schema := p.GetProviderSchema(ctx)
	dsTy := schema.DataSources["test_data"].Block.ImpliedType()

	resp := p.ReadDataSource(ctx, providers.ReadDataSourceRequest{
		TypeName: "test_data",
		Config: cty.ObjectVal(map[string]cty.Value{
			"id":    cty.NullVal(cty.String),
			"input": cty.StringVal("hello"),
		}),
	})
	if resp.Diagnostics.HasErrors() {
		t.Fatalf("unexpected errors: %s", resp.Diagnostics.Err())
	}

	if resp.State.IsNull() {
		t.Fatal("expected non-null state")
	}
	_ = dsTy // used for schema reference

	idVal := resp.State.GetAttr("id")
	if idVal.AsString() != "computed-id" {
		t.Errorf("expected id 'computed-id', got %q", idVal.AsString())
	}
}

func TestMsgpackRoundTrip(t *testing.T) {
	// Verify that cty->msgpack->tfprotov6.DynamicValue->msgpack->cty round-trips
	// correctly for complex types.
	ty := cty.Object(map[string]cty.Type{
		"str":  cty.String,
		"num":  cty.Number,
		"list": cty.List(cty.String),
		"map":  cty.Map(cty.Bool),
	})

	original := cty.ObjectVal(map[string]cty.Value{
		"str":  cty.StringVal("hello"),
		"num":  cty.NumberIntVal(42),
		"list": cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
		"map": cty.MapVal(map[string]cty.Value{
			"x": cty.True,
			"y": cty.False,
		}),
	})

	// Marshal via our helper
	dv, err := marshalValue(original, ty)
	if err != nil {
		t.Fatalf("marshalValue: %v", err)
	}

	// Unmarshal via our helper
	result, err := unmarshalValue(dv, ty)
	if err != nil {
		t.Fatalf("unmarshalValue: %v", err)
	}

	// Verify round-trip equality by re-marshaling and comparing bytes.
	origBytes, _ := msgpack.Marshal(original, ty)
	resultBytes, _ := msgpack.Marshal(result, ty)
	if string(origBytes) != string(resultBytes) {
		t.Error("round-trip produced different msgpack bytes")
	}
}

func TestTftypeToCtyType(t *testing.T) {
	tests := []struct {
		name     string
		input    tftypes.Type
		expected cty.Type
	}{
		{"string", tftypes.String, cty.String},
		{"number", tftypes.Number, cty.Number},
		{"bool", tftypes.Bool, cty.Bool},
		{"dynamic", tftypes.DynamicPseudoType, cty.DynamicPseudoType},
		{"list", tftypes.List{ElementType: tftypes.String}, cty.List(cty.String)},
		{"set", tftypes.Set{ElementType: tftypes.Number}, cty.Set(cty.Number)},
		{"map", tftypes.Map{ElementType: tftypes.Bool}, cty.Map(cty.Bool)},
		{"tuple", tftypes.Tuple{ElementTypes: []tftypes.Type{tftypes.String, tftypes.Number}}, cty.Tuple([]cty.Type{cty.String, cty.Number})},
		{"object", tftypes.Object{AttributeTypes: map[string]tftypes.Type{"a": tftypes.String, "b": tftypes.Number}}, cty.Object(map[string]cty.Type{"a": cty.String, "b": cty.Number})},
		{"nested", tftypes.List{ElementType: tftypes.Object{AttributeTypes: map[string]tftypes.Type{"id": tftypes.String}}}, cty.List(cty.Object(map[string]cty.Type{"id": cty.String}))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tftypeToCtyType(tt.input)
			if err != nil {
				t.Fatalf("tftypeToCtyType: %v", err)
			}
			if !result.Equals(tt.expected) {
				t.Errorf("expected %s, got %s", tt.expected.FriendlyName(), result.FriendlyName())
			}
		})
	}
}
