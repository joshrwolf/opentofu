// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

const (
	targetRequestVersion = "build-provider-target-request-v1"
	targetResultVersion  = "build-provider-target-result-v1"
)

type TargetRequest struct {
	Kind     catalog.RunnerKind
	Provider addrs.Provider
	TypeName string
	Key      digest.Digest
	Payload  []byte
}

type targetRequestEnvelope struct {
	Version       string `json:"version"`
	Kind          string `json:"kind"`
	TypeName      string `json:"type_name"`
	SchemaVersion uint64 `json:"schema_version"`
	ConfigType    []byte `json:"config_type"`
	Config        []byte `json:"config"`
	ResultType    []byte `json:"result_type"`
}

type targetResultEnvelope struct {
	Version string `json:"version"`
	State   []byte `json:"state"`
}

func BuildTargetRequest(kind catalog.RunnerKind, provider addrs.Provider, typeName string, schema *providers.Schema, config cty.Value, rng tfdiags.SourceRange) (*TargetRequest, tfdiags.Diagnostics) {
	if schema == nil || schema.Block == nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Unsupported provider target in build mode",
			fmt.Sprintf("Target %q from provider %s does not have a schema available for build-mode lowering.", typeName, provider.String()),
		))
	}

	impliedType := schema.Block.ImpliedType()
	configJSON, err := ctyjson.Marshal(config, impliedType)
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Failed to encode configuration for %s %q: %s", kind, typeName, err),
		))
	}
	typeJSON, err := ctyjson.MarshalType(impliedType)
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Failed to encode schema type for %s %q: %s", kind, typeName, err),
		))
	}

	envelope := targetRequestEnvelope{
		Version:       targetRequestVersion,
		Kind:          string(kind),
		TypeName:      typeName,
		SchemaVersion: uint64(schema.Version),
		ConfigType:    typeJSON,
		Config:        configJSON,
		ResultType:    typeJSON,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Failed to encode request for %s %q: %s", kind, typeName, err),
		))
	}

	key, err := digest.FromValue(struct {
		Version  string
		Kind     catalog.RunnerKind
		Provider addrs.Provider
		TypeName string
		Payload  []byte
	}{
		Version:  targetRequestVersion,
		Kind:     kind,
		Provider: provider,
		TypeName: typeName,
		Payload:  payload,
	})
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Failed to key request for %s %q: %s", kind, typeName, err),
		))
	}

	return &TargetRequest{
		Kind:     kind,
		Provider: provider,
		TypeName: typeName,
		Key:      key,
		Payload:  payload,
	}, nil
}

func DecodeTargetRequest(req TargetRequest, rng tfdiags.SourceRange) (targetRequestEnvelope, cty.Type, cty.Value, tfdiags.Diagnostics) {
	var envelope targetRequestEnvelope
	if err := json.Unmarshal(req.Payload, &envelope); err != nil {
		return targetRequestEnvelope{}, cty.NilType, cty.NilVal, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Failed to decode request for %s %q: %s", req.Kind, req.TypeName, err),
		))
	}
	if envelope.Version != targetRequestVersion {
		return targetRequestEnvelope{}, cty.NilType, cty.NilVal, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Provider target request for %s %q uses unsupported version %q.", req.Kind, req.TypeName, envelope.Version),
		))
	}

	configType, err := ctyjson.UnmarshalType(envelope.ConfigType)
	if err != nil {
		return targetRequestEnvelope{}, cty.NilType, cty.NilVal, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Failed to decode configuration type for %s %q: %s", req.Kind, req.TypeName, err),
		))
	}

	config, err := ctyjson.Unmarshal(envelope.Config, configType)
	if err != nil {
		return targetRequestEnvelope{}, cty.NilType, cty.NilVal, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Failed to decode configuration for %s %q: %s", req.Kind, req.TypeName, err),
		))
	}

	return envelope, configType, config, nil
}

func DecodeTargetResult(req TargetRequest, payload []byte, rng tfdiags.SourceRange) (cty.Value, digest.Digest, tfdiags.Diagnostics) {
	var envelope targetRequestEnvelope
	if err := json.Unmarshal(req.Payload, &envelope); err != nil {
		return cty.NilVal, digest.Digest{}, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Failed to decode request for %s %q: %s", req.Kind, req.TypeName, err),
		))
	}
	if envelope.Version != targetRequestVersion {
		return cty.NilVal, digest.Digest{}, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target request",
			fmt.Sprintf("Provider target request for %s %q uses unsupported version %q.", req.Kind, req.TypeName, envelope.Version),
		))
	}

	resultType, err := ctyjson.UnmarshalType(envelope.ResultType)
	if err != nil {
		return cty.NilVal, digest.Digest{}, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target result",
			fmt.Sprintf("Failed to decode result type for %s %q: %s", req.Kind, req.TypeName, err),
		))
	}

	var result targetResultEnvelope
	if err := json.Unmarshal(payload, &result); err != nil {
		return cty.NilVal, digest.Digest{}, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target result",
			fmt.Sprintf("Failed to decode result for %s %q: %s", req.Kind, req.TypeName, err),
		))
	}
	if result.Version != targetResultVersion {
		return cty.NilVal, digest.Digest{}, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target result",
			fmt.Sprintf("Provider target result for %s %q uses unsupported version %q.", req.Kind, req.TypeName, result.Version),
		))
	}

	value, err := ctyjson.Unmarshal(result.State, resultType)
	if err != nil {
		return cty.NilVal, digest.Digest{}, tfdiags.Diagnostics{}.Append(functionRequestDiagnostic(
			rng,
			"Invalid provider target result",
			fmt.Sprintf("Failed to decode state for %s %q: %s", req.Kind, req.TypeName, err),
		))
	}

	return value, digest.FromBytes(payload), nil
}

func encodeTargetResult(value cty.Value, ty cty.Type) ([]byte, error) {
	stateJSON, err := ctyjson.Marshal(value, ty)
	if err != nil {
		return nil, err
	}
	return json.Marshal(targetResultEnvelope{
		Version: targetResultVersion,
		State:   stateJSON,
	})
}
