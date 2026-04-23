// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"
	"sync"

	"github.com/hashicorp/hcl/v2"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
	"github.com/zclconf/go-cty/cty"
)

type Loaded struct {
	RootDir string
	Config  *configs.Config
	Catalog *catalog.Catalog

	sources func() map[string]*hcl.File

	repetitionMu          sync.RWMutex
	targetRepetitionCache map[catalog.AddrKey]cachedRepetitionEval
	importRepetitionCache map[catalog.ModulePathKey]cachedRepetitionEval

	evalCacheMu     sync.RWMutex
	evalCacheValues map[evalCacheKey]cty.Value
	purityMu        sync.Mutex
	purityAnalysis  map[*configs.Module]map[string]bool

	scopeMu    sync.Mutex
	scopeCache map[catalog.ModulePathKey]*scopeEntry
}

type scopeEntry struct {
	scope *configs.PreparedScope
	err   error
	done  chan struct{}
}

type repetitionKind uint8

const (
	repetitionKindNone repetitionKind = iota
	repetitionKindCount
	repetitionKindForEach
	repetitionKindEnabled
)

type cachedRepetitionEval struct {
	kind   repetitionKind
	result InstanceResult
	values map[string]cty.Value
	diags  tfdiags.Diagnostics
}

func (l *Loaded) SourceRoot() string {
	return l.RootDir
}

func (l *Loaded) Sources() map[string]*hcl.File {
	if l == nil || l.sources == nil {
		return nil
	}
	return l.sources()
}

func Load(ctx context.Context, loader *configload.Loader, req LoadRequest, call configs.StaticModuleCall) (*Loaded, tfdiags.Diagnostics) {
	ctx, span := tracing.Tracer().Start(ctx, "build.Load")
	defer span.End()

	var diags tfdiags.Diagnostics

	cfg, loadDiags := loader.LoadConfig(ctx, req.RootDir, call)
	diags = diags.Append(loadDiags)
	if cfg == nil {
		return nil, diags
	}

	cat, catalogDiags := LowerCatalog(cfg)
	diags = diags.Append(catalogDiags)

	return &Loaded{
		RootDir:               req.RootDir,
		Config:                cfg,
		Catalog:               cat,
		sources:               loader.Sources,
		targetRepetitionCache: make(map[catalog.AddrKey]cachedRepetitionEval),
		importRepetitionCache: make(map[catalog.ModulePathKey]cachedRepetitionEval),
		evalCacheValues:       make(map[evalCacheKey]cty.Value),
		purityAnalysis:        make(map[*configs.Module]map[string]bool),
		scopeCache:            make(map[catalog.ModulePathKey]*scopeEntry),
	}, diags
}
