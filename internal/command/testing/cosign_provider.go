// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package testing

import (
	"crypto/sha256"
	"fmt"

	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tofu"
	"github.com/zclconf/go-cty/cty"
)

var CosignProviderSchema = &providers.GetProviderSchemaResponse{
	Provider: providers.Schema{
		Block: &configschema.Block{},
	},
	ResourceTypes: map[string]providers.Schema{
		"cosign_copy": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"id":          {Type: cty.String, Optional: true, Computed: true},
					"source":      {Type: cty.String, Required: true},
					"destination": {Type: cty.String, Required: true},
				},
			},
		},
		"cosign_sign": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"id":    {Type: cty.String, Optional: true, Computed: true},
					"image": {Type: cty.String, Required: true},
				},
			},
		},
		"cosign_attest": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"id":    {Type: cty.String, Optional: true, Computed: true},
					"image": {Type: cty.String, Required: true},
				},
			},
		},
	},
}

type CosignProvider struct {
	Provider *tofu.MockProvider
}

func NewCosignProvider() *CosignProvider {
	provider := &CosignProvider{
		Provider: new(tofu.MockProvider),
	}

	provider.Provider.GetProviderSchemaResponse = CosignProviderSchema
	provider.Provider.ConfigureProviderFn = provider.ConfigureProvider
	provider.Provider.PlanResourceChangeFn = provider.PlanResourceChange
	provider.Provider.ApplyResourceChangeFn = provider.ApplyResourceChange

	return provider
}

func (provider *CosignProvider) ConfigureProvider(request providers.ConfigureProviderRequest) providers.ConfigureProviderResponse {
	return providers.ConfigureProviderResponse{}
}

func (provider *CosignProvider) PlanResourceChange(request providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
	if request.ProposedNewState.IsNull() {
		return providers.PlanResourceChangeResponse{
			PlannedState: request.ProposedNewState,
		}
	}

	resource := request.ProposedNewState
	vals := resource.AsValueMap()
	if id := vals["id"]; id.IsNull() || !id.IsKnown() {
		vals["id"] = cty.UnknownVal(cty.String)
	}
	return providers.PlanResourceChangeResponse{
		PlannedState: cty.ObjectVal(vals),
	}
}

func (provider *CosignProvider) ApplyResourceChange(request providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
	if request.PlannedState.IsNull() {
		return providers.ApplyResourceChangeResponse{
			NewState: request.PlannedState,
		}
	}

	resource := request.PlannedState
	sum := sha256.Sum256([]byte(resource.GoString()))
	digest := fmt.Sprintf("%x", sum[:])

	vals := resource.AsValueMap()
	vals["id"] = cty.StringVal(digest)

	return providers.ApplyResourceChangeResponse{
		NewState: cty.ObjectVal(vals),
	}
}
