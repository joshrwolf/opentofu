// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package embedded

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/providers"
)

// schemaToProviders converts a tfprotov6.Schema to a providers.Schema.
func schemaToProviders(s *tfprotov6.Schema) (providers.Schema, error) {
	if s == nil {
		return providers.Schema{}, nil
	}
	block, err := schemaBlockToConfig(s.Block)
	if err != nil {
		return providers.Schema{}, err
	}
	return providers.Schema{
		Version: s.Version,
		Block:   block,
	}, nil
}

// schemaBlockToConfig converts a tfprotov6.SchemaBlock to a configschema.Block.
func schemaBlockToConfig(b *tfprotov6.SchemaBlock) (*configschema.Block, error) {
	if b == nil {
		return &configschema.Block{}, nil
	}

	block := &configschema.Block{
		Attributes: make(map[string]*configschema.Attribute),
		BlockTypes: make(map[string]*configschema.NestedBlock),
		Description: b.Description,
		Deprecated:  b.Deprecated,
	}

	switch b.DescriptionKind {
	case tfprotov6.StringKindMarkdown:
		block.DescriptionKind = configschema.StringMarkdown
	default:
		block.DescriptionKind = configschema.StringPlain
	}

	for _, a := range b.Attributes {
		attr, err := schemaAttributeToConfig(a)
		if err != nil {
			return nil, fmt.Errorf("attribute %q: %w", a.Name, err)
		}
		block.Attributes[a.Name] = attr
	}

	for _, bt := range b.BlockTypes {
		nb, err := schemaNestedBlockToConfig(bt)
		if err != nil {
			return nil, fmt.Errorf("block type %q: %w", bt.TypeName, err)
		}
		block.BlockTypes[bt.TypeName] = nb
	}

	return block, nil
}

func schemaAttributeToConfig(a *tfprotov6.SchemaAttribute) (*configschema.Attribute, error) {
	attr := &configschema.Attribute{
		Description: a.Description,
		Required:    a.Required,
		Optional:    a.Optional,
		Computed:    a.Computed,
		Sensitive:   a.Sensitive,
		Deprecated:  a.Deprecated,
		WriteOnly:   a.WriteOnly,
	}

	switch a.DescriptionKind {
	case tfprotov6.StringKindMarkdown:
		attr.DescriptionKind = configschema.StringMarkdown
	default:
		attr.DescriptionKind = configschema.StringPlain
	}

	if a.Type != nil {
		ct, err := tftypeToCtyType(a.Type)
		if err != nil {
			return nil, err
		}
		attr.Type = ct
	}

	if a.NestedType != nil {
		nt, err := schemaObjectToConfig(a.NestedType)
		if err != nil {
			return nil, err
		}
		attr.NestedType = nt
	}

	return attr, nil
}

func schemaNestedBlockToConfig(b *tfprotov6.SchemaNestedBlock) (*configschema.NestedBlock, error) {
	nb := &configschema.NestedBlock{
		MinItems: int(b.MinItems),
		MaxItems: int(b.MaxItems),
	}

	// Nesting mode numbers differ between tfprotov6 and configschema;
	// explicit mapping is required.
	switch b.Nesting {
	case tfprotov6.SchemaNestedBlockNestingModeSingle:
		nb.Nesting = configschema.NestingSingle
	case tfprotov6.SchemaNestedBlockNestingModeGroup:
		nb.Nesting = configschema.NestingGroup
	case tfprotov6.SchemaNestedBlockNestingModeList:
		nb.Nesting = configschema.NestingList
	case tfprotov6.SchemaNestedBlockNestingModeSet:
		nb.Nesting = configschema.NestingSet
	case tfprotov6.SchemaNestedBlockNestingModeMap:
		nb.Nesting = configschema.NestingMap
	}

	block, err := schemaBlockToConfig(b.Block)
	if err != nil {
		return nil, err
	}
	nb.Block = *block

	return nb, nil
}

