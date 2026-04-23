// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

const configRequestVersion = "build-provider-config-request-v1"

type ConfigRequest struct {
	Provider addrs.Provider
	Key      digest.Digest
	Payload  []byte
}

type configRequestEnvelope struct {
	Version    string `json:"version"`
	ConfigType []byte `json:"config_type"`
	Config     []byte `json:"config"`
}

func BuildConfigRequest(provider addrs.Provider, schema *providers.Schema, config cty.Value, rng tfdiags.SourceRange) (*ConfigRequest, tfdiags.Diagnostics) {
	if schema == nil || schema.Block == nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Unsupported provider configuration in build mode",
			fmt.Sprintf("Provider %s does not expose a provider schema for build-mode configuration.", provider.String()),
		))
	}

	impliedType := schema.Block.ImpliedType()
	configJSON, err := ctyjson.Marshal(config, impliedType)
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider configuration request",
			fmt.Sprintf("Failed to encode provider configuration for %s: %s", provider.String(), err),
		))
	}
	typeJSON, err := ctyjson.MarshalType(impliedType)
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider configuration request",
			fmt.Sprintf("Failed to encode provider configuration type for %s: %s", provider.String(), err),
		))
	}

	payload, err := json.Marshal(configRequestEnvelope{
		Version:    configRequestVersion,
		ConfigType: typeJSON,
		Config:     configJSON,
	})
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider configuration request",
			fmt.Sprintf("Failed to encode provider configuration request for %s: %s", provider.String(), err),
		))
	}

	key, err := digest.FromValue(struct {
		Version  string
		Provider addrs.Provider
		Payload  []byte
	}{
		Version:  configRequestVersion,
		Provider: provider,
		Payload:  payload,
	})
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider configuration request",
			fmt.Sprintf("Failed to key provider configuration request for %s: %s", provider.String(), err),
		))
	}

	return &ConfigRequest{
		Provider: provider,
		Key:      key,
		Payload:  payload,
	}, nil
}

func DecodeConfigRequest(req ConfigRequest, rng tfdiags.SourceRange) (cty.Value, tfdiags.Diagnostics) {
	var envelope configRequestEnvelope
	if err := json.Unmarshal(req.Payload, &envelope); err != nil {
		return cty.NilVal, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider configuration request",
			fmt.Sprintf("Failed to decode provider configuration request for %s: %s", req.Provider.String(), err),
		))
	}
	if envelope.Version != configRequestVersion {
		return cty.NilVal, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider configuration request",
			fmt.Sprintf("Provider configuration request for %s uses unsupported version %q.", req.Provider.String(), envelope.Version),
		))
	}

	configType, err := ctyjson.UnmarshalType(envelope.ConfigType)
	if err != nil {
		return cty.NilVal, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider configuration request",
			fmt.Sprintf("Failed to decode provider configuration type for %s: %s", req.Provider.String(), err),
		))
	}

	config, err := ctyjson.Unmarshal(envelope.Config, configType)
	if err != nil {
		return cty.NilVal, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider configuration request",
			fmt.Sprintf("Failed to decode provider configuration for %s: %s", req.Provider.String(), err),
		))
	}

	return config, nil
}
