// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/providers"
)

func execResourceSchema() providers.Schema {
	return providers.Schema{
		Block: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				// Inputs
				"command": {
					Type:     cty.List(cty.String),
					Required: true,
				},
				"env": {
					Type:     cty.Map(cty.String),
					Optional: true,
				},
				"stdin": {
					Type:     cty.String,
					Optional: true,
				},
				"working_dir": {
					Type:     cty.String,
					Optional: true,
				},
				"timeout": {
					Type:     cty.String,
					Optional: true,
				},
				"cacheable": {
					Type:     cty.Bool,
					Optional: true,
				},
				"output_files": {
					Optional: true,
					NestedType: &configschema.Object{
						Nesting: configschema.NestingList,
						Attributes: map[string]*configschema.Attribute{
							"name": {Type: cty.String, Required: true},
							"path": {Type: cty.String, Required: true},
						},
					},
				},

				// Computed outputs — all paths, not content
				"stdout": {
					Type:     cty.String,
					Computed: true,
				},
				"stderr": {
					Type:     cty.String,
					Computed: true,
				},
				"exit_code": {
					Type:     cty.Number,
					Computed: true,
				},
				"files": {
					Type:     cty.Map(cty.String),
					Computed: true,
				},
			},
		},
	}
}
