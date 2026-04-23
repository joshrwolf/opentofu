// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/dice"
	buildhcl "github.com/opentofu/opentofu/internal/build/hcl"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/build/solve"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
)

type Inspector struct {
	solver    *solve.Solver
	catalog   *catalog.Catalog
	providers *buildprovider.Session
}

type InspectorConfig struct {
	ProviderFactories    map[addrs.Provider]providers.Factory
	ProviderFactoryError error
}

func NewInspector(ctx context.Context, loaded *Loaded, cfg InspectorConfig) (*Inspector, tfdiags.Diagnostics) {
	ctx, span := tracing.Tracer().Start(ctx, "build.NewInspector")
	defer span.End()

	var diags tfdiags.Diagnostics

	if loaded == nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build configuration not loaded",
			"Cannot create a build inspector without a loaded configuration.",
		))
		tracing.SetSpanError(span, diags)
		return nil, diags
	}

	providers := buildprovider.NewSession(buildprovider.SessionConfig{
		Factories:    cfg.ProviderFactories,
		FactoryError: cfg.ProviderFactoryError,
	})

	session, _ := dice.NewSession(ctx)
	solver := solve.New(session, loaded.Catalog, loaded, solve.Config{
		Providers: providers,
	})
	diags = diags.Append(buildhcl.ValidateBuild(loaded.Config))
	if diags.HasErrors() {
		tracing.SetSpanError(span, diags)
		diags = diags.Append(providers.Close(context.WithoutCancel(ctx)))
		return nil, diags
	}

	return &Inspector{solver: solver, catalog: loaded.Catalog, providers: providers}, diags
}

func (i *Inspector) Catalog() *catalog.Catalog { return i.catalog }
func (i *Inspector) Solver() *solve.Solver      { return i.solver }
func (i *Inspector) Stats() []dice.Stats         { return i.solver.Stats() }

func (i *Inspector) Close(ctx context.Context) tfdiags.Diagnostics {
	if i.providers == nil {
		return nil
	}
	diags := i.providers.Close(context.WithoutCancel(ctx))
	i.providers = nil
	return diags
}
