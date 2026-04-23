// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"
	"runtime"

	"go.opentelemetry.io/otel/attribute"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/dice"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildhcl "github.com/opentofu/opentofu/internal/build/hcl"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	"github.com/opentofu/opentofu/internal/build/selector"
	"github.com/opentofu/opentofu/internal/build/solve"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
)

type Engine struct {
	providerSession *buildprovider.Session
	exec            *executor
}

type EngineConfig struct {
	ProviderFactories    map[addrs.Provider]providers.Factory
	ProviderFactoryError error
	ProviderInvoker      *buildprovider.Invoker
	Parallelism          int
	Cache                engine.Cache
	Logf                 func(string, ...any)
}

func NewEngine(ctx context.Context, loaded *Loaded, cfg EngineConfig) (*Engine, tfdiags.Diagnostics) {
	ctx, span := tracing.Tracer().Start(ctx, "build.NewEngine")
	defer span.End()

	var diags tfdiags.Diagnostics

	if loaded == nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build configuration not loaded",
			"Cannot create a build engine without a loaded configuration.",
		))
		tracing.SetSpanError(span, diags)
		return nil, diags
	}

	providerSession := buildprovider.NewSession(buildprovider.SessionConfig{
		Factories:    cfg.ProviderFactories,
		FactoryError: cfg.ProviderFactoryError,
	})

	parallelism := cfg.Parallelism
	if parallelism <= 0 {
		parallelism = runtime.GOMAXPROCS(0)
	}

	session, _ := dice.NewSession(ctx)
	runner := NewRunner(RunnerConfig{
		ProviderInvoker: cfg.ProviderInvoker,
		ProviderSession: providerSession,
	})
	dispatcher := engine.New(engine.Config{
		Context:     ctx,
		Parallelism: parallelism,
		Runner:      runner,
		Cache:       cfg.Cache,
	})

	solver := solve.New(session, loaded.Catalog, loaded, solve.Config{
		Providers: providerSession,
		Engine:    dispatcher,
	})
	diags = diags.Append(buildhcl.ValidateBuild(loaded.Config))
	if diags.HasErrors() {
		tracing.SetSpanError(span, diags)
		diags = diags.Append(providerSession.Close(ctx))
		return nil, diags
	}

	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	span.SetAttributes(
		attribute.Int("build.parallelism", parallelism),
	)

	return &Engine{
		providerSession: providerSession,
		exec: &executor{
			solver:  solver,
			catalog: loaded.Catalog,
			engine:  dispatcher,
			runner:  runner,
			logf:    logf,
		},
	}, diags
}

func (e *Engine) Execute(ctx context.Context, selectors selector.Set) (Result, tfdiags.Diagnostics) {
	return e.exec.Execute(ctx, selectors)
}

func (e *Engine) Stats() []dice.Stats {
	return e.exec.solver.Stats()
}

func (e *Engine) Close(ctx context.Context) tfdiags.Diagnostics {
	if e.providerSession == nil {
		return nil
	}
	diags := e.providerSession.Close(context.WithoutCancel(ctx))
	e.providerSession = nil
	return diags
}
