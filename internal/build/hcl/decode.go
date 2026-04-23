// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type targetDecoder struct {
	ps        *configs.PreparedScope
	module    addrs.Module
	sourceDir string
}

func (l *Loaded) newTargetDecoder(ctx context.Context, addr catalog.Addr, declRange hcl.Range, countExpr, forEachExpr, enabledExpr hcl.Expression, runtime RuntimeResolver, moduleOptions *configs.StaticEvalOptions) (*targetDecoder, tfdiags.Diagnostics) {
	moduleCfg := configForModulePath(l.Config, addr.Module)
	if moduleCfg == nil || moduleCfg.Module == nil || moduleCfg.Module.StaticEvaluator == nil {
		return nil, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Module "+addr.Module.String()+" does not have an HCL static evaluator.",
		))
	}

	var moduleOpts configs.StaticEvalOptions
	if moduleOptions != nil {
		moduleOpts = *moduleOptions
	}
	if runtime != nil {
		moduleOpts.Runtime = runtimeLookupForTarget(addr, runtime)
	}
	targetOpts, optionDiags := l.targetEvalOptions(ctx, addr, declRange, countExpr, forEachExpr, enabledExpr, moduleOpts)
	if optionDiags.HasErrors() {
		return nil, optionDiags
	}

	ps, psDiags := moduleCfg.Module.StaticEvaluator.Prepare(ctx, moduleOpts.Variables, moduleOpts.EvalCache)
	if psDiags.HasErrors() {
		return nil, tfdiags.Diagnostics{}.Append(psDiags)
	}
	ps = ps.WithOverlay(configs.EvalOverlay{
		Runtime:           moduleOpts.Runtime,
		ProviderFunctions: buildprovider.DefaultQueryFunctionResolver().Resolve,
	}, targetOpts.CountAttrs, targetOpts.ForEachAttrs)

	return &targetDecoder{
		ps:        ps,
		module:    moduleCfg.Path,
		sourceDir: moduleCfg.Module.SourceDir,
	}, nil
}

func (d *targetDecoder) decode(ctx context.Context, body hcl.Body, schema *configschema.Block, subject string, declRange hcl.Range) (cty.Value, bool, tfdiags.Diagnostics) {
	ident := configs.StaticIdentifier{
		Module:    d.module,
		Subject:   subject,
		DeclRange: declRange,
	}
	value, decodeDiags := d.ps.DecodeBlock(ctx, body, schema.DecoderSpec(), ident)
	if decodeDiags.HasErrors() {
		if configs.StaticEvalDefers(decodeDiags) {
			return cty.NilVal, true, nil
		}
		return cty.NilVal, false, tfdiags.Diagnostics{}.Append(decodeDiags)
	}
	return value, false, nil
}
