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

var OciProviderSchema = &providers.GetProviderSchemaResponse{
	Provider: providers.Schema{
		Block: &configschema.Block{},
	},
	ResourceTypes: map[string]providers.Schema{
		"oci_tags": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"id":     {Type: cty.String, Optional: true, Computed: true},
					"source": {Type: cty.String, Required: true},
					"tag":    {Type: cty.String, Required: true},
				},
			},
		},
	},
	DataSources: map[string]providers.Schema{
		"oci_exec_test": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"script": {Type: cty.String, Required: true},
					"result": {Type: cty.String, Optional: true, Computed: true},
				},
			},
		},
		"oci_structure_test": {
			Block: &configschema.Block{
				Attributes: map[string]*configschema.Attribute{
					"config": {Type: cty.String, Required: true},
					"result": {Type: cty.String, Optional: true, Computed: true},
				},
			},
		},
	},
}

type OciProvider struct {
	Provider *tofu.MockProvider
}

func NewOciProvider() *OciProvider {
	provider := &OciProvider{
		Provider: new(tofu.MockProvider),
	}

	provider.Provider.GetProviderSchemaResponse = OciProviderSchema
	provider.Provider.ConfigureProviderFn = provider.ConfigureProvider
	provider.Provider.PlanResourceChangeFn = provider.PlanResourceChange
	provider.Provider.ApplyResourceChangeFn = provider.ApplyResourceChange
	provider.Provider.ReadDataSourceFn = provider.ReadDataSource

	return provider
}

func (provider *OciProvider) ConfigureProvider(request providers.ConfigureProviderRequest) providers.ConfigureProviderResponse {
	return providers.ConfigureProviderResponse{}
}

func (provider *OciProvider) ReadDataSource(request providers.ReadDataSourceRequest) providers.ReadDataSourceResponse {
	vals := request.Config.AsValueMap()
	switch request.TypeName {
	case "oci_exec_test":
		vals["result"] = vals["script"]
	case "oci_structure_test":
		vals["result"] = vals["config"]
	}
	return providers.ReadDataSourceResponse{
		State: cty.ObjectVal(vals),
	}
}

func (provider *OciProvider) PlanResourceChange(request providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
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

func (provider *OciProvider) ApplyResourceChange(request providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
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
