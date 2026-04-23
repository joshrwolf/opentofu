// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package arguments

import "testing"

func TestParseBuildDefaults(t *testing.T) {
	build, closer, diags := ParseBuild(nil)
	defer closer()

	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if build.Parallelism != DefaultParallelism {
		t.Fatalf("wrong parallelism: %d", build.Parallelism)
	}
	if !build.UseCache {
		t.Fatal("expected cache enabled by default")
	}
	if len(build.Selectors) != 1 || build.Selectors[0].Raw != "**" {
		t.Fatalf("unexpected default selectors: %#v", build.Selectors)
	}
}

func TestParseBuildFlags(t *testing.T) {
	build, closer, diags := ParseBuild([]string{"-parallelism=3", "-no-cache", "-cache-clear", "output.image"})
	defer closer()

	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if build.Parallelism != 3 {
		t.Fatalf("wrong parallelism: %d", build.Parallelism)
	}
	if build.UseCache {
		t.Fatal("expected cache disabled")
	}
	if !build.CacheClear {
		t.Fatal("expected cache-clear to be set")
	}
	if len(build.Selectors) != 1 || build.Selectors[0].Raw != "output.image" {
		t.Fatalf("unexpected selectors: %#v", build.Selectors)
	}
}
