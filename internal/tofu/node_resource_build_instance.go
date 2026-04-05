// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// NodeBuildableResourceInstance is the graph node for a resource instance in
// the build execution mode. It performs a single-pass hash-check-then-apply:
//
//  1. Evaluate the resource's configuration (resolving all expressions)
//  2. Collect content hashes of upstream dependencies from state
//  3. Compute a content hash for this resource
//  4. If the hash matches the prior state → cache hit, skip
//  5. If not → call ApplyResourceChange, update state with new hash
type NodeBuildableResourceInstance struct {
	*NodeAbstractResourceInstance

	// SkipRoles and OnlyRoles control role-based filtering at execution time.
	// These are checked against the provider's ResourceMeta role declaration.
	SkipRoles []providers.ResourceRole
	OnlyRoles []providers.ResourceRole
}

var (
	_ GraphNodeExecutable       = (*NodeBuildableResourceInstance)(nil)
	_ GraphNodeReferenceable    = (*NodeBuildableResourceInstance)(nil)
	_ GraphNodeReferencer       = (*NodeBuildableResourceInstance)(nil)
	_ GraphNodeResourceInstance = (*NodeBuildableResourceInstance)(nil)
)

func (n *NodeBuildableResourceInstance) Execute(ctx context.Context, evalCtx EvalContext, op walkOperation) tfdiags.Diagnostics {
	addr := n.ResourceInstanceAddr()

	switch addr.Resource.Resource.Mode {
	case addrs.ManagedResourceMode:
		return n.managedResourceExecute(ctx, evalCtx)
	case addrs.DataResourceMode:
		return n.dataResourceExecute(ctx, evalCtx)
	default:
		var diags tfdiags.Diagnostics
		diags = diags.Append(fmt.Errorf("unsupported resource mode %s in build execution", addr.Resource.Resource.Mode))
		return diags
	}
}

