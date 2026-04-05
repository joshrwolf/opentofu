// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"log"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// BuildOpts are options for the Build operation.
type BuildOpts struct {
	// SetVariables are the raw variable values provided by the caller
	// (e.g., from CLI -var flags or .tfvars files).
	SetVariables InputValues

	// SkipRoles causes resources with any of these roles to be skipped
	// during build execution. For example, SkipRoles: ["test"] skips all
	// resources whose provider declares them as RoleTest.
	SkipRoles []providers.ResourceRole

	// OnlyRoles, if non-empty, causes only resources with one of these roles
	// to be executed. Resources with other roles (or no role) are skipped.
	// OnlyRoles and SkipRoles are mutually exclusive; if both are set,
	// OnlyRoles takes precedence.
	OnlyRoles []providers.ResourceRole

	// OutputTargets, if non-empty, prunes the execution graph to only the
	// backward closure of the named root outputs. For example,
	// OutputTargets: ["image_ref"] executes only what's needed to produce
	// the "image_ref" output, skipping unrelated resources like tests/tags.
	OutputTargets []string

	// Targets restricts execution to specific resources or modules and their
	// dependencies. This is the primary mechanism for scoping builds within
	// the full DAG (e.g., -target=module.nginx builds only that module).
	Targets []addrs.Targetable

	// Excludes removes specific resources or modules from the build.
	Excludes []addrs.Targetable
}

// Build performs a single-pass build execution. Unlike the traditional
// Plan+Apply workflow, Build walks the resource graph once:
//
//   - For each resource, it evaluates the configuration, computes a content
//     hash from the evaluated inputs, and checks the persisted state.
//   - If the hash matches (cache hit), the resource is skipped.
//   - If it doesn't match (cache miss), the provider's ApplyResourceChange
//     is called directly, and the result is written to state with the hash.
//
// This is designed for build-system workloads where:
//   - Providers are not designed around PlanResourceChange semantics
//   - There is no concept of infrastructure drift (builds are deterministic)
//   - The plan phase is pure overhead (with empty state, everything is Create)
//
// Build returns the new state after execution. The caller is responsible for
// persisting it via the backend.
func (c *Context) Build(ctx context.Context, config *configs.Config, prevState *states.State, opts *BuildOpts) (*states.State, tfdiags.Diagnostics) {
	defer c.acquireRun("build")()

	var diags tfdiags.Diagnostics

	log.Printf("[DEBUG] Building graph for single-pass build execution")

	if opts == nil {
		opts = &BuildOpts{}
	}

	variables := opts.SetVariables
	if variables == nil {
		variables = make(InputValues)
	}

	diags = diags.Append(checkInputVariables(config.Module.Variables, variables))
	if diags.HasErrors() {
		return nil, diags
	}

	providerFunctionTracker := make(ProviderFunctionMapping)

	graph, graphDiags := (&BuildGraphBuilder{
		Config:                  config,
		State:                   prevState,
		RootVariableValues:      variables,
		Plugins:                 c.plugins,
		Targets:                 opts.Targets,
		Excludes:                opts.Excludes,
		SkipRoles:               opts.SkipRoles,
		OnlyRoles:               opts.OnlyRoles,
		OutputTargets:           opts.OutputTargets,
		ProviderFunctionTracker: providerFunctionTracker,
	}).Build(ctx, addrs.RootModuleInstance)
	diags = diags.Append(graphDiags)
	if diags.HasErrors() {
		return nil, diags
	}

	inputState := prevState
	if inputState == nil {
		inputState = states.NewState()
	}

	walker, walkDiags := c.walk(ctx, graph, walkBuild, &graphWalkOpts{
		Config:                  config,
		InputState:              inputState,
		Changes:                 plans.NewChanges(),
		ProviderFunctionTracker: providerFunctionTracker,
	})
	diags = diags.Append(walker.NonFatalDiagnostics)
	diags = diags.Append(walkDiags)

	newState := walker.State.Close()

	// Remove orphaned resource entries from state — resources that exist in
	// state from a prior build but are no longer present in the config. This
	// prevents state from growing unboundedly as resources are added/removed.
	if newState != nil {
		pruneOrphanedBuildResources(newState, config)
	}

	return newState, diags
}

// pruneOrphanedBuildResources removes resource entries from state that have
// no corresponding declaration in the config. This is the build-mode
// equivalent of the orphan cleanup that the plan/apply path handles via
// OrphanResourceInstanceTransformer.
func pruneOrphanedBuildResources(state *states.State, config *configs.Config) {
	for _, ms := range state.Modules {
		if ms == nil {
			continue
		}

		// Find the config module that corresponds to this state module.
		modConfig := config.DescendentForInstance(ms.Addr)

		for resKey, rs := range ms.Resources {
			if modConfig == nil {
				// The entire module is gone from config — remove all its resources.
				delete(ms.Resources, resKey)
				continue
			}
			// Check if this resource still exists in config.
			if modConfig.Module.ResourceByAddr(rs.Addr.Resource) == nil {
				log.Printf("[TRACE] pruneOrphanedBuildResources: removing %s (not in config)", rs.Addr)
				delete(ms.Resources, resKey)
			}
		}
	}
}