func schemaObjectToConfig(o *tfprotov6.SchemaObject) (*configschema.Object, error) {
	obj := &configschema.Object{
		Attributes: make(map[string]*configschema.Attribute),
	}

	switch o.Nesting {
	case tfprotov6.SchemaObjectNestingModeSingle:
		obj.Nesting = configschema.NestingSingle
	case tfprotov6.SchemaObjectNestingModeList:
		obj.Nesting = configschema.NestingList
	case tfprotov6.SchemaObjectNestingModeSet:
		obj.Nesting = configschema.NestingSet
	case tfprotov6.SchemaObjectNestingModeMap:
		obj.Nesting = configschema.NestingMap
	}

	for _, a := range o.Attributes {
		attr, err := schemaAttributeToConfig(a)
		if err != nil {
			return nil, fmt.Errorf("attribute %q: %w", a.Name, err)
		}
		obj.Attributes[a.Name] = attr
	}

	return obj, nil
}

func functionToSpec(f *tfprotov6.Function) (providers.FunctionSpec, error) {
	spec := providers.FunctionSpec{
		Summary:            f.Summary,
		Description:        f.Description,
		DeprecationMessage: f.DeprecationMessage,
	}

	switch f.DescriptionKind {
	case tfprotov6.StringKindMarkdown:
		spec.DescriptionFormat = providers.TextFormattingMarkdown
	default:
		spec.DescriptionFormat = providers.TextFormattingPlain
	}

	spec.Parameters = make([]providers.FunctionParameterSpec, len(f.Parameters))
	for i, p := range f.Parameters {
		param, err := functionParameterToSpec(p)
		if err != nil {
			return spec, fmt.Errorf("parameter %d: %w", i, err)
		}
		spec.Parameters[i] = param
	}

	if f.VariadicParameter != nil {
		vp, err := functionParameterToSpec(f.VariadicParameter)
		if err != nil {
			return spec, fmt.Errorf("variadic parameter: %w", err)
		}
		spec.VariadicParameter = &vp
	}

	if f.Return != nil {
		rt, err := tftypeToCtyType(f.Return.Type)
		if err != nil {
			return spec, fmt.Errorf("return type: %w", err)
		}
		spec.Return = rt
	}

	return spec, nil
}

func functionParameterToSpec(p *tfprotov6.FunctionParameter) (providers.FunctionParameterSpec, error) {
	ct, err := tftypeToCtyType(p.Type)
	if err != nil {
		return providers.FunctionParameterSpec{}, err
	}

	spec := providers.FunctionParameterSpec{
		Name:               p.Name,
		Type:               ct,
		AllowNullValue:     p.AllowNullValue,
		AllowUnknownValues: p.AllowUnknownValues,
		Description:        p.Description,
	}

	switch p.DescriptionKind {
	case tfprotov6.StringKindMarkdown:
		spec.DescriptionFormat = providers.TextFormattingMarkdown
	default:
		spec.DescriptionFormat = providers.TextFormattingPlain
	}

	return spec, nil
}

// identitySchemaToProviders converts a tfprotov6 resource identity schema.
func identitySchemaToProviders(s *tfprotov6.ResourceIdentitySchema) (*providers.ResourceIdentitySchema, error) {
	if s == nil {
		return nil, nil
	}

	attrs := make(map[string]*configschema.Attribute, len(s.IdentityAttributes))
	for _, a := range s.IdentityAttributes {
		attr := &configschema.Attribute{
			Description: a.Description,
			Required:    a.RequiredForImport,
			Optional:    a.OptionalForImport,
		}
		if a.Type != nil {
			ct, err := tftypeToCtyType(a.Type)
			if err != nil {
				return nil, fmt.Errorf("identity attribute %q: %w", a.Name, err)
			}
			attr.Type = ct
		}
		attrs[a.Name] = attr
	}

	return &providers.ResourceIdentitySchema{
		Version: s.Version,
		Body: &configschema.Object{
			Attributes: attrs,
			Nesting:    configschema.NestingSingle,
		},
	}, nil
}
