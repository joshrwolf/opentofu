// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"sync"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

type SessionConfig struct {
	Factories          map[addrs.Provider]providers.Factory
	FactoryError       error
	TargetCapabilities *TargetCapabilityRegistry
}

type Session struct {
	factories    map[addrs.Provider]providers.Factory
	startErr     error
	capabilities *TargetCapabilityRegistry

	mu        sync.Mutex
	schemas   map[addrs.Provider]providers.ProviderSchema
	providers map[sessionBindingKey]providers.Interface
}

type sessionBindingKey struct {
	Provider  addrs.Provider
	ConfigKey digest.Digest
}

func NewSession(cfg SessionConfig) *Session {
	factories := cfg.Factories
	if factories == nil {
		factories = map[addrs.Provider]providers.Factory{}
	}
	capabilities := cfg.TargetCapabilities
	if capabilities == nil {
		capabilities = DefaultTargetCapabilityRegistry()
	}
	return &Session{
		factories:    factories,
		startErr:     cfg.FactoryError,
		capabilities: capabilities,
		schemas:      map[addrs.Provider]providers.ProviderSchema{},
		providers:    map[sessionBindingKey]providers.Interface{},
	}
}

func (s *Session) Capability(provider addrs.Provider, kind catalog.RunnerKind, typeName string) (ResolvedTargetCapability, bool) {
	if s == nil {
		return ResolvedTargetCapability{}, false
	}
	return s.capabilities.Capability(provider, kind, typeName)
}

func (s *Session) Close(ctx context.Context) tfdiags.Diagnostics {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	providersToClose := make([]providers.Interface, 0, len(s.providers))
	for _, instance := range s.providers {
		providersToClose = append(providersToClose, instance)
	}
	s.providers = map[sessionBindingKey]providers.Interface{}
	s.mu.Unlock()

	var diags tfdiags.Diagnostics
	for _, instance := range providersToClose {
		if instance == nil {
			continue
		}
		if err := instance.Close(ctx); err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Warning,
				"Failed to close build provider",
				err.Error(),
			))
		}
	}
	return diags
}

func (s *Session) Schema(ctx context.Context, provider addrs.Provider) (*providers.ProviderSchema, tfdiags.Diagnostics) {
	if s == nil {
		return nil, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			"Build provider session is not configured.",
		))
	}
	if s.startErr != nil {
		return nil, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			s.startErr.Error(),
		))
	}

	s.mu.Lock()
	if schema, ok := s.schemas[provider]; ok {
		s.mu.Unlock()
		return &schema, nil
	}
	s.mu.Unlock()

	factory, ok := s.factories[provider]
	if !ok {
		return nil, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			"Provider "+provider.String()+" is not available for build execution. Run tofu init or configure a test provider override first.",
		))
	}

	instance, err := factory()
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			fmt.Sprintf("Failed to start provider %s: %s", provider.String(), err),
		))
	}
	defer instance.Close(ctx)

	schema := instance.GetProviderSchema(ctx)
	if err := schema.Validate(provider); err != nil {
		return nil, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			err.Error(),
		))
	}

	s.mu.Lock()
	s.schemas[provider] = schema
	s.mu.Unlock()
	return &schema, schema.Diagnostics
}

func (s *Session) TargetSchema(ctx context.Context, kind catalog.RunnerKind, provider addrs.Provider, typeName string) (*providers.Schema, tfdiags.Diagnostics) {
	capability, ok := s.Capability(provider, kind, typeName)
	if !ok || !capability.Supported {
		return nil, nil
	}

	schemaSet, diags := s.Schema(ctx, provider)
	if diags.HasErrors() || schemaSet == nil {
		return nil, diags
	}

	mode := addrs.ManagedResourceMode
	if kind == catalog.RunnerKindProviderData {
		mode = addrs.DataResourceMode
	}
	schema, _ := schemaSet.SchemaForResourceType(mode, typeName)
	if schema == nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			fmt.Sprintf("Provider %s does not expose schema for %s %q.", provider.String(), kind, typeName),
		))
	}
	return schema, diags
}

