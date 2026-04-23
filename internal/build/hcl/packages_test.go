// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/modsdir"
)

func TestPackageInstancesUsesConcreteRepeatedParentModule(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "parent" {
  source   = "./parent"
  for_each = {
    amd64 = "linux/amd64"
  }
}
`)
	writeTestFile(t, filepath.Join(rootDir, "parent", "main.tf"), `
module "child" {
  source = "../child"
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
output "image" {
  value = "busybox"
}
`)
	modulesDir := filepath.Join(rootDir, ".terraform", "modules")
	if err := os.MkdirAll(modulesDir, 0o755); err != nil {
		t.Fatalf("failed to create modules dir: %s", err)
	}
	manifest := modsdir.Manifest{
		"": modsdir.Record{
			Key: "",
			Dir: rootDir,
		},
		"parent": modsdir.Record{
			Key:        "parent",
			SourceAddr: "./parent",
			Dir:        filepath.Join(rootDir, "parent"),
		},
		"parent.child": modsdir.Record{
			Key:        "parent.child",
			SourceAddr: "../child",
			Dir:        filepath.Join(rootDir, "child"),
		},
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		t.Fatalf("failed to write modules manifest: %s", err)
	}

	loaded := loadTestConfig(t, rootDir)
	solver := newTestSolver(t, loaded)

	modules, diags := solver.PackageInstances(t.Context(), catalog.RootModule().Child("parent", catalog.StringKey("amd64")).Child("child", catalog.NoKey()))
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}

	want := []catalog.ModulePath{
		catalog.RootModule().Child("parent", catalog.StringKey("amd64")).Child("child", catalog.NoKey()),
	}
	if diff := cmp.Diff(want, modules.Modules, cmp.Comparer(func(a, b catalog.ModulePath) bool {
		return a.Identity() == b.Identity()
	})); diff != "" {
		t.Fatalf("wrong nested package instances (-want +got):\n%s", diff)
	}
}

func TestPackageInstancesUsesConcreteSingleRepeatedParentWhenEvaluatingNestedModuleInputs(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "parent" {
  source   = "./parent"
  for_each = {
    amd64 = {
      image = "busybox"
    }
  }

  image = each.value.image
}
`)
	writeTestFile(t, filepath.Join(rootDir, "parent", "main.tf"), `
variable "image" {
  type = string
}

module "child" {
  source = "../child"
  count  = var.image != "" ? 1 : 0
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
output "image" {
  value = "busybox"
}
`)
	modulesDir := filepath.Join(rootDir, ".terraform", "modules")
	if err := os.MkdirAll(modulesDir, 0o755); err != nil {
		t.Fatalf("failed to create modules dir: %s", err)
	}
	manifest := modsdir.Manifest{
		"": modsdir.Record{
			Key: "",
			Dir: rootDir,
		},
		"parent": modsdir.Record{
			Key:        "parent",
			SourceAddr: "./parent",
			Dir:        filepath.Join(rootDir, "parent"),
		},
		"parent.child": modsdir.Record{
			Key:        "parent.child",
			SourceAddr: "../child",
			Dir:        filepath.Join(rootDir, "child"),
		},
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		t.Fatalf("failed to write modules manifest: %s", err)
	}

	loaded := loadTestConfig(t, rootDir)
	solver := newTestSolver(t, loaded)

	modules, diags := solver.PackageInstances(t.Context(), catalog.RootModule().Child("parent", catalog.NoKey()).Child("child", catalog.NoKey()))
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}

	want := []catalog.ModulePath{
		catalog.RootModule().Child("parent", catalog.StringKey("amd64")).Child("child", catalog.IntKey(0)),
	}
	if diff := cmp.Diff(want, modules.Modules, cmp.Comparer(func(a, b catalog.ModulePath) bool {
		return a.Identity() == b.Identity()
	})); diff != "" {
		t.Fatalf("wrong nested package instances with repeated parent input (-want +got):\n%s", diff)
	}
}
