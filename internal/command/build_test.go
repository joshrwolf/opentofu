// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/opentofu/opentofu/internal/command/arguments"
	"github.com/opentofu/opentofu/internal/command/workdir"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tofu"
)

func TestBuild(t *testing.T) {
	td := t.TempDir()
	testCopyDir(t, testFixturePath("build-basic"), td)
	t.Chdir(td)

	p := buildFixtureProvider()

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir("."),
			testingOverrides: metaOverridesForProvider(p),
			View:             view,
		},
	}

	code := c.Run(nil)
	output := done(t)
	if code != 0 {
		t.Fatalf("bad exit code: %d\n\n%s", code, output.Stderr())
	}

	// State should have been written to the default location.
	state := testStateRead(t, filepath.Join(td, arguments.DefaultStateFilename))
	if state == nil {
		t.Fatal("state should not be nil")
	}

	mod := state.RootModule()
	if len(mod.Resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(mod.Resources))
	}

	for _, rs := range mod.Resources {
		for key, inst := range rs.Instances {
			if inst.Current == nil || inst.Current.ContentHash == "" {
				t.Errorf("resource %s[%s] missing content hash", rs.Addr, key)
			}
		}
	}
}

func TestBuild_cacheHit(t *testing.T) {
	td := t.TempDir()
	testCopyDir(t, testFixturePath("build-basic"), td)
	t.Chdir(td)

	var applyCount int32
	p := buildFixtureProviderCounted(&applyCount)

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir("."),
			testingOverrides: metaOverridesForProvider(p),
			View:             view,
		},
	}

	// First build.
	code := c.Run(nil)
	output := done(t)
	if code != 0 {
		t.Fatalf("first build failed: %d\n\n%s", code, output.Stderr())
	}
	if n := atomic.LoadInt32(&applyCount); n != 2 {
		t.Fatalf("expected 2 applies on first build, got %d", n)
	}

	// Second build — same config, same state → cache hit.
	atomic.StoreInt32(&applyCount, 0)
	view2, done2 := testView(t)
	c2 := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir("."),
			testingOverrides: metaOverridesForProvider(p),
			View:             view2,
		},
	}
	code = c2.Run(nil)
	output = done2(t)
	if code != 0 {
		t.Fatalf("second build failed: %d\n\n%s", code, output.Stderr())
	}
	if n := atomic.LoadInt32(&applyCount); n != 0 {
		t.Fatalf("expected 0 applies on second build (cache hit), got %d", n)
	}
}

func TestBuild_outputTarget(t *testing.T) {
	td := t.TempDir()
	testCopyDir(t, testFixturePath("build-outputs"), td)
	t.Chdir(td)

	var applyCount int32
	p := buildFixtureProviderCounted(&applyCount)

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir("."),
			testingOverrides: metaOverridesForProvider(p),
			View:             view,
		},
	}

	// Target only "image_ref" — should execute 1 resource.
	code := c.Run([]string{"-o", "image_ref"})
	output := done(t)
	if code != 0 {
		t.Fatalf("build failed: %d\n\n%s", code, output.Stderr())
	}

	if n := atomic.LoadInt32(&applyCount); n != 1 {
		t.Fatalf("expected 1 apply (only build resource), got %d", n)
	}

	state := testStateRead(t, filepath.Join(td, arguments.DefaultStateFilename))
	if len(state.RootModule().Resources) != 1 {
		t.Fatalf("expected 1 resource in state, got %d", len(state.RootModule().Resources))
	}
}

func TestBuild_conflictingRoleFlags(t *testing.T) {
	td := t.TempDir()
	testCopyDir(t, testFixturePath("build-basic"), td)
	t.Chdir(td)

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir("."),
			testingOverrides: metaOverridesForProvider(testProvider()),
			View:             view,
		},
	}

	code := c.Run([]string{"-skip", "test", "-only", "build"})
	_ = done(t)
	if code == 0 {
		t.Fatal("expected non-zero exit for conflicting -skip and -only")
	}
}

func buildFixtureProvider() *tofu.MockProvider {
	// Reuse the apply fixture provider — it has the right schema and apply behavior.
	// Build mode doesn't call PlanResourceChange, but having it set is harmless.
	return applyFixtureProvider()
}

func buildFixtureProviderCounted(count *int32) *tofu.MockProvider {
	p := buildFixtureProvider()
	origApply := p.ApplyResourceChangeFn
	p.ApplyResourceChangeFn = func(req providers.ApplyResourceChangeRequest) providers.ApplyResourceChangeResponse {
		atomic.AddInt32(count, 1)
		return origApply(req)
	}
	return p
}