func (s *Session) InvokeTarget(ctx context.Context, binding Binding, req TargetRequest) (InvokeResult, tfdiags.Diagnostics) {
	if s == nil {
		return InvokeResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			"Build provider session is not configured.",
		))
	}
	capability, ok := s.Capability(binding.Provider, req.Kind, req.TypeName)
	if !ok || !capability.Supported {
		return InvokeResult{}, UnsupportedTargetError(req.Kind, binding.Provider, req.TypeName)
	}

	instance, diags := s.instance(ctx, binding)
	if diags.HasErrors() {
		return InvokeResult{}, diags
	}
	if instance == nil {
		return InvokeResult{}, diags
	}

	envelope, configType, config, requestDiags := DecodeTargetRequest(req, tfdiags.SourceRange{})
	diags = diags.Append(requestDiags)
	if requestDiags.HasErrors() {
		return InvokeResult{}, diags
	}
	resultType, err := ctyjson.UnmarshalType(envelope.ResultType)
	if err != nil {
		return InvokeResult{}, diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			fmt.Sprintf("Failed to decode result type for %s %q: %s", req.Kind, req.TypeName, err),
		))
	}

	switch req.Kind {
	case catalog.RunnerKindProviderData:
		resp := instance.ReadDataSource(ctx, providers.ReadDataSourceRequest{
			TypeName:     req.TypeName,
			Config:       config,
			ProviderMeta: cty.NullVal(cty.DynamicPseudoType),
		})
		diags = diags.Append(resp.Diagnostics)
		if resp.Diagnostics.HasErrors() {
			return InvokeResult{}, diags
		}

		payload, err := encodeTargetResult(resp.State, resultType)
		if err != nil {
			return InvokeResult{}, diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build provider error",
				fmt.Sprintf("Failed to encode build result for %s %q: %s", req.Kind, req.TypeName, err),
			))
		}
		return InvokeResult{
			OutputKey: digest.FromBytes(payload),
			Payload:   payload,
		}, diags
	case catalog.RunnerKindProviderResource:
		prior := cty.NullVal(configType)
		plan := instance.PlanResourceChange(ctx, providers.PlanResourceChangeRequest{
			TypeName:         req.TypeName,
			PriorState:       prior,
			ProposedNewState: config,
			Config:           config,
			ProviderMeta:     cty.NullVal(cty.DynamicPseudoType),
		})
		diags = diags.Append(plan.Diagnostics)
		if plan.Diagnostics.HasErrors() {
			return InvokeResult{}, diags
		}

		apply := instance.ApplyResourceChange(ctx, providers.ApplyResourceChangeRequest{
			TypeName:       req.TypeName,
			PriorState:     prior,
			PlannedState:   plan.PlannedState,
			Config:         config,
			PlannedPrivate: plan.PlannedPrivate,
			ProviderMeta:   cty.NullVal(cty.DynamicPseudoType),
		})
		diags = diags.Append(apply.Diagnostics)
		if apply.Diagnostics.HasErrors() {
			return InvokeResult{}, diags
		}

		payload, err := encodeTargetResult(apply.NewState, resultType)
		if err != nil {
			return InvokeResult{}, diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Build provider error",
				fmt.Sprintf("Failed to encode build result for %s %q: %s", req.Kind, req.TypeName, err),
			))
		}
		return InvokeResult{
			OutputKey: digest.FromBytes(payload),
			Payload:   payload,
		}, diags
	default:
		return InvokeResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			"Provider target kind "+string(req.Kind)+" is not implemented.",
		))
	}
}

func (s *Session) instance(ctx context.Context, binding Binding) (providers.Interface, tfdiags.Diagnostics) {
	key := sessionBindingKey{
		Provider:  binding.Provider,
		ConfigKey: binding.ConfigKey,
	}
	if s.startErr != nil {
		return nil, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			s.startErr.Error(),
		))
	}

	s.mu.Lock()
	if instance, ok := s.providers[key]; ok {
		s.mu.Unlock()
		return instance, nil
	}
	s.mu.Unlock()

	factory, ok := s.factories[binding.Provider]
	if !ok {
		return nil, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			"Provider "+binding.Provider.String()+" is not available for build execution.",
		))
	}

	instance, err := factory()
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build provider error",
			fmt.Sprintf("Failed to start provider %s: %s", binding.Provider.String(), err),
		))
	}

	schema, diags := s.Schema(ctx, binding.Provider)
	if diags.HasErrors() || schema == nil {
		instance.Close(ctx)
		return nil, diags
	}
	config := cty.EmptyObjectVal
	if schema.Provider.Block != nil {
		config = schema.Provider.Block.EmptyValue()
	}
	if binding.ConfigRequest != nil {
		requestConfig, requestDiags := DecodeConfigRequest(*binding.ConfigRequest, tfdiags.SourceRange{})
		diags = diags.Append(requestDiags)
		if requestDiags.HasErrors() {
			instance.Close(ctx)
			return nil, diags
		}
		config = requestConfig
	}
	configure := instance.ConfigureProvider(ctx, providers.ConfigureProviderRequest{Config: config})
	diags = diags.Append(configure.Diagnostics)
	if configure.Diagnostics.HasErrors() {
		instance.Close(ctx)
		return nil, diags
	}

	s.mu.Lock()
	if existing, ok := s.providers[key]; ok {
		s.mu.Unlock()
		instance.Close(ctx)
		return existing, diags
	}
	s.providers[key] = instance
	s.mu.Unlock()

	return instance, diags
}
