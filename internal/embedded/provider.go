// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// Package embedded provides an adapter that wraps a tfprotov6.ProviderServer
// as a providers.Interface, enabling in-process provider execution without
// gRPC subprocess overhead. This is the foundation for compiling providers
// directly into the chofu binary.
//
// The adapter bridges two type systems via msgpack: OpenTofu's cty.Value and
// the provider framework's tftypes.Value use identical msgpack wire formats,
// so values pass through as raw bytes without intermediate conversion.
package embedded

import (
	"context"
	"fmt"
	"sync"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// clientCapabilities is sent on requests that accept it, telling the provider
// what features this client supports.
var clientCapabilities = &tfprotov6.ConfigureProviderClientCapabilities{
	DeferralAllowed: false,
}

// Provider wraps a tfprotov6.ProviderServer as a providers.Interface,
// enabling in-process provider execution without gRPC or subprocess overhead.
type Provider struct {
	server tfprotov6.ProviderServer

	// Schema is cached after first successful fetch. The mutex protects
	// against concurrent first-access races while allowing retry on failure
	// (unlike sync.Once which permanently caches errors).
	schemaMu    sync.Mutex
	schemaCache *providers.GetProviderSchemaResponse

	// impliedTypes caches ImpliedType() results per type name to avoid
	// repeated schema tree walks on the hot path.
	impliedTypes map[string]cty.Type
}

var _ providers.Interface = (*Provider)(nil)

// NewProvider creates a new embedded provider adapter wrapping the given
// in-process tfprotov6.ProviderServer.
func NewProvider(server tfprotov6.ProviderServer) *Provider {
	return &Provider{server: server}
}

// GetProviderSchema fetches the provider's schema from the in-process server,
// caching the result after the first successful fetch.
func (p *Provider) GetProviderSchema(ctx context.Context) providers.GetProviderSchemaResponse {
	p.schemaMu.Lock()
	defer p.schemaMu.Unlock()

	if p.schemaCache != nil {
		return *p.schemaCache
	}

	resp := p.getProviderSchema(ctx)
	if !resp.Diagnostics.HasErrors() {
		p.schemaCache = &resp
		p.impliedTypes = make(map[string]cty.Type)
	}
	return resp
}

func (p *Provider) getProviderSchema(ctx context.Context) (resp providers.GetProviderSchemaResponse) {
	resp.ResourceTypes = make(map[string]providers.Schema)
	resp.DataSources = make(map[string]providers.Schema)
	resp.EphemeralResources = make(map[string]providers.Schema)
	resp.Functions = make(map[string]providers.FunctionSpec)

	protoResp, err := p.server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("getting provider schema: %w", err))
		return resp
	}
	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	if protoResp.Provider == nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("missing provider schema"))
		return resp
	}

	provSchema, err := schemaToProviders(protoResp.Provider)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("converting provider schema: %w", err))
		return resp
	}
	provSchema.Block.Ephemeral = true
	resp.Provider = provSchema

	if protoResp.ProviderMeta != nil {
		metaSchema, err := schemaToProviders(protoResp.ProviderMeta)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("converting provider meta schema: %w", err))
			return resp
		}
		resp.ProviderMeta = metaSchema
	}

	for name, s := range protoResp.ResourceSchemas {
		rs, err := schemaToProviders(s)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("converting resource schema %q: %w", name, err))
			return resp
		}
		resp.ResourceTypes[name] = rs
	}

	for name, s := range protoResp.DataSourceSchemas {
		ds, err := schemaToProviders(s)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("converting data source schema %q: %w", name, err))
			return resp
		}
		resp.DataSources[name] = ds
	}

	for name, f := range protoResp.Functions {
		fs, err := functionToSpec(f)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("converting function %q: %w", name, err))
			return resp
		}
		resp.Functions[name] = fs
	}

	for name, s := range protoResp.EphemeralResourceSchemas {
		es, err := schemaToProviders(s)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("converting ephemeral resource schema %q: %w", name, err))
			return resp
		}
		es.Block.Ephemeral = true
		resp.EphemeralResources[name] = es
	}

	// Identity schemas are optional; failures here should not block the provider.
	idResp, err := p.server.GetResourceIdentitySchemas(ctx, &tfprotov6.GetResourceIdentitySchemasRequest{})
	if err == nil && idResp != nil && len(convertDiagnostics(idResp.Diagnostics)) == 0 {
		for name, idSchema := range idResp.IdentitySchemas {
			if resSchema, ok := resp.ResourceTypes[name]; ok {
				ids, err := identitySchemaToProviders(idSchema)
				if err == nil && ids != nil {
					resSchema.IdentitySchema = ids.Body
					resSchema.IdentitySchemaVersion = ids.Version
					resp.ResourceTypes[name] = resSchema
				}
			}
		}
	}

	if protoResp.ServerCapabilities != nil {
		resp.ServerCapabilities.PlanDestroy = protoResp.ServerCapabilities.PlanDestroy
		resp.ServerCapabilities.GetProviderSchemaOptional = protoResp.ServerCapabilities.GetProviderSchemaOptional
	}

	return resp
}

