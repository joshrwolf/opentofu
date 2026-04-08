// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"

	version "github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
)

// TargetFilter determines which parts of the config tree are relevant
// to the current build. When no targets are set, everything is relevant.
//
// TargetFilter is read-only after construction and safe for concurrent use.
type TargetFilter struct {
	targets []addrs.Targetable
}

// NewTargetFilter creates a filter from the given target list.
// A nil or empty list means "everything is targeted" (full build).
func NewTargetFilter(targets []addrs.Targetable) *TargetFilter {
	return &TargetFilter{targets: targets}
}

// Active reports whether targeting is enabled.
func (f *TargetFilter) Active() bool {
	return len(f.targets) > 0
}

// ModuleRelevant reports whether expanding a child module call is
// necessary to satisfy any target. Used during expansion to prune
// entire subtrees before they're expanded.
//
// Returns true if:
//   - no targets are set (full build)
//   - any target is inside the child module (we must expand to reach it)
//   - any target contains the child module (everything inside is wanted)
func (f *TargetFilter) ModuleRelevant(parentModule addrs.Module, callName string) bool {
	if !f.Active() {
		return true
	}

	childModule := parentModule.Child(callName)

	for _, t := range f.targets {
		// Generalize instance-level targets to static module paths
		// for comparison against the config-level module call.
		target := generalizeTarget(t)

		// Target is broader than or equal to this module — everything inside is wanted.
		if target.TargetContains(childModule) {
			return true
		}
		// Target is deeper than this module — we must expand to reach it.
		if childModule.TargetContains(target) {
			return true
		}
	}
	return false
}

// VertexTargeted reports whether a graph vertex is directly addressed
// by any target. Used during graph subgraph extraction.
//
// Provider configs are always considered targeted (they're infrastructure).
// Variables, locals, and outputs are targeted if their containing module is.
// Resources are checked at both instance and config level.
func (f *TargetFilter) VertexTargeted(v *Vertex) bool {
	if !f.Active() {
		return true
	}

	switch v.Kind {
	case KindProviderConfig:
		return true

	case KindModuleExpand, KindModuleClose:
		// Module gate vertices are targeted if their module is targeted.
		modulePath := v.Module.Module()
		for _, t := range f.targets {
			target := generalizeTarget(t)
			if target.TargetContains(modulePath) {
				return true
			}
		}

	case KindResource, KindDataSource:
		for _, t := range f.targets {
			if t.TargetContains(v.ResourceAddr) {
				return true
			}
			if t.TargetContains(v.ResourceAddr.ContainingResource().Config()) {
				return true
			}
		}

	case KindVariable, KindLocal, KindOutput:
		modulePath := v.Module.Module()
		for _, t := range f.targets {
			target := generalizeTarget(t)
			if target.TargetContains(modulePath) {
				return true
			}
			// Note: we intentionally do NOT check modulePath.TargetContains(target)
			// here. That reverse check is correct for expansion pruning (ModuleRelevant)
			// where we need to know "should we expand the root to reach crane?", but
			// wrong for vertex targeting. A root-level output is not part of
			// -target=module.crane just because the root module contains crane.
			// Such vertices are included only if they're transitive dependencies
			// of targeted vertices (Step 2 of FilterTargets).
		}
	}

	return false
}

// WrapWalker wraps a configs.ModuleWalker to skip module subtrees that
// are irrelevant to the targets. When no targets are set, the base walker
// is returned unchanged — zero overhead for full builds.
//
// This is used during config loading to avoid parsing .tf files for
// modules that won't be expanded or walked.
func (f *TargetFilter) WrapWalker(base configs.ModuleWalker) configs.ModuleWalker {
	if !f.Active() {
		return base
	}
	return configs.ModuleWalkerFunc(func(ctx context.Context, req *configs.ModuleRequest) (*configs.Module, *version.Version, hcl.Diagnostics) {
		if !f.modulePathRelevant(req.Path) {
			return nil, nil, nil
		}
		return base.LoadModule(ctx, req)
	})
}

// modulePathRelevant checks whether a module path (from a ModuleRequest)
// could contain or be contained by any target.
func (f *TargetFilter) modulePathRelevant(path addrs.Module) bool {
	for _, t := range f.targets {
		target := generalizeTarget(t)
		if target.TargetContains(path) {
			return true
		}
		if path.TargetContains(target) {
			return true
		}
	}
	return false
}

// generalizeTarget converts instance-level targets to their static
// equivalents for comparison against config-level module paths that
// don't have instance keys yet.
//
//   - ModuleInstance → Module (drop instance keys)
//   - AbsResourceInstance → ConfigResource (drop instance key + module keys)
//   - AbsResource → ConfigResource (drop module instance keys)
func generalizeTarget(t addrs.Targetable) addrs.Targetable {
	switch addr := t.(type) {
	case addrs.ModuleInstance:
		return addr.Module()
	case addrs.AbsResourceInstance:
		return addr.ContainingResource().Config()
	case addrs.AbsResource:
		return addr.Config()
	default:
		return t
	}
}
