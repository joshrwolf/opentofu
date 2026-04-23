// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"

	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func (l *Loaded) EvalProvider(ctx context.Context, req EvalProviderRequest) (EvalResult, tfdiags.Diagnostics) {
	if l == nil || l.Catalog == nil || l.Config == nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"HCL build source is not initialized.",
		))
	}
	if req.Schema == nil || req.Schema.Block == nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"EvalProvider requires a provider schema.",
		))
	}

	declModule := req.Module.Declaration()
	decl, ok := l.Catalog.Provider(declModule, req.Local)
	if !ok {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Unknown provider configuration "+req.Local.StringCompact()+" in module "+declModule.String()+".",
		))
	}

	providerCfg, ok := decl.Payload.(*configs.Provider)
	if !ok || providerCfg == nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Provider configuration "+req.Local.StringCompact()+" in module "+declModule.String()+" is not backed by an HCL provider declaration.",
		))
	}

	moduleCfg := configForModulePath(l.Config, req.Module)
	if moduleCfg == nil || moduleCfg.Module == nil || moduleCfg.Module.StaticEvaluator == nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Module "+req.Module.String()+" does not have an HCL static evaluator.",
		))
	}

	scope, scopeErr := l.moduleScope(ctx, req.Module)
	if scopeErr != nil {
		return EvalResult{}, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Failed to prepare scope for "+req.Module.String()+": "+scopeErr.Error(),
		))
	}
	ps := scope.WithOverlay(configs.EvalOverlay{
		ProviderFunctions: buildprovider.DefaultQueryFunctionResolver().Resolve,
	}, nil, nil)

	ident := configs.StaticIdentifier{
		Module:    moduleCfg.Path,
		Subject:   "provider." + req.Local.StringCompact(),
		DeclRange: providerCfg.DeclRange,
	}
	value, decodeDiags := ps.DecodeBlock(ctx, providerCfg.Config, req.Schema.Block.DecoderSpec(), ident)
	if decodeDiags.HasErrors() {
		if configs.StaticEvalDefers(decodeDiags) {
			return EvalResult{
				Digest:   decl.ConfigKey,
				Deferred: true,
			}, nil
		}
		return EvalResult{}, tfdiags.Diagnostics{}.Append(decodeDiags)
	}

	return EvalResult{
		Value:  value,
		Digest: decl.ConfigKey,
		Known:  true,
	}, nil
}
