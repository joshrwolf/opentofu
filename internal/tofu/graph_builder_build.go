// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"log"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/dag"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// BuildGraphBuilder constructs the execution graph for the single-pass build
// mode. Unlike plan/apply, this produces a single graph walked once. Each
// resource node performs content-hash checking against persisted state and
// either skips execution (cache hit) or calls ApplyResourceChange (cache miss).
type BuildGraphBuilder struct {
	Config             *configs.Config
	State              *states.State
	RootVariableValues InputValues
	Plugins            *contextPlugins

	// Targets and Excludes restrict execution to specific resources/modules.
	// This is the primary mechanism for scoping builds within the full DAG.
	Targets  []addrs.Targetable
	Excludes []addrs.Targetable

	// SkipRoles and OnlyRoles are role-based filters passed through to
	// NodeBuildableResourceInstance nodes for runtime filtering.
	SkipRoles []providers.ResourceRole
	OnlyRoles []providers.ResourceRole

	// OutputTargets prunes the graph to the backward closure of named outputs.
	OutputTargets []string

	ProviderFunctionTracker ProviderFunctionMapping
}

func (b *BuildGraphBuilder) Build(ctx context.Context, path addrs.ModuleInstance) (*Graph, tfdiags.Diagnostics) {
	log.Printf("[TRACE] BuildGraphBuilder: building graph for walkBuild")
	return (&BasicGraphBuilder{
		Steps: b.Steps(),
		Name:  "BuildGraphBuilder",
	}).Build(ctx, path)
}

func (b *BuildGraphBuilder) Steps() []GraphTransformer {
	concreteProvider := func(a *NodeAbstractProvider) dag.Vertex {
		return &NodeApplyableProvider{
			NodeAbstractProvider: a,
		}
	}

	concreteResource := func(a *NodeAbstractResource) dag.Vertex {
		return &nodeExpandBuildableResource{
			NodeAbstractResource: a,
			skipRoles:            b.SkipRoles,
			onlyRoles:            b.OnlyRoles,
		}
	}

	steps := []GraphTransformer{
		&ConfigTransformer{
			Concrete: concreteResource,
			Config:   b.Config,
		},

		&RootVariableTransformer{Config: b.Config, RawValues: b.RootVariableValues},
		&ModuleVariableTransformer{Config: b.Config},
		&LocalTransformer{Config: b.Config},
		&OutputTransformer{
			Config:   b.Config,
			Planning: false,
		},

		&AttachStateTransformer{State: b.State},
		&AttachResourceConfigTransformer{Config: b.Config},

		transformProviders(concreteProvider, b.Config, walkBuild),
		&AttachSchemaTransformer{Plugins: b.Plugins, Config: b.Config},
		&ProviderUnconfiguredTransformer{},
		&ProviderFunctionTransformer{Config: b.Config, ProviderFunctionTracker: b.ProviderFunctionTracker},
		&PruneProviderTransformer{},

		&ModuleExpansionTransformer{Config: b.Config},

		&ReferenceTransformer{},
		&AttachDependenciesTransformer{},
		&attachResourceDependsOnTransformer{},

		&pruneUnusedNodesTransformer{Op: walkBuild},
		&OutputTargetTransformer{OutputTargets: b.OutputTargets},
		&TargetingTransformer{Targets: b.Targets, Excludes: b.Excludes},
		&CloseRootModuleTransformer{},
		&TransitiveReductionTransformer{},
	}

	return steps
}

// nodeExpandBuildableResource handles count/for_each expansion for build mode.
type nodeExpandBuildableResource struct {
	*NodeAbstractResource

	skipRoles []providers.ResourceRole
	onlyRoles []providers.ResourceRole
}

var (
	_ GraphNodeDynamicExpandable    = (*nodeExpandBuildableResource)(nil)
	_ GraphNodeReferenceable        = (*nodeExpandBuildableResource)(nil)
	_ GraphNodeReferencer           = (*nodeExpandBuildableResource)(nil)
	_ GraphNodeConfigResource       = (*nodeExpandBuildableResource)(nil)
	_ GraphNodeAttachResourceConfig = (*nodeExpandBuildableResource)(nil)
)

func (n *nodeExpandBuildableResource) DynamicExpand(evalCtx EvalContext) (*Graph, error) {
	var diags tfdiags.Diagnostics

	expander := evalCtx.InstanceExpander()
	moduleInstances := expander.ExpandModule(n.Addr.Module)

	for _, module := range moduleInstances {
		resAddr := n.Addr.Resource.Absolute(module)
		moduleCtx := evalCtx.WithPath(resAddr.Module)

		moreDiags := n.writeResourceState(context.TODO(), moduleCtx, resAddr)
		diags = diags.Append(moreDiags)
		if moreDiags.HasErrors() {
			return nil, diags.ErrWithWarnings()
		}
	}

	var g Graph
	for _, module := range moduleInstances {
		resAddr := n.Addr.Resource.Absolute(module)
		instanceAddrs := expander.ExpandResource(resAddr)

		for _, addr := range instanceAddrs {
			a := NewNodeAbstractResourceInstance(addr)
			a.Config = n.Config
			a.ResolvedProvider = n.ResolvedProvider
			a.Schema = n.Schema
			a.ProvisionerSchemas = n.ProvisionerSchemas
			a.ProviderMetas = n.ProviderMetas
			a.dependsOn = n.dependsOn
			a.Dependencies = n.dependsOn

			concrete := &NodeBuildableResourceInstance{
				NodeAbstractResourceInstance: a,
				SkipRoles:                    n.skipRoles,
				OnlyRoles:                    n.onlyRoles,
			}
			g.Add(concrete)
		}
	}

	// Attach state to instance nodes so they can look up prior objects.
	func() {
		state := evalCtx.State().Lock()
		defer evalCtx.State().Unlock()
		if err := (&AttachStateTransformer{State: state}).Transform(context.TODO(), &g); err != nil {
			diags = diags.Append(err)
		}
	}()
	if diags.HasErrors() {
		return nil, diags.ErrWithWarnings()
	}

	// Connect references for ordering within the subgraph.
	if err := (&ReferenceTransformer{}).Transform(context.TODO(), &g); err != nil {
		return nil, err
	}

	addRootNodeToGraph(&g)
	return &g, diags.ErrWithWarnings()
}
