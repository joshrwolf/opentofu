// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/opentofu/opentofu/internal/build/catalog"
	buildhcl "github.com/opentofu/opentofu/internal/build/hcl"
	"github.com/opentofu/opentofu/internal/build/dice"
	"github.com/opentofu/opentofu/internal/build/solve"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
	"github.com/opentofu/opentofu/internal/modsdir"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestParseSelectorsDefaultsToRecursiveAll(t *testing.T) {
	set, diags := ParseSelectors(nil)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if got, want := len(set), 1; got != want {
		t.Fatalf("wrong selector count: got %d want %d", got, want)
	}
	if got, want := set[0].Raw, "**"; got != want {
		t.Fatalf("wrong default selector: got %q want %q", got, want)
	}
}

func TestStreamCandidateMatches(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "child" {
  source = "./child"
}

output "root_value" {
  value = "ok"
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
output "child_value" {
  value = "ok"
}
`)
	writeModulesManifest(t, rootDir)

	loaded, diags := loadTestConfig(t, rootDir)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}

	patterns, diags := ParseSelectors([]string{"module.child.**"})
	if diags.HasErrors() {
		t.Fatalf("unexpected selector diagnostics: %s", diags.Err())
	}

	var got []catalog.Addr
	StreamCandidateMatches(loaded.Catalog, patterns, func(addr catalog.Addr) bool {
		got = append(got, addr)
		return true
	})

	want := []catalog.Addr{
		catalog.ModuleAddr(catalog.RootModule().Child("child", catalog.NoKey())),
		catalog.OutputAddr(catalog.RootModule().Child("child", catalog.NoKey()), "child_value"),
	}
	if gotLen, wantLen := len(got), len(want); gotLen != wantLen {
		t.Fatalf("wrong match count: got %d want %d (%#v)", gotLen, wantLen, got)
	}
	for i := range want {
		if got[i].Identity() != want[i].Identity() {
			t.Fatalf("wrong match at %d: got %s want %s", i, got[i], want[i])
		}
	}
}

func TestStreamSelectionsResolvesConcreteInstances(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "child" {
  source   = "./child"
  for_each = {
    amd64 = "linux/amd64"
    arm64 = "linux/arm64"
  }
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
resource "test_instance" "build" {
  for_each = {
    release = true
    debug   = false
  }
}

output "digest" {
  value = "ok"
}
`)
	writeModulesManifest(t, rootDir)

	loaded, solver := loadTestSolver(t, rootDir)

	patterns, diags := ParseSelectors([]string{
		`module.child["arm64"].test_instance.build["release"]`,
		`module.child["amd64"].output.digest`,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected selector diagnostics: %s", diags.Err())
	}

	var got []string
	resolveDiags := StreamSelections(t.Context(), loaded.Catalog, solver, patterns, func(addr catalog.Addr) bool {
		got = append(got, addr.String())
		return true
	})
	if resolveDiags.HasErrors() {
		t.Fatalf("unexpected resolution diagnostics: %s", resolveDiags.Err())
	}

	want := []string{
		`module.child["amd64"].output.digest`,
		`module.child["arm64"].test_instance.build["release"]`,
	}
	slices.Sort(got)
	slices.Sort(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("wrong resolved selections (-want +got):\n%s", diff)
	}
}

func TestStreamSelectionsRecursiveAllResolvesConcreteTree(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "child" {
  source   = "./child"
  for_each = {
    amd64 = "linux/amd64"
    arm64 = "linux/arm64"
  }
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
resource "test_instance" "build" {
  for_each = {
    release = true
    debug   = false
  }
}

output "digest" {
  value = "ok"
}
`)
	writeModulesManifest(t, rootDir)

	loaded, solver := loadTestSolver(t, rootDir)

	patterns, diags := ParseSelectors([]string{"**"})
	if diags.HasErrors() {
		t.Fatalf("unexpected selector diagnostics: %s", diags.Err())
	}

	var got []string
	resolveDiags := StreamSelections(t.Context(), loaded.Catalog, solver, patterns, func(addr catalog.Addr) bool {
		got = append(got, addr.String())
		return true
	})
	if resolveDiags.HasErrors() {
		t.Fatalf("unexpected resolution diagnostics: %s", resolveDiags.Err())
	}

	want := []string{
		`module`,
		`module.child["amd64"]`,
		`module.child["amd64"].output.digest`,
		`module.child["amd64"].test_instance.build["debug"]`,
		`module.child["amd64"].test_instance.build["release"]`,
		`module.child["arm64"]`,
		`module.child["arm64"].output.digest`,
		`module.child["arm64"].test_instance.build["debug"]`,
		`module.child["arm64"].test_instance.build["release"]`,
	}
	slices.Sort(got)
	slices.Sort(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("wrong recursive-all selections (-want +got):\n%s", diff)
	}
}

func TestStreamSelectionsRecursiveAllPreservesDeferredSubtreeDeclarations(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "child" {
  source = "./child"
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
resource "test_instance" "base" {}

module "maybe" {
  source = "../grandchild"
  count  = test_instance.base.id != "" ? 1 : 0
}
`)
	writeTestFile(t, filepath.Join(rootDir, "grandchild", "main.tf"), `
resource "test_instance" "build" {}
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
		"child": modsdir.Record{
			Key:        "child",
			SourceAddr: "./child",
			Dir:        filepath.Join(rootDir, "child"),
		},
		"child.maybe": modsdir.Record{
			Key:        "child.maybe",
			SourceAddr: "../grandchild",
			Dir:        filepath.Join(rootDir, "grandchild"),
		},
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		t.Fatalf("failed to write modules manifest: %s", err)
	}

	loaded, solver := loadTestSolver(t, rootDir)

	patterns, diags := ParseSelectors([]string{"**"})
	if diags.HasErrors() {
		t.Fatalf("unexpected selector diagnostics: %s", diags.Err())
	}

	var got []string
	resolveDiags := StreamSelections(t.Context(), loaded.Catalog, solver, patterns, func(addr catalog.Addr) bool {
		got = append(got, addr.String())
		return true
	})
	_ = resolveDiags

	want := []string{
		`module`,
		`module.child`,
		`module.child.test_instance.base`,
	}
	slices.Sort(got)
	slices.Sort(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("wrong recursive-all selections with deferred subtree (-want +got):\n%s", diff)
	}
}

func TestStreamSelectionsPreservesConcreteRepeatedParentForDeferredSubtree(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "child" {
  source   = "./child"
  for_each = {
    amd64 = "linux/amd64"
  }
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
resource "test_instance" "base" {}

module "maybe" {
  source = "../grandchild"
  count  = test_instance.base.id != "" ? 1 : 0
}
`)
	writeTestFile(t, filepath.Join(rootDir, "grandchild", "main.tf"), `
resource "test_instance" "build" {}
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
		"child": modsdir.Record{
			Key:        "child",
			SourceAddr: "./child",
			Dir:        filepath.Join(rootDir, "child"),
		},
		"child.maybe": modsdir.Record{
			Key:        "child.maybe",
			SourceAddr: "../grandchild",
			Dir:        filepath.Join(rootDir, "grandchild"),
		},
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		t.Fatalf("failed to write modules manifest: %s", err)
	}

	loaded, solver := loadTestSolver(t, rootDir)

	patterns, diags := ParseSelectors([]string{"module.child.**"})
	if diags.HasErrors() {
		t.Fatalf("unexpected selector diagnostics: %s", diags.Err())
	}

	var got []string
	resolveDiags := StreamSelections(t.Context(), loaded.Catalog, solver, patterns, func(addr catalog.Addr) bool {
		got = append(got, addr.String())
		return true
	})
	// In inspection mode (no engine), deferred child modules produce errors
	// and their subtrees are skipped. The parent module and its direct
	// targets are still yielded.
	_ = resolveDiags

	want := []string{
		`module.child["amd64"]`,
		`module.child["amd64"].test_instance.base`,
	}
	slices.Sort(got)
	slices.Sort(want)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("wrong deferred subtree selections under repeated parent (-want +got):\n%s", diff)
	}
}

func loadTestConfig(t *testing.T, rootDir string) (*Loaded, tfdiags.Diagnostics) {
	t.Helper()
	loader, err := configload.NewLoader(&configload.Config{
		ModulesDir: filepath.Join(rootDir, ".terraform", "modules"),
	})
	if err != nil {
		t.Fatalf("failed to create loader: %s", err)
	}
	return Load(t.Context(), loader, buildhcl.LoadRequest{RootDir: rootDir}, configs.RootModuleCallForTesting())
}

func loadTestSolver(t *testing.T, rootDir string) (*Loaded, *solve.Solver) {
	t.Helper()
	loaded, diags := loadTestConfig(t, rootDir)
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if loaded == nil {
		t.Fatal("expected loaded configuration")
	}
	solver := solve.New(func() *dice.Session { s, _ := dice.NewSession(t.Context()); return s }(), loaded.Catalog, loaded, solve.Config{})
	return loaded, solver
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("failed to create directory: %s", err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)), 0o644); err != nil {
		t.Fatalf("failed to write file: %s", err)
	}
}

func writeModulesManifest(t *testing.T, rootDir string) {
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
