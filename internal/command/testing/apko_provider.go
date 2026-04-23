// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package testing

import (
	"crypto/sha256"
	"fmt"

	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tofu"
	"github.com/zclconf/go-cty/cty"
)

var ApkoProviderSchema = &providers.GetProviderSchemaResponse{
	Provider: providers.Schema{
		Block: &configschema.Block{},
	},
	ResourceTypes: map[string]providers.Schema{
		"apko_build": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"id":        {Type: cty.String, Optional: true, Computed: true},
					"repo":      {Type: cty.String, Required: true},
					"config":    {Type: cty.String, Required: true},
					"image_ref": {Type: cty.String, Optional: true, Computed: true},
				},
			},
		},
		"apko_build_raw": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"id":          {Type: cty.String, Optional: true, Computed: true},
					"repo":        {Type: cty.String, Required: true},
					"configs_raw": {Type: cty.Map(cty.String), Required: true},
					"image_ref":   {Type: cty.String, Optional: true, Computed: true},
				},
			},
		},
	},
	DataSources: map[string]providers.Schema{
		"apko_config": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"config_contents": {Type: cty.String, Required: true},
					"config":          {Type: cty.String, Optional: true, Computed: true},
				},
			},
		},
	},
}

type ApkoProvider struct {
	Provider *tofu.MockProvider
}

func NewApkoProvider() *ApkoProvider {
	provider := &ApkoProvider{
		Provider: new(tofu.MockProvider),
	}

	provider.Provider.GetProviderSchemaResponse = ApkoProviderSchema
	provider.Provider.ConfigureProviderFn = provider.ConfigureProvider
	provider.Provider.PlanResourceChangeFn = provider.PlanResourceChange
	provider.Provider.ApplyResourceChangeFn = provider.ApplyResourceChange
	provider.Provider.ReadDataSourceFn = provider.ReadDataSource

	return provider
}

func (provider *ApkoProvider) ConfigureProvider(request providers.ConfigureProviderRequest) providers.ConfigureProviderResponse {
	return providers.ConfigureProviderResponse{}
}

func (provider *ApkoProvider) ReadDataSource(request providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
	switch request.TypeName {
	case "apko_config":
		config := request.Config.GetAttr("config_contents")
		return providers.ReadDataSourceResponse{
			State: cty.ObjectVal(map[string]cty.Value{
				"config_contents": config,
				"config":          config,
			}),
		}
	default:
		return providers.ReadDataSourceResponse{
			Diagnostics: tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Unsupported data source",
				fmt.Sprintf("fake apko provider does not implement %q", request.TypeName),
			)),
		}
	}
}

func (provider *ApkoProvider) PlanResourceChange(request providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
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
	if imageRef := vals["image_ref"]; imageRef.IsNull() || !imageRef.IsKnown() {
		vals["image_ref"] = cty.UnknownVal(cty.String)
	}
	return providers.PlanResourceChangeResponse{
		PlannedState: cty.ObjectVal(vals),
	}
}

func (provider *ApkoProvider) ApplyResourceChange(request providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
	if request.PlannedState.IsNull() {
		return providers.ApplyResourceChangeResponse{
			NewState: request.PlannedState,
		}
	}

	resource := request.PlannedState
	vals := resource.AsValueMap()
	repo := vals["repo"].AsString()
	input := ""
	if attr, ok := vals["config"]; ok && attr.IsKnown() && !attr.IsNull() {
		input = attr.AsString()
	} else if attr, ok := vals["configs_raw"]; ok && attr.IsKnown() && !attr.IsNull() {
		input = attr.GoString()
	}
	sum := sha256.Sum256([]byte(repo + "\n" + input))
	digest := fmt.Sprintf("%x", sum[:])

	vals["id"] = cty.StringVal(digest)
	vals["image_ref"] = cty.StringVal(fmt.Sprintf("%s@sha256:%s", repo, digest))

	return providers.ApplyResourceChangeResponse{
		NewState: cty.ObjectVal(vals),
	}
}
