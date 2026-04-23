// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
	"github.com/opentofu/opentofu/internal/modsdir"
)

func TestLoadLowersCatalogStructure(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

provider "test" {
  alias = "src"
}

resource "test_instance" "base" {
  provider = test.src
}

module "child" {
  source = "./child"
  providers = {
    test.alt = test.src
  }
  depends_on = [test_instance.base]
}

output "child_result" {
  value      = module.child.result
  depends_on = [module.child]
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
      configuration_aliases = [test.alt]
    }
  }
}

resource "test_instance" "child" {
  provider = test.alt
}

output "result" {
  value      = "ok"
  depends_on = [test_instance.child]
}
`)
	writeInstalledModulesManifest(t, rootDir)

	loader, err := configload.NewLoader(&configload.Config{
		ModulesDir: filepath.Join(rootDir, ".terraform", "modules"),
	})
	if err != nil {
		t.Fatalf("failed to create loader: %s", err)
	}

	loaded, diags := Load(t.Context(), loader, LoadRequest{RootDir: rootDir}, configs.RootModuleCallForTesting())
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if loaded == nil || loaded.Catalog == nil {
		t.Fatal("expected loaded catalog")
	}

	root := catalog.RootModule()
	rootPkg, ok := loaded.Catalog.Package(root)
	if !ok {
		t.Fatal("expected root package")
	}
	if got, want := len(rootPkg.Imports), 1; got != want {
		t.Fatalf("wrong import count: got %d want %d", got, want)
	}

	childImport := rootPkg.Imports[0]
	if got, want := childImport.Name, "child"; got != want {
		t.Fatalf("wrong import name: got %q want %q", got, want)
	}
	if got, want := len(childImport.ProviderPass), 1; got != want {
		t.Fatalf("wrong provider pass count: got %d want %d", got, want)
	}
	if got, want := childImport.ProviderPass[0].InChild, (addrs.LocalProviderConfig{LocalName: "test", Alias: "alt"}); got != want {
		t.Fatalf("wrong child provider pass: got %#v want %#v", got, want)
	}
	if got, want := childImport.ProviderPass[0].InParent, (addrs.LocalProviderConfig{LocalName: "test", Alias: "src"}); got != want {
		t.Fatalf("wrong parent provider pass: got %#v want %#v", got, want)
	}
	if got, want := childImport.ExplicitDeps, []catalog.Addr{
		catalog.ResourceAddr(root, catalog.TargetKindResource, "test_instance", "base", catalog.NoKey()),
	}; len(got) != len(want) || got[0].Identity() != want[0].Identity() {
		t.Fatalf("wrong import explicit deps: got %#v want %#v", got, want)
	}

	rootOutput, ok := loaded.Catalog.Output(catalog.OutputAddr(root, "child_result"))
	if !ok {
		t.Fatal("expected root output")
	}
	if got, want := rootOutput.ExplicitDeps, []catalog.Addr{
		catalog.ModuleAddr(root.Child("child", catalog.NoKey())),
	}; len(got) != len(want) || got[0].Identity() != want[0].Identity() {
		t.Fatalf("wrong output explicit deps: got %#v want %#v", got, want)
	}

	child := root.Child("child", catalog.NoKey())
	childPkg, ok := loaded.Catalog.Package(child)
	if !ok {
		t.Fatal("expected child package")
	}
	if got, want := len(childPkg.RequiredProviders), 1; got != want {
		t.Fatalf("wrong required provider count: got %d want %d", got, want)
	}

	req := childPkg.RequiredProviders[0]
	if got, want := req.LocalName, "test"; got != want {
		t.Fatalf("wrong required provider local name: got %q want %q", got, want)
	}
	if got, want := req.Provider, addrs.NewProvider(addrs.DefaultProviderRegistryHost, "foo", "test"); !got.Equals(want) {
		t.Fatalf("wrong required provider addr: got %s want %s", got, want)
	}
	if got, want := req.Aliases, []addrs.LocalProviderConfig{{LocalName: "test", Alias: "alt"}}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("wrong required provider aliases: got %#v want %#v", got, want)
	}
}

func TestLoadReportsUnsupportedDependsOnReference(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
locals {
  wait = "nope"
}

resource "test_instance" "bad" {
  depends_on = [local.wait]
}
`)

	loader, err := configload.NewLoader(&configload.Config{
		ModulesDir: filepath.Join(rootDir, ".terraform", "modules"),
	})
	if err != nil {
		t.Fatalf("failed to create loader: %s", err)
	}

	_, diags := Load(t.Context(), loader, LoadRequest{RootDir: rootDir}, configs.RootModuleCallForTesting())
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	got := diags.Err().Error()
	if want := "Unsupported build depends_on reference"; !strings.Contains(got, want) {
		t.Fatalf("missing diagnostic %q in:\n%s", want, got)
	}
}

