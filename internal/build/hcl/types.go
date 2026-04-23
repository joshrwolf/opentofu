// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import "github.com/opentofu/opentofu/internal/build/solve"

type LoadRequest struct {
	RootDir string
}

// Type aliases for types owned by the solve package.
// The hcl evaluator implements the solve.Evaluator interface using these types.
type (
	EvalResult                 = solve.EvalResult
	InstanceResult             = solve.InstanceResult
	InstanceShape              = solve.InstanceShape
	EvalImportInstancesRequest = solve.EvalImportInstancesRequest
	EvalTargetInstancesRequest = solve.EvalTargetInstancesRequest
	EvalTargetRequest          = solve.EvalTargetRequest
	EvalProviderRequest        = solve.EvalProviderRequest
	EvalOutputRequest          = solve.EvalOutputRequest
	RuntimeResolver            = solve.RuntimeResolver
	ProviderFunctionRef        = solve.ProviderFunctionRef
	ProviderFunctionCall       = solve.ProviderFunctionCall
)

const (
	InstanceShapeSingle   = solve.InstanceShapeSingle
	InstanceShapeList     = solve.InstanceShapeList
	InstanceShapeMap      = solve.InstanceShapeMap
	InstanceShapeOptional = solve.InstanceShapeOptional
)