func (n *NodeBuildableResourceInstance) managedResourceExecute(ctx context.Context, evalCtx EvalContext) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	addr := n.ResourceInstanceAddr()
	resource := addr.Resource.Resource

	log.Printf("[TRACE] NodeBuildableResourceInstance: executing %s", addr)

	provider, providerSchema, err := n.getProvider(ctx, evalCtx)
	if err != nil {
		return diags.Append(err)
	}

	// Fetch resource metadata once for both role filtering and cache policy.
	meta := providers.GetResourceMetaFromProvider(provider, resource.Type)

	if n.shouldSkipByRole(meta.Role) {
		log.Printf("[TRACE] NodeBuildableResourceInstance: %s skipped by role filter", addr)
		return diags
	}

	schema, _ := providerSchema.SchemaForResourceAddr(resource)
	if schema == nil {
		diags = diags.Append(fmt.Errorf("provider does not support resource type %q", resource.Type))
		return diags
	}

	if n.Config == nil {
		diags = diags.Append(fmt.Errorf("resource %s has no configuration", addr))
		return diags
	}

	forEach, forEachDiags := evaluateForEachExpression(ctx, n.Config.ForEach, evalCtx, n.Addr)
	diags = diags.Append(forEachDiags)
	if diags.HasErrors() {
		return diags
	}
	keyData := EvalDataForInstanceKey(n.ResourceInstanceAddr().Resource.Key, forEach)

	configVal, _, configDiags := evalCtx.EvaluateBlock(ctx, n.Config.Config, schema.Block, nil, keyData)
	diags = diags.Append(configDiags)
	if diags.HasErrors() {
		return diags
	}

	refs := n.References()
	absRefs := referencedAbsResources(refs, n.Addr.Module)
	depHashes := n.collectDependencyHashes(evalCtx, absRefs)
	contentHash := ComputeContentHash(resource.Type, configVal, depHashes)

	state := evalCtx.State()
	priorObjSrc := state.ResourceInstanceObject(addr, states.CurrentGen)

	if priorObjSrc != nil && priorObjSrc.ContentHash != "" {
		log.Printf("[TRACE] NodeBuildableResourceInstance: %s inputHash=%s stateHash=%s",
			addr, contentHash[:12], priorObjSrc.ContentHash[:12])
	}

	cached := n.isCacheValid(priorObjSrc, contentHash, meta.CachePolicy)

	if cached {
		log.Printf("[TRACE] NodeBuildableResourceInstance: %s cache hit (hash %s…)", addr, contentHash[:12])

		// Decode prior state for hook display.
		var priorVal cty.Value
		if priorObjSrc != nil {
			if priorObj, decErr := priorObjSrc.Decode(schema.Block.ImpliedType()); decErr == nil {
				priorVal = priorObj.Value
			}
		}

		diags = diags.Append(evalCtx.Hook(func(h Hook) (HookAction, error) {
			return h.PostApply(addr, states.NotDeposed, priorVal, nil)
		}))
		return diags
	}

	log.Printf("[TRACE] NodeBuildableResourceInstance: %s cache miss, executing provider", addr)

	unmarkedConfigVal, _ := configVal.UnmarkDeep()
	unmarkedConfigVal = cty.UnknownAsNull(unmarkedConfigVal)

	var priorVal cty.Value
	var priorPrivate []byte
	if priorObjSrc != nil {
		if priorObj, decErr := priorObjSrc.Decode(schema.Block.ImpliedType()); decErr == nil {
			priorVal = priorObj.Value
			priorPrivate = priorObj.Private
		}
	}
	if priorVal == cty.NilVal {
		priorVal = cty.NullVal(schema.Block.ImpliedType())
	}

	// Pre-apply hook.
	action := plans.Create
	if priorObjSrc != nil {
		action = plans.Update
	}
	diags = diags.Append(evalCtx.Hook(func(h Hook) (HookAction, error) {
		return h.PreApply(addr, states.NotDeposed, action, priorVal, unmarkedConfigVal)
	}))
	if diags.HasErrors() {
		return diags
	}

	metaConfigVal, metaDiags := n.providerMetas(ctx, evalCtx)
	diags = diags.Append(metaDiags)
	if diags.HasErrors() {
		return diags
	}

	resp := provider.ApplyResourceChange(ctx, providers.ApplyResourceChangeRequest{
		TypeName:       resource.Type,
		PriorState:     priorVal,
		PlannedState:   unmarkedConfigVal,
		Config:         unmarkedConfigVal,
		PlannedPrivate: priorPrivate,
		ProviderMeta:   metaConfigVal,
	})
	diags = diags.Append(resp.Diagnostics.InConfigBody(n.Config.Config, addr.String()))

	newVal := resp.NewState
	if newVal == cty.NilVal {
		newVal = cty.NullVal(schema.Block.ImpliedType())
	}
	newVal, _ = newVal.UnmarkDeep()

	var newStatus states.ObjectStatus
	if diags.HasErrors() {
		newStatus = states.ObjectTainted
	} else {
		newStatus = states.ObjectReady
	}

	newObj := &states.ResourceInstanceObject{
		Value:        newVal,
		Private:      resp.Private,
		Status:       newStatus,
		Dependencies: n.Dependencies,
		References: func() []addrs.ConfigResource {
			configRefs := make([]addrs.ConfigResource, len(absRefs))
			for i, r := range absRefs {
				configRefs[i] = r.Config()
			}
			return configRefs
		}(),
		ContentHash:  contentHash,
		CachedAt:     time.Now().UTC(),
	}

	if writeErr := n.writeResourceInstanceState(ctx, evalCtx, newObj, workingState); writeErr != nil {
		diags = diags.Append(writeErr)
	}

	// Post-apply hook.
	var applyErr error
	if diags.HasErrors() {
		applyErr = diags.Err()
	}
	diags = diags.Append(evalCtx.Hook(func(h Hook) (HookAction, error) {
		return h.PostApply(addr, states.NotDeposed, newVal, applyErr)
	}))

	return diags
}