func TestLoadConfigKeysIgnoreCommentOnlyChurn(t *testing.T) {
	rootWithCommentA := t.TempDir()
	rootWithCommentB := t.TempDir()

	writeTestFile(t, filepath.Join(rootWithCommentA, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

provider "test" {
  alias = "src" # first comment
}

resource "test_instance" "base" {
  provider = test.src # first resource comment
}
`)
	writeTestFile(t, filepath.Join(rootWithCommentB, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

# second comment block
provider "test" {
  alias = "src"
}

resource "test_instance" "base" {
  # second resource comment
  provider = test.src
}
`)

	catalogA := loadCatalogForTest(t, rootWithCommentA)
	catalogB := loadCatalogForTest(t, rootWithCommentB)

	root := catalog.RootModule()
	rootPkgA, ok := catalogA.Package(root)
	if !ok {
		t.Fatal("expected root package for first config")
	}
	rootPkgB, ok := catalogB.Package(root)
	if !ok {
		t.Fatal("expected root package for second config")
	}
	providerA := rootPkgA.Providers[0].ConfigKey
	providerB := rootPkgB.Providers[0].ConfigKey
	if diff := cmp.Diff(providerA, providerB); diff != "" {
		t.Fatalf("provider config key changed for comment-only churn (-want +got):\n%s", diff)
	}

	targetA := rootPkgA.Targets[0].ConfigKey
	targetB := rootPkgB.Targets[0].ConfigKey
	if diff := cmp.Diff(targetA, targetB); diff != "" {
		t.Fatalf("target config key changed for comment-only churn (-want +got):\n%s", diff)
	}
}

func TestLowerCatalogMarksInvalidConfigKeys(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

resource "test_instance" "base" {
  name = "ok"
}
`)
	writeTestFile(t, filepath.Join(rootDir, "providers.tf"), `
provider "test" {
  alias = "src"
}
`)

	loader, err := configload.NewLoader(&configload.Config{
		ModulesDir: filepath.Join(rootDir, ".terraform", "modules"),
	})
	if err != nil {
		t.Fatalf("failed to create loader: %s", err)
	}

	cfg, diags := loader.LoadConfig(t.Context(), rootDir, configs.RootModuleCallForTesting())
	if diags.HasErrors() {
		t.Fatalf("unexpected config load diagnostics: %s", diags.Error())
	}
	if cfg == nil {
		t.Fatal("expected loaded config")
	}

	if err := os.Remove(filepath.Join(rootDir, "main.tf")); err != nil {
		t.Fatalf("failed to remove target source: %s", err)
	}
	if err := os.Remove(filepath.Join(rootDir, "providers.tf")); err != nil {
		t.Fatalf("failed to remove provider source: %s", err)
	}

	cat, lowerDiags := LowerCatalog(cfg)
	if !lowerDiags.HasErrors() {
		t.Fatal("expected lowering diagnostics")
	}
	if cat == nil {
		t.Fatal("expected catalog")
	}

	rootPkg, ok := cat.Package(catalog.RootModule())
	if !ok {
		t.Fatal("expected root package")
	}
	if got, want := len(rootPkg.Targets), 1; got != want {
		t.Fatalf("wrong target count: got %d want %d", got, want)
	}
	if got, want := len(rootPkg.Providers), 1; got != want {
		t.Fatalf("wrong provider count: got %d want %d", got, want)
	}
	if rootPkg.Targets[0].ConfigValid {
		t.Fatal("expected invalid target config key")
	}
	if rootPkg.Providers[0].ConfigValid {
		t.Fatal("expected invalid provider config key")
	}
}

func loadCatalogForTest(t *testing.T, rootDir string) *catalog.Catalog {
	t.Helper()

	loader, err := configload.NewLoader(&configload.Config{
		ModulesDir: filepath.Join(rootDir, ".terraform", "modules"),
	})
	if err != nil {
		t.Fatalf("failed to create loader: %s", err)
	}

	loaded, diags := Load(t.Context(), loader, LoadRequest{RootDir: rootDir}, configs.RootModuleCallForTesting())
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if loaded == nil || loaded.Catalog == nil {
		t.Fatal("expected loaded catalog")
	}
	return loaded.Catalog
}

func writeInstalledModulesManifest(t *testing.T, rootDir string) {
	t.Helper()

	modulesDir := filepath.Join(rootDir, ".terraform", "modules")
	if err := os.MkdirAll(modulesDir, 0o755); err != nil {
		t.Fatalf("failed to create modules dir: %s", err)
	}

	manifest := modsdir.Manifest{
		"": modsdir.Record{
			Key: "",
			Dir: rootDir,
		},
		"child": modsdir.Record{
			Key:        "child",
			SourceAddr: "./child",
			Dir:        filepath.Join(rootDir, "child"),
		},
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		t.Fatalf("failed to write modules manifest: %s", err)
	}
}