// Schema lookup helpers. Each returns the implied cty.Type for marshaling.
// ImpliedType() results are cached to avoid repeated schema tree walks on
// the hot path (called per-resource during build).

func (p *Provider) cachedImpliedType(key string, block *configschema.Block) cty.Type {
	// Caller must hold schemaMu or schema must already be cached (immutable after that).
	if ty, ok := p.impliedTypes[key]; ok {
		return ty
	}
	ty := block.ImpliedType()
	p.impliedTypes[key] = ty
	return ty
}

func (p *Provider) providerType(ctx context.Context) (cty.Type, tfdiags.Diagnostics) {
	schema := p.GetProviderSchema(ctx)
	if schema.Diagnostics.HasErrors() {
		return cty.NilType, schema.Diagnostics
	}
	return p.cachedImpliedType("provider:", schema.Provider.Block), nil
}

func (p *Provider) providerMetaType(ctx context.Context) cty.Type {
	schema := p.GetProviderSchema(ctx)
	if schema.ProviderMeta.Block == nil {
		return cty.NilType
	}
	return p.cachedImpliedType("meta:", schema.ProviderMeta.Block)
}

func (p *Provider) resourceType(ctx context.Context, typeName string) (cty.Type, tfdiags.Diagnostics) {
	schema := p.GetProviderSchema(ctx)
	if schema.Diagnostics.HasErrors() {
		return cty.NilType, schema.Diagnostics
	}
	s, ok := schema.ResourceTypes[typeName]
	if !ok {
		var diags tfdiags.Diagnostics
		diags = diags.Append(fmt.Errorf("unknown resource type %q", typeName))
		return cty.NilType, diags
	}
	return p.cachedImpliedType("r:"+typeName, s.Block), nil
}

func (p *Provider) dataSourceType(ctx context.Context, typeName string) (cty.Type, tfdiags.Diagnostics) {
	schema := p.GetProviderSchema(ctx)
	if schema.Diagnostics.HasErrors() {
		return cty.NilType, schema.Diagnostics
	}
	s, ok := schema.DataSources[typeName]
	if !ok {
		var diags tfdiags.Diagnostics
		diags = diags.Append(fmt.Errorf("unknown data source type %q", typeName))
		return cty.NilType, diags
	}
	return p.cachedImpliedType("d:"+typeName, s.Block), nil
}

// marshalProviderMeta is a helper that marshals the ProviderMeta field if present.
func (p *Provider) marshalProviderMeta(ctx context.Context, meta cty.Value) (*tfprotov6.DynamicValue, error) {
	metaTy := p.providerMetaType(ctx)
	if metaTy == cty.NilType {
		return nil, nil
	}
	if meta == (cty.Value{}) || meta.IsNull() {
		return nil, nil
	}
	return marshalValue(meta, metaTy)
}

