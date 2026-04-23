// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"slices"

	"github.com/opentofu/opentofu/internal/addrs"
)

type Catalog struct {
	Root              *Package
	packages          map[ModulePathKey]*Package
	imports           map[ModulePathKey]*Import
	targets           map[AddrKey]*TargetDecl
	outputs           map[AddrKey]*OutputDecl
	providers         map[ProviderKey]*ProviderDecl
	actionableSubtree map[ModulePathKey][]*TargetDecl
}

func New(root *Package) *Catalog {
	c := &Catalog{
		Root:              root,
		packages:          map[ModulePathKey]*Package{},
		imports:           map[ModulePathKey]*Import{},
		targets:           map[AddrKey]*TargetDecl{},
		outputs:           map[AddrKey]*OutputDecl{},
		providers:         map[ProviderKey]*ProviderDecl{},
		actionableSubtree: map[ModulePathKey][]*TargetDecl{},
	}
	if root != nil {
		c.indexPackage(root)
		c.indexActionableTargets(root)
	}
	return c
}

func (c *Catalog) indexPackage(pkg *Package) {
	c.packages[pkg.Module.Identity()] = pkg
	for i := range pkg.Imports {
		imp := &pkg.Imports[i]
		c.imports[imp.TargetModule.Identity()] = imp
	}

	for _, target := range pkg.Targets {
		c.targets[target.Addr.Identity()] = target
	}
	for _, output := range pkg.Outputs {
		c.outputs[output.Addr.Identity()] = output
	}
	for _, provider := range pkg.Providers {
		c.providers[provider.Identity()] = provider
	}

	childNames := make([]string, 0, len(pkg.Children))
	for name := range pkg.Children {
		childNames = append(childNames, name)
	}
	slices.Sort(childNames)
	pkg.ChildOrder = childNames

	for _, name := range pkg.ChildOrder {
		c.indexPackage(pkg.Children[name])
	}
}

func (c *Catalog) Package(module ModulePath) (*Package, bool) {
	if c == nil {
		return nil, false
	}
	pkg, ok := c.packages[module.Identity()]
	return pkg, ok
}

func (c *Catalog) Target(addr Addr) (*TargetDecl, bool) {
	if c == nil {
		return nil, false
	}
	target, ok := c.targets[addr.Identity()]
	return target, ok
}

func (c *Catalog) Import(module ModulePath) (*Import, bool) {
	if c == nil {
		return nil, false
	}
	imp, ok := c.imports[module.Identity()]
	return imp, ok
}

func (c *Catalog) Output(addr Addr) (*OutputDecl, bool) {
	if c == nil {
		return nil, false
	}
	output, ok := c.outputs[addr.Identity()]
	return output, ok
}

func (c *Catalog) Provider(module ModulePath, local addrs.LocalProviderConfig) (*ProviderDecl, bool) {
	if c == nil {
		return nil, false
	}
	provider, ok := c.providers[ProviderKey{
		Module:    module.Identity(),
		LocalName: local.LocalName,
		Alias:     local.Alias,
	}]
	return provider, ok
}

func (c *Catalog) WalkPackages(fn func(*Package)) {
	if c == nil || c.Root == nil || fn == nil {
		return
	}
	var walk func(*Package)
	walk = func(pkg *Package) {
		fn(pkg)
		for _, name := range pkg.ChildOrder {
			walk(pkg.Children[name])
		}
	}
	walk(c.Root)
}

func (c *Catalog) ActionableTargetsInModule(module ModulePath, recursive bool) []*TargetDecl {
	pkg, ok := c.Package(module)
	if !ok {
		return nil
	}

	if recursive {
		return c.actionableSubtree[module.Identity()]
	}

	var ret []*TargetDecl
	for _, target := range pkg.Targets {
		if target.Addr.Actionable() {
			ret = append(ret, target)
		}
	}
	return ret
}

func (c *Catalog) indexActionableTargets(pkg *Package) []*TargetDecl {
	ret := make([]*TargetDecl, 0, len(pkg.Targets))
	for _, target := range pkg.Targets {
		if target.Addr.Actionable() {
			ret = append(ret, target)
		}
	}

	for _, name := range pkg.ChildOrder {
		ret = append(ret, c.indexActionableTargets(pkg.Children[name])...)
	}

	c.actionableSubtree[pkg.Module.Identity()] = ret
	return ret
}
