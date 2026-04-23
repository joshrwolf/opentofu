// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package catalog

import (
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/digest"
)

type TargetKind string

const (
	TargetKindModule   TargetKind = "module"
	TargetKindResource TargetKind = "resource"
	TargetKindData     TargetKind = "data"
	TargetKindRun      TargetKind = "run"
	TargetKindOutput   TargetKind = "output"
)

func (k TargetKind) String() string {
	return string(k)
}

type RunnerKind string

const (
	RunnerKindProviderResource RunnerKind = "provider.resource"
	RunnerKindProviderData     RunnerKind = "provider.data"
	RunnerKindProviderFunction RunnerKind = "provider.function"
	RunnerKindBuiltinRun       RunnerKind = "builtin.run"
	RunnerKindCompatExec       RunnerKind = "compat.exec"
	RunnerKindCompatExternal   RunnerKind = "compat.external"
)

type SourcePos struct {
	Line   int
	Column int
	Byte   int
}

type SourceRef struct {
	Filename string
	Start    SourcePos
	End      SourcePos
}

type ProviderPass struct {
	InChild  addrs.LocalProviderConfig
	InParent addrs.LocalProviderConfig
}

type RequiredProviderDecl struct {
	Module    ModulePath
	LocalName string
	Provider  addrs.Provider
	Aliases   []addrs.LocalProviderConfig
	Source    SourceRef
	Payload   any
}

type Import struct {
	Name         string
	Module       ModulePath
	TargetModule ModulePath
	Source       SourceRef
	ExplicitDeps []Addr
	ProviderPass []ProviderPass
	Payload      any
}

type ProviderDecl struct {
	Module      ModulePath
	Local       addrs.LocalProviderConfig
	Provider    addrs.Provider
	Source      SourceRef
	ConfigKey   digest.Digest
	ConfigValid bool
	Payload     any
}

func (p *ProviderDecl) Identity() ProviderKey {
	return ProviderKey{
		Module:    p.Module.Identity(),
		LocalName: p.Local.LocalName,
		Alias:     p.Local.Alias,
	}
}

type RunnerSpec struct {
	Kind      RunnerKind
	Provider  addrs.Provider
	TypeName  string
	Cacheable bool
	LockKeys  []string
}

type TargetDecl struct {
	Addr          Addr
	Driver        string
	Source        SourceRef
	ConfigKey     digest.Digest
	ConfigValid   bool
	ExplicitDeps  []Addr
	ProviderLocal addrs.LocalProviderConfig
	Payload       any
	Runner        *RunnerSpec
}

type OutputDecl struct {
	Addr         Addr
	Source       SourceRef
	ExplicitDeps []Addr
	Payload      any
}

type Package struct {
	Name              string
	Module            ModulePath
	Source            SourceRef
	ProviderTypes     map[string]addrs.Provider
	Payload           any
	Parent            *Package
	ParentImport      *Import
	Children          map[string]*Package
	ChildOrder        []string
	Imports           []Import
	Targets           []*TargetDecl
	Outputs           []*OutputDecl
	Providers         []*ProviderDecl
	RequiredProviders []*RequiredProviderDecl
}

func (p *Package) ProviderForLocal(localName string) addrs.Provider {
	if p == nil {
		return addrs.ImpliedProviderForUnqualifiedType(localName)
	}
	if provider, ok := p.ProviderTypes[localName]; ok {
		return provider
	}
	return addrs.ImpliedProviderForUnqualifiedType(localName)
}
