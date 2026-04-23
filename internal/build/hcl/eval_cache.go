// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/configs"
)

type evalCacheKey struct {
	Module catalog.ModulePathKey
	Name   string
}

type evalCache struct {
	loaded   *Loaded
	module   catalog.ModulePath
	eligible map[string]bool
}

func (c *evalCache) Lookup(name string) (cty.Value, bool) {
	if !c.eligible[name] {
		return cty.NilVal, false
	}
	c.loaded.evalCacheMu.RLock()
	defer c.loaded.evalCacheMu.RUnlock()
	val, ok := c.loaded.evalCacheValues[evalCacheKey{c.module.Identity(), name}]
	return val, ok
}

func (c *evalCache) Store(name string, val cty.Value) {
	if !c.eligible[name] {
		return
	}
	c.loaded.evalCacheMu.Lock()
	defer c.loaded.evalCacheMu.Unlock()
	c.loaded.evalCacheValues[evalCacheKey{c.module.Identity(), name}] = val
}

func (l *Loaded) localEligibility(mod *configs.Module) map[string]bool {
	l.purityMu.Lock()
	defer l.purityMu.Unlock()
	if result, ok := l.purityAnalysis[mod]; ok {
		return result
	}
	result := analyzeLocalPurity(mod)
	l.purityAnalysis[mod] = result
	return result
}

var impureFunctions = map[string]bool{
	"bcrypt":    true,
	"timestamp": true,
	"uuid":      true,
}

var pureRootNames = map[string]bool{
	"var":       true,
	"path":      true,
	"terraform": true,
}

func analyzeLocalPurity(mod *configs.Module) map[string]bool {
	if mod == nil || len(mod.Locals) == 0 {
		return nil
	}

	eligible := make(map[string]bool, len(mod.Locals))
	deps := make(map[string][]string)

	for name, local := range mod.Locals {
		pure, localDeps := analyzeExpression(local.Expr)
		eligible[name] = pure
		if len(localDeps) > 0 {
			deps[name] = localDeps
		}
	}

	changed := true
	for changed {
		changed = false
		for name, pure := range eligible {
			if !pure {
				continue
			}
			for _, dep := range deps[name] {
				if depPure, ok := eligible[dep]; ok && !depPure {
					eligible[name] = false
					changed = true
					break
				}
				if _, ok := eligible[dep]; !ok {
					eligible[name] = false
					changed = true
					break
				}
			}
		}
	}

	return eligible
}

func analyzeExpression(expr hcl.Expression) (pure bool, localDeps []string) {
	if expr == nil {
		return true, nil
	}

	for _, traversal := range expr.Variables() {
		root := traversal.RootName()
		if root == "local" {
			if len(traversal) > 1 {
				if attr, ok := traversal[1].(hcl.TraverseAttr); ok {
					localDeps = append(localDeps, attr.Name)
				}
			}
			continue
		}
		if !pureRootNames[root] {
			return false, nil
		}
	}

	if fexpr, ok := expr.(hcl.ExpressionWithFunctions); ok {
		for _, fn := range fexpr.Functions() {
			name := functionName(fn)
			if impureFunctions[name] || strings.HasPrefix(name, "provider::") {
				return false, nil
			}
		}
	}

	return true, localDeps
}

func functionName(traversal hcl.Traversal) string {
	if len(traversal) == 0 {
		return ""
	}
	return traversal.RootName()
}

// sync.Mutex is used instead of sync.RWMutex for purityAnalysis because
// writes only happen on the first access per *configs.Module (rare).
var _ configs.EvalCache = (*evalCache)(nil)
