// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"fmt"

	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/providers"
)

// Provider is the built-in chofu provider. It exists only to supply the
// schema for chofu_exec resources — the chofu engine intercepts evaluation
// before any Plan/Apply methods are called.
type Provider struct{}

func NewProvider() providers.Interface {
	return &Provider{}
}

func (p *Provider) GetProviderSchema(_ context.Context) providers.GetProviderSchemaResponse {
	return providers.GetProviderSchemaResponse{
		Provider: providers.Schema{
			Block: &configschema.Block{},
		},
		ResourceTypes: map[string]providers.Schema{
			"chofu_exec": execResourceSchema(),
		},
	}
}

func (p *Provider) ValidateProviderConfig(_ context.Context, req providers.ValidateProviderConfigRequest) providers.ValidateProviderConfigResponse {
	return providers.ValidateProviderConfigResponse{PreparedConfig: req.Config}
}

func (p *Provider) ValidateResourceConfig(_ context.Context, req providers.ValidateResourceConfigRequest) providers.ValidateResourceConfigResponse {
	var resp providers.ValidateResourceConfigResponse
	if req.TypeName != "chofu_exec" {
		resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("unsupported resource type %q", req.TypeName))
	}
	return resp
}

func (p *Provider) ValidateDataResourceConfig(_ context.Context, _ providers.ValidateDataResourceConfigRequest) providers.ValidateDataResourceConfigResponse {
	return providers.ValidateDataResourceConfigResponse{}
}

func (p *Provider) ValidateEphemeralConfig(_ context.Context, _ providers.ValidateEphemeralConfigRequest) providers.ValidateEphemeralConfigResponse {
	return providers.ValidateEphemeralConfigResponse{}
}

func (p *Provider) ConfigureProvider(_ context.Context, _ providers.ConfigureProviderRequest) providers.ConfigureProviderResponse {
	return providers.ConfigureProviderResponse{}
}

// The methods below should never be called — the chofu engine intercepts
// chofu_exec resources before they reach the provider protocol. If they
// are called, it indicates a bug in the engine's interception logic.

func (p *Provider) ReadResource(_ context.Context, _ providers.ReadResourceRequest) providers.ReadResourceResponse {
	panic("chofu provider: ReadResource should not be called — the engine must intercept chofu_exec evaluation")
}

func (p *Provider) PlanResourceChange(_ context.Context, _ providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
	panic("chofu provider: PlanResourceChange should not be called — the engine must intercept chofu_exec evaluation")
}

func (p *Provider) ApplyResourceChange(_ context.Context, _ providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
	panic("chofu provider: ApplyResourceChange should not be called — the engine must intercept chofu_exec evaluation")
}

func (p *Provider) UpgradeResourceState(_ context.Context, _ providers.UpgradeResourceStateRequest) providers.UpgradeResourceStateResponse {
	return providers.UpgradeResourceStateResponse{}
}

func (p *Provider) UpgradeResourceIdentity(_ context.Context, _ providers.UpgradeResourceIdentityRequest) providers.UpgradeResourceIdentityResponse {
	return providers.UpgradeResourceIdentityResponse{}
}

func (p *Provider) ImportResourceState(_ context.Context, _ providers.ImportResourceStateRequest) providers.ImportResourceStateResponse {
	panic("chofu provider: ImportResourceState is not supported")
}

func (p *Provider) MoveResourceState(_ context.Context, _ providers.MoveResourceStateRequest) providers.MoveResourceStateResponse {
	return providers.MoveResourceStateResponse{}
}

func (p *Provider) ReadDataSource(_ context.Context, _ providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
	panic("chofu provider: no data sources")
}

func (p *Provider) OpenEphemeralResource(_ context.Context, _ providers.OpenEphemeralResourceRequest) providers.OpenEphemeralResourceResponse {
	panic("chofu provider: no ephemeral resources")
}

func (p *Provider) RenewEphemeralResource(_ context.Context, _ providers.RenewEphemeralResourceRequest) providers.RenewEphemeralResourceResponse {
	panic("chofu provider: no ephemeral resources")
}

func (p *Provider) CloseEphemeralResource(_ context.Context, _ providers.CloseEphemeralResourceRequest) providers.CloseEphemeralResourceResponse {
	panic("chofu provider: no ephemeral resources")
}

func (p *Provider) CallFunction(_ context.Context, r providers.CallFunctionRequest) providers.CallFunctionResponse {
	return providers.CallFunctionResponse{
		Error: fmt.Errorf("chofu provider has no function %q", r.Name),
	}
}

func (p *Provider) GetFunctions(_ context.Context) providers.GetFunctionsResponse {
	return providers.GetFunctionsResponse{}
}

func (p *Provider) Stop(_ context.Context) error { return nil }
func (p *Provider) Close(_ context.Context) error { return nil }