// dataResourceExecute handles data sources in build mode.
// Data sources are always read (never cached).
func (n *NodeBuildableResourceInstance) dataResourceExecute(ctx context.Context, evalCtx EvalContext) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	addr := n.ResourceInstanceAddr()
	resource := addr.Resource.Resource

	provider, providerSchema, err := n.getProvider(ctx, evalCtx)
	if err != nil {
		return diags.Append(err)
	}

	schema, _ := providerSchema.SchemaForResourceAddr(resource)
	if schema == nil {
		diags = diags.Append(fmt.Errorf("provider does not support data source %q", resource.Type))
		return diags
	}

	if n.Config == nil {
		diags = diags.Append(fmt.Errorf("data source %s has no configuration", addr))
		return diags
	}

	forEach, forEachDiags := evaluateForEachExpression(ctx, n.Config.ForEach, evalCtx, n.Addr)
	diags = diags.Append(forEachDiags)
	if diags.HasErrors() {
		return diags
	}
	keyData := EvalDataForInstanceKey(n.ResourceInstanceAddr().Resource.Key, forEach)

	configVal, _, configDiags := evalCtx.EvaluateBlock(ctx, n.Config.Config, schema.Block, nil, keyData)
	diags = diags.Append(configDiags)
	if diags.HasErrors() {
		return diags
	}

	unmarkedConfigVal, _ := configVal.UnmarkDeep()
	unmarkedConfigVal = cty.UnknownAsNull(unmarkedConfigVal)

	resp := provider.ReadDataSource(ctx, providers.ReadDataSourceRequest{
		TypeName: resource.Type,
		Config:   unmarkedConfigVal,
	})
	diags = diags.Append(resp.Diagnostics.InConfigBody(n.Config.Config, addr.String()))
	if diags.HasErrors() {
		return diags
	}

	newVal := resp.State
	if newVal == cty.NilVal {
		newVal = cty.NullVal(schema.Block.ImpliedType())
	}
	newVal, _ = newVal.UnmarkDeep()

	newObj := &states.ResourceInstanceObject{
		Value:  newVal,
		Status: states.ObjectReady,
	}
	if writeErr := n.writeResourceInstanceState(ctx, evalCtx, newObj, workingState); writeErr != nil {
		diags = diags.Append(writeErr)
	}

	return diags
}

// referencedAbsResources extracts deduplicated absolute resource addresses
// from a set of references, resolving them against the given module instance.
func referencedAbsResources(refs []*addrs.Reference, module addrs.ModuleInstance) []addrs.AbsResource {
	var result []addrs.AbsResource
	seen := map[string]bool{}
	for _, ref := range refs {
		var resAddr addrs.Resource
		switch subject := ref.Subject.(type) {
		case addrs.Resource:
			resAddr = subject
		case addrs.ResourceInstance:
			resAddr = subject.Resource
		default:
			continue
		}
		absRes := resAddr.Absolute(module)
		key := absRes.String()
		if !seen[key] {
			seen[key] = true
			result = append(result, absRes)
		}
	}
	return result
}

// collectDependencyHashes reads ContentHash from all referenced resources in
// state. This captures both implicit (expression) and explicit (depends_on)
// references, ensuring content hash cascading through the full DAG.
func (n *NodeBuildableResourceInstance) collectDependencyHashes(evalCtx EvalContext, absRefs []addrs.AbsResource) map[string]string {
	hashes := make(map[string]string)
	state := evalCtx.State()

	for _, absRes := range absRefs {
		depResource := state.Resource(absRes)
		if depResource == nil {
			continue
		}
		for key := range depResource.Instances {
			instAddr := absRes.Instance(key)
			obj := state.ResourceInstanceObject(instAddr, states.CurrentGen)
			if obj != nil && obj.ContentHash != "" {
				hashes[instAddr.String()] = obj.ContentHash
			}
		}
	}

	return hashes
}

// shouldSkipByRole checks whether this resource should be skipped based on
// the --skip/--only role filtering options.
func (n *NodeBuildableResourceInstance) shouldSkipByRole(role providers.ResourceRole) bool {
	if len(n.OnlyRoles) == 0 && len(n.SkipRoles) == 0 {
		return false
	}
	if len(n.OnlyRoles) > 0 {
		return !slices.Contains(n.OnlyRoles, role)
	}
	return slices.Contains(n.SkipRoles, role)
}

// isCacheValid determines whether the prior state is a valid cache hit.
func (n *NodeBuildableResourceInstance) isCacheValid(prior *states.ResourceInstanceObjectSrc, contentHash string, policy providers.CachePolicy) bool {
	if prior == nil || prior.ContentHash != contentHash {
		return false
	}
	switch policy.Mode {
	case providers.CacheByInputs:
		return true
	case providers.CacheWithTTL:
		return time.Since(prior.CachedAt) < policy.TTL
	case providers.NeverCache:
		return false
	default:
		return true
	}
}
