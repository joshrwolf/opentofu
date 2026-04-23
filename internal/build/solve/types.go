// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package solve

import (
	"context"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type Evaluator interface {
	EvalImportInstances(context.Context, EvalImportInstancesRequest) (InstanceResult, tfdiags.Diagnostics)
	EvalTargetInstances(context.Context, EvalTargetInstancesRequest) (InstanceResult, tfdiags.Diagnostics)
	EvalProvider(context.Context, EvalProviderRequest) (EvalResult, tfdiags.Diagnostics)
	EvalTarget(context.Context, EvalTargetRequest) (EvalResult, tfdiags.Diagnostics)
	EvalOutput(context.Context, EvalOutputRequest) (EvalResult, tfdiags.Diagnostics)
}

type EvalImportInstancesRequest struct {
	Module   catalog.ModulePath
	DeclAddr catalog.Addr
	Runtime  RuntimeResolver
}

type EvalTargetInstancesRequest struct {
	DeclAddr catalog.Addr
	Runtime  RuntimeResolver
}

type EvalTargetRequest struct {
	Addr    catalog.Addr
	Schema  *providers.Schema
	Runtime RuntimeResolver
}

type EvalProviderRequest struct {
	Module catalog.ModulePath
	Local  addrs.LocalProviderConfig
	Schema *providers.Schema
}

type EvalOutputRequest struct {
	Addr    catalog.Addr
	Runtime RuntimeResolver
}

type RuntimeResolver interface {
	TargetValue(context.Context, catalog.Addr) (cty.Value, bool, tfdiags.Diagnostics)
	OutputValue(context.Context, catalog.Addr) (cty.Value, bool, tfdiags.Diagnostics)
	ModuleValue(context.Context, catalog.ModulePath) (cty.Value, bool, tfdiags.Diagnostics)
}

type ProviderFunctionRef struct {
	Function addrs.ProviderFunction
	Range    tfdiags.SourceRange
}

type ProviderFunctionCall struct {
	Ref     ProviderFunctionRef
	Key     digest.Digest
	Request *buildprovider.FunctionRequest
}

type InstanceShape string

const (
	InstanceShapeSingle   InstanceShape = "single"
	InstanceShapeList     InstanceShape = "list"
	InstanceShapeMap      InstanceShape = "map"
	InstanceShapeOptional InstanceShape = "optional"
)

type InstanceResult struct {
	Keys                  []catalog.Key
	Digest                digest.Digest
	Shape                 InstanceShape
	Refs                  []catalog.Addr
	ProviderFunctionCalls []ProviderFunctionCall
	Deferred              bool
}

type EvalResult struct {
	Value                 cty.Value
	Payload               *buildrun.Payload
	Digest                digest.Digest
	Refs                  []catalog.Addr
	ProviderFunctionCalls []ProviderFunctionCall
	Known                 bool
	Deferred              bool
	Volatile              bool
}
