// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import "github.com/opentofu/opentofu/internal/build/digest"

type TargetPayload struct {
	Binding  Binding
	TypeName string
	ConfigKey digest.Digest
	Request   TargetRequest
}

type FunctionPayload struct {
	Binding  Binding
	TypeName string
	Request  FunctionRequest
}
