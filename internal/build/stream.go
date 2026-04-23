// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"

	"github.com/opentofu/opentofu/internal/build/catalog"
	buildhcl "github.com/opentofu/opentofu/internal/build/hcl"
	"github.com/opentofu/opentofu/internal/build/selector"
	"github.com/opentofu/opentofu/internal/build/solve"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type Loaded = buildhcl.Loaded
type LoadRequest = buildhcl.LoadRequest

func Load(ctx context.Context, loader *configload.Loader, req LoadRequest, call configs.StaticModuleCall) (*Loaded, tfdiags.Diagnostics) {
	return buildhcl.Load(ctx, loader, req, call)
}

func ParseSelectors(raw []string) (selector.Set, tfdiags.Diagnostics) {
	if len(raw) == 0 {
		raw = []string{"**"}
	}
	set, err := selector.ParseAll(raw)
	if err != nil {
		return set, tfdiags.Diagnostics{}.Append(err)
	}
	return set, nil
}

func StreamCandidateMatches(cat *catalog.Catalog, patterns selector.Set, yield func(catalog.Addr) bool) {
	if cat == nil || cat.Root == nil || yield == nil {
		return
	}

	var walk func(*catalog.Package) bool
	walk = func(pkg *catalog.Package) bool {
		moduleAddr := catalog.ModuleAddr(pkg.Module)
		m := patterns.Match(moduleAddr)
		if !m.Possible() {
			return true
		}

		if m.Declaration() && !yield(moduleAddr) {
			return false
		}
		for _, target := range pkg.Targets {
			if patterns.Match(target.Addr).Declaration() && !yield(target.Addr) {
				return false
			}
		}
		for _, output := range pkg.Outputs {
			if patterns.Match(output.Addr).Declaration() && !yield(output.Addr) {
				return false
			}
		}

		for _, name := range pkg.ChildOrder {
			child := pkg.Children[name]
			if !patterns.Match(catalog.ModuleAddr(child.Module)).Possible() {
				continue
			}
			if !walk(child) {
				return false
			}
		}
		return true
	}

	walk(cat.Root)
}

func StreamSelections(ctx context.Context, cat *catalog.Catalog, solver *solve.Solver, patterns selector.Set, yield func(catalog.Addr) bool) tfdiags.Diagnostics {
	if cat == nil || cat.Root == nil || solver == nil || yield == nil {
		return nil
	}

	var diags tfdiags.Diagnostics
	seen := map[catalog.AddrKey]struct{}{}

	var walk func(pkg *catalog.Package, module catalog.ModulePath, deferred bool) bool
	walk = func(pkg *catalog.Package, module catalog.ModulePath, deferred bool) bool {
		moduleAddr := catalog.ModuleAddr(module)
		if !patterns.Match(moduleAddr).Possible() {
			return true
		}

		if patterns.Match(moduleAddr).Exact() {
			if !yieldUniqueAddr(moduleAddr, seen, yield) {
				return false
			}
		}

		for _, target := range pkg.Targets {
			if !target.Addr.Actionable() {
				continue
			}

			declAddr := target.Addr
			declAddr.Module = module
			declAddr.Key = catalog.NoKey()

			if deferred {
				if patterns.Match(declAddr).Exact() {
					if !yieldUniqueAddr(declAddr, seen, yield) {
						return false
					}
				}
				continue
			}

			instances, instanceDiags := solver.TargetInstances(ctx, declAddr)
			diags = diags.Append(instanceDiags)
			if instanceDiags.HasErrors() {
				continue
			}
			for _, instance := range instances.Addrs {
				if patterns.Match(instance).Exact() {
					if !yieldUniqueAddr(instance, seen, yield) {
						return false
					}
				}
			}
		}

		for _, name := range pkg.ChildOrder {
			child := pkg.Children[name]
			childDecl := module.Child(name, catalog.NoKey())
			if !patterns.Match(catalog.ModuleAddr(childDecl)).Possible() {
				continue
			}
			if deferred {
				if !walk(child, childDecl, true) {
					return false
				}
				continue
			}
			packages, packageDiags := solver.PackageInstances(ctx, childDecl)
			diags = diags.Append(packageDiags)
			if packageDiags.HasErrors() {
				continue
			}
			for _, childModule := range packages.Modules {
				if !walk(child, childModule, false) {
					return false
				}
			}
		}

		for _, output := range pkg.Outputs {
			concrete := output.Addr
			concrete.Module = module
			if patterns.Match(concrete).Exact() {
				if !yieldUniqueAddr(concrete, seen, yield) {
					return false
				}
			}
		}

		return true
	}

	walk(cat.Root, catalog.RootModule(), false)
	return diags
}

func yieldUniqueAddr(addr catalog.Addr, seen map[catalog.AddrKey]struct{}, yield func(catalog.Addr) bool) bool {
	key := addr.Identity()
	if _, ok := seen[key]; ok {
		return true
	}
	seen[key] = struct{}{}
	return yield(addr)
}