func (p *Provider) ValidateProviderConfig(ctx context.Context, r providers.ValidateProviderConfigRequest) (resp providers.ValidateProviderConfigResponse) {
	ty, diags := p.providerType(ctx)
	if diags.HasErrors() {
		resp.Diagnostics = diags
		return resp
	}

	config, err := marshalValue(r.Config, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoResp, err := p.server.ValidateProviderConfig(ctx, &tfprotov6.ValidateProviderConfigRequest{
		Config: config,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("validating provider config: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *Provider) ValidateResourceConfig(ctx context.Context, r providers.ValidateResourceConfigRequest) (resp providers.ValidateResourceConfigResponse) {
	ty, diags := p.resourceType(ctx, r.TypeName)
	if diags.HasErrors() {
		resp.Diagnostics = diags
		return resp
	}

	config, err := marshalValue(r.Config, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoResp, err := p.server.ValidateResourceConfig(ctx, &tfprotov6.ValidateResourceConfigRequest{
		TypeName: r.TypeName,
		Config:   config,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("validating resource config: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *Provider) ValidateDataResourceConfig(ctx context.Context, r providers.ValidateDataResourceConfigRequest) (resp providers.ValidateDataResourceConfigResponse) {
	ty, diags := p.dataSourceType(ctx, r.TypeName)
	if diags.HasErrors() {
		resp.Diagnostics = diags
		return resp
	}

	config, err := marshalValue(r.Config, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoResp, err := p.server.ValidateDataResourceConfig(ctx, &tfprotov6.ValidateDataResourceConfigRequest{
		TypeName: r.TypeName,
		Config:   config,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("validating data resource config: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *Provider) ValidateEphemeralConfig(ctx context.Context, r providers.ValidateEphemeralConfigRequest) (resp providers.ValidateEphemeralConfigResponse) {
	schema := p.GetProviderSchema(ctx)
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}
	s, ok := schema.EphemeralResources[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown ephemeral resource type %q", r.TypeName))
		return resp
	}

	config, err := marshalValue(r.Config, s.Block.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoResp, err := p.server.ValidateEphemeralResourceConfig(ctx, &tfprotov6.ValidateEphemeralResourceConfigRequest{
		TypeName: r.TypeName,
		Config:   config,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("validating ephemeral config: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *Provider) ConfigureProvider(ctx context.Context, r providers.ConfigureProviderRequest) (resp providers.ConfigureProviderResponse) {
	ty, diags := p.providerType(ctx)
	if diags.HasErrors() {
		resp.Diagnostics = diags
		return resp
	}

	config, err := marshalValue(r.Config, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoResp, err := p.server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{
		TerraformVersion:   r.TerraformVersion,
		Config:             config,
		ClientCapabilities: clientCapabilities,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("configuring provider: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *Provider) UpgradeResourceState(ctx context.Context, r providers.UpgradeResourceStateRequest) (resp providers.UpgradeResourceStateResponse) {
	ty, diags := p.resourceType(ctx, r.TypeName)
	if diags.HasErrors() {
		resp.Diagnostics = diags
		return resp
	}

	protoResp, err := p.server.UpgradeResourceState(ctx, &tfprotov6.UpgradeResourceStateRequest{
		TypeName: r.TypeName,
		Version:  int64(r.Version),
		RawState: &tfprotov6.RawState{
			JSON:    r.RawStateJSON,
			Flatmap: r.RawStateFlatmap,
		},
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("upgrading resource state: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	val, err := unmarshalValue(protoResp.UpgradedState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.UpgradedState = val
	return resp
}

func (p *Provider) UpgradeResourceIdentity(ctx context.Context, r providers.UpgradeResourceIdentityRequest) (resp providers.UpgradeResourceIdentityResponse) {
	schema := p.GetProviderSchema(ctx)
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}

	protoResp, err := p.server.UpgradeResourceIdentity(ctx, &tfprotov6.UpgradeResourceIdentityRequest{
		TypeName: r.TypeName,
		Version:  r.Version,
		RawIdentity: &tfprotov6.RawState{
			JSON: r.RawIdentityJSON,
		},
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("upgrading resource identity: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	if protoResp.UpgradedIdentity != nil && protoResp.UpgradedIdentity.IdentityData != nil {
		if resSchema, ok := schema.ResourceTypes[r.TypeName]; ok && resSchema.IdentitySchema != nil {
			idType := resSchema.IdentitySchema.ImpliedType()
			val, err := unmarshalValue(protoResp.UpgradedIdentity.IdentityData, idType)
			if err != nil {
				resp.Diagnostics = resp.Diagnostics.Append(err)
				return resp
			}
			resp.UpgradedIdentity = val
		}
	}

	return resp
}

func (p *Provider) ReadResource(ctx context.Context, r providers.ReadResourceRequest) (resp providers.ReadResourceResponse) {
	ty, diags := p.resourceType(ctx, r.TypeName)
	if diags.HasErrors() {
		resp.Diagnostics = diags
		return resp
	}

	currentState, err := marshalValue(r.PriorState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &tfprotov6.ReadResourceRequest{
		TypeName:     r.TypeName,
		CurrentState: currentState,
		Private:      r.Private,
	}

	if pm, err := p.marshalProviderMeta(ctx, r.ProviderMeta); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	} else if pm != nil {
		protoReq.ProviderMeta = pm
	}

	protoResp, err := p.server.ReadResource(ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("reading resource: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	val, err := unmarshalValue(protoResp.NewState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.NewState = val
	resp.Private = protoResp.Private
	return resp
}

func (p *Provider) PlanResourceChange(ctx context.Context, r providers.PlanResourceChangeRequest) (resp providers.PlanResourceChangeResponse) {
	ty, diags := p.resourceType(ctx, r.TypeName)
	if diags.HasErrors() {
		resp.Diagnostics = diags
		return resp
	}

	priorState, err := marshalValue(r.PriorState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	proposedNew, err := marshalValue(r.ProposedNewState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	config, err := marshalValue(r.Config, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &tfprotov6.PlanResourceChangeRequest{
		TypeName:         r.TypeName,
		PriorState:       priorState,
		ProposedNewState: proposedNew,
		Config:           config,
		PriorPrivate:     r.PriorPrivate,
	}

	if pm, err := p.marshalProviderMeta(ctx, r.ProviderMeta); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	} else if pm != nil {
		protoReq.ProviderMeta = pm
	}

	protoResp, err := p.server.PlanResourceChange(ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("planning resource change: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	val, err := unmarshalValue(protoResp.PlannedState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.PlannedState = val
	resp.PlannedPrivate = protoResp.PlannedPrivate

	for _, ap := range protoResp.RequiresReplace {
		resp.RequiresReplace = append(resp.RequiresReplace, attributePathToPath(ap))
	}

	return resp
}

func (p *Provider) ApplyResourceChange(ctx context.Context, r providers.ApplyResourceChangeRequest) (resp providers.ApplyResourceChangeResponse) {
	// Prevent context cancellation from aborting a partially-applied resource.
	// The provider should be stopped gracefully via Stop() instead.
	ctx = context.WithoutCancel(ctx)

	ty, diags := p.resourceType(ctx, r.TypeName)
	if diags.HasErrors() {
		resp.Diagnostics = diags
		return resp
	}

	priorState, err := marshalValue(r.PriorState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	plannedState, err := marshalValue(r.PlannedState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	config, err := marshalValue(r.Config, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &tfprotov6.ApplyResourceChangeRequest{
		TypeName:       r.TypeName,
		PriorState:     priorState,
		PlannedState:   plannedState,
		Config:         config,
		PlannedPrivate: r.PlannedPrivate,
	}

	if pm, err := p.marshalProviderMeta(ctx, r.ProviderMeta); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	} else if pm != nil {
		protoReq.ProviderMeta = pm
	}

	protoResp, err := p.server.ApplyResourceChange(ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("applying resource change: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	val, err := unmarshalValue(protoResp.NewState, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.NewState = val
	resp.Private = protoResp.Private
	return resp
}

func (p *Provider) ImportResourceState(ctx context.Context, r providers.ImportResourceStateRequest) (resp providers.ImportResourceStateResponse) {
	protoResp, err := p.server.ImportResourceState(ctx, &tfprotov6.ImportResourceStateRequest{
		TypeName: r.TypeName,
		ID:       r.Target.ID,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("importing resource state: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	for _, imported := range protoResp.ImportedResources {
		ty, d := p.resourceType(ctx, imported.TypeName)
		if d.HasErrors() {
			resp.Diagnostics = resp.Diagnostics.Append(d)
			return resp
		}

		state, err := unmarshalValue(imported.State, ty)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}

		resp.ImportedResources = append(resp.ImportedResources, providers.ImportedResource{
			TypeName: imported.TypeName,
			State:    state,
			Private:  imported.Private,
		})
	}

	return resp
}

func (p *Provider) MoveResourceState(ctx context.Context, r providers.MoveResourceStateRequest) (resp providers.MoveResourceStateResponse) {
	protoResp, err := p.server.MoveResourceState(ctx, &tfprotov6.MoveResourceStateRequest{
		SourceProviderAddress: r.SourceProviderAddress,
		SourceTypeName:        r.SourceTypeName,
		SourceSchemaVersion:   int64(r.SourceSchemaVersion),
		SourceState: &tfprotov6.RawState{
			JSON:    r.SourceStateJSON,
			Flatmap: r.SourceStateFlatmap,
		},
		SourcePrivate:  r.SourcePrivate,
		TargetTypeName: r.TargetTypeName,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("moving resource state: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	if protoResp.TargetState != nil {
		ty, d := p.resourceType(ctx, r.TargetTypeName)
		if d.HasErrors() {
			resp.Diagnostics = resp.Diagnostics.Append(d)
			return resp
		}
		val, err := unmarshalValue(protoResp.TargetState, ty)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		resp.TargetState = val
	}
	resp.TargetPrivate = protoResp.TargetPrivate
	return resp
}

func (p *Provider) ReadDataSource(ctx context.Context, r providers.ReadDataSourceRequest) (resp providers.ReadDataSourceResponse) {
	ty, diags := p.dataSourceType(ctx, r.TypeName)
	if diags.HasErrors() {
		resp.Diagnostics = diags
		return resp
	}

	config, err := marshalValue(r.Config, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoReq := &tfprotov6.ReadDataSourceRequest{
		TypeName: r.TypeName,
		Config:   config,
	}

	if pm, err := p.marshalProviderMeta(ctx, r.ProviderMeta); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	} else if pm != nil {
		protoReq.ProviderMeta = pm
	}

	protoResp, err := p.server.ReadDataSource(ctx, protoReq)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("reading data source: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	val, err := unmarshalValue(protoResp.State, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.State = val
	return resp
}

func (p *Provider) OpenEphemeralResource(ctx context.Context, r providers.OpenEphemeralResourceRequest) (resp providers.OpenEphemeralResourceResponse) {
	schema := p.GetProviderSchema(ctx)
	if schema.Diagnostics.HasErrors() {
		resp.Diagnostics = schema.Diagnostics
		return resp
	}
	s, ok := schema.EphemeralResources[r.TypeName]
	if !ok {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unknown ephemeral resource type %q", r.TypeName))
		return resp
	}
	ty := s.Block.ImpliedType()

	config, err := marshalValue(r.Config, ty)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	protoResp, err := p.server.OpenEphemeralResource(ctx, &tfprotov6.OpenEphemeralResourceRequest{
		TypeName: r.TypeName,
		Config:   config,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("opening ephemeral resource: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	if resp.Diagnostics.HasErrors() {
		return resp
	}

	if protoResp.Result != nil {
		val, err := unmarshalValue(protoResp.Result, ty)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
		resp.Result = val
	}
	resp.Private = protoResp.Private
	if !protoResp.RenewAt.IsZero() {
		t := protoResp.RenewAt
		resp.RenewAt = &t
	}
	return resp
}

func (p *Provider) RenewEphemeralResource(ctx context.Context, r providers.RenewEphemeralResourceRequest) (resp providers.RenewEphemeralResourceResponse) {
	protoResp, err := p.server.RenewEphemeralResource(ctx, &tfprotov6.RenewEphemeralResourceRequest{
		TypeName: r.TypeName,
		Private:  r.Private,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("renewing ephemeral resource: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	resp.Private = protoResp.Private
	if !protoResp.RenewAt.IsZero() {
		t := protoResp.RenewAt
		resp.RenewAt = &t
	}
	return resp
}

func (p *Provider) CloseEphemeralResource(ctx context.Context, r providers.CloseEphemeralResourceRequest) (resp providers.CloseEphemeralResourceResponse) {
	protoResp, err := p.server.CloseEphemeralResource(ctx, &tfprotov6.CloseEphemeralResourceRequest{
		TypeName: r.TypeName,
		Private:  r.Private,
	})
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("closing ephemeral resource: %w", err))
		return resp
	}

	resp.Diagnostics = resp.Diagnostics.Append(convertDiagnostics(protoResp.Diagnostics))
	return resp
}

func (p *Provider) GetFunctions(ctx context.Context) (resp providers.GetFunctionsResponse) {
	schema := p.GetProviderSchema(ctx)
	resp.Functions = schema.Functions
	resp.Diagnostics = schema.Diagnostics
	return resp
}

func (p *Provider) CallFunction(ctx context.Context, r providers.CallFunctionRequest) (resp providers.CallFunctionResponse) {
	schema := p.GetProviderSchema(ctx)
	if schema.Diagnostics.HasErrors() {
		resp.Error = schema.Diagnostics.Err()
		return resp
	}

	spec, ok := schema.Functions[r.Name]
	if !ok {
		resp.Error = fmt.Errorf("unknown function %q", r.Name)
		return resp
	}

	args := make([]*tfprotov6.DynamicValue, len(r.Arguments))
	for i, arg := range r.Arguments {
		var argTy cty.Type
		if i < len(spec.Parameters) {
			argTy = spec.Parameters[i].Type
		} else if spec.VariadicParameter != nil {
			argTy = spec.VariadicParameter.Type
		} else {
			resp.Error = fmt.Errorf("too many arguments for function %q", r.Name)
			return resp
		}

		dv, err := marshalValue(arg, argTy)
		if err != nil {
			resp.Error = fmt.Errorf("marshaling argument %d: %w", i, err)
			return resp
		}
		args[i] = dv
	}

	protoResp, err := p.server.CallFunction(ctx, &tfprotov6.CallFunctionRequest{
		Name:      r.Name,
		Arguments: args,
	})
	if err != nil {
		resp.Error = fmt.Errorf("calling function: %w", err)
		return resp
	}

	if protoResp.Error != nil {
		callErr := &providers.CallFunctionArgumentError{
			Text: protoResp.Error.Text,
		}
		if protoResp.Error.FunctionArgument != nil {
			callErr.FunctionArgument = int(*protoResp.Error.FunctionArgument)
		}
		resp.Error = callErr
		return resp
	}

	if protoResp.Result != nil {
		val, err := unmarshalValue(protoResp.Result, spec.Return)
		if err != nil {
			resp.Error = fmt.Errorf("unmarshaling function result: %w", err)
			return resp
		}
		resp.Result = val
	}

	return resp
}

func (p *Provider) Stop(_ context.Context) error {
	// StopProvider signals graceful cancellation of in-flight operations.
	// For in-process providers this is less critical than for subprocesses,
	// but we honor the contract.
	resp, err := p.server.StopProvider(context.Background(), &tfprotov6.StopProviderRequest{})
	if err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("stopping provider: %s", resp.Error)
	}
	return nil
}

func (p *Provider) Close(_ context.Context) error {
	// In-process providers have no subprocess to kill. Nothing to do.
	return nil
}
