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

var ImagetestProviderSchema = &providers.GetProviderSchemaResponse{
	Provider: providers.Schema{
		Block: &configschema.Block{},
	},
	ResourceTypes: map[string]providers.Schema{
		"imagetest_harness_docker":   imagetestNamedSchema(),
		"imagetest_harness_k3s":      imagetestNamedSchema(),
		"imagetest_feature":          imagetestNamedSchema(),
		"imagetest_tests":            imagetestNamedSchema(),
		"imagetest_container_volume": imagetestNamedSchema(),
	},
	DataSources: map[string]providers.Schema{
		"imagetest_inventory": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"items": {Type: cty.List(cty.String), Optional: true, Computed: true},
				},
			},
		},
	},
}

func imagetestNamedSchema() providers.Schema {
	return providers.Schema{
		Block: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"id":   {Type: cty.String, Optional: true, Computed: true},
				"name": {Type: cty.String, Required: true},
			},
		},
	}
}

type ImagetestProvider struct {
	Provider *tofu.MockProvider
}

func NewImagetestProvider() *ImagetestProvider {
	provider := &ImagetestProvider{
		Provider: new(tofu.MockProvider),
	}

	provider.Provider.GetProviderSchemaResponse = ImagetestProviderSchema
	provider.Provider.ConfigureProviderFn = provider.ConfigureProvider
	provider.Provider.PlanResourceChangeFn = provider.PlanResourceChange
	provider.Provider.ApplyResourceChangeFn = provider.ApplyResourceChange
	provider.Provider.ReadDataSourceFn = provider.ReadDataSource

	return provider
}

func (provider *ImagetestProvider) ConfigureProvider(request providers.ConfigureProviderRequest) providers.ConfigureProviderResponse {
	return providers.ConfigureProviderResponse{}
}

func (provider *ImagetestProvider) ReadDataSource(request providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
	return providers.ReadDataSourceResponse{
		State: cty.ObjectVal(map[string]cty.Value{
			"items": cty.ListVal([]cty.Value{cty.StringVal("inventory")}),
		}),
	}
}

func (provider *ImagetestProvider) PlanResourceChange(request providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
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

func (provider *ImagetestProvider) ApplyResourceChange(request providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
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
