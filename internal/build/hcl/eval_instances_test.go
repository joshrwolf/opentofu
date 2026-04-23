// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/modsdir"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestLoadedEvalTargetInstancesCount(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "build" {
  count = 2
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTargetInstances(t.Context(), EvalTargetInstancesRequest{
		DeclAddr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "build", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Deferred {
		t.Fatal("did not expect deferred instances")
	}

	want := []catalog.Key{catalog.IntKey(0), catalog.IntKey(1)}
	if diff := cmp.Diff(want, result.Keys); diff != "" {
		t.Fatalf("wrong count instance keys (-want +got):\n%s", diff)
	}
}

func TestLoadedEvalTargetInstancesForEach(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "build" {
  for_each = {
    beta  = "b"
    alpha = "a"
  }
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTargetInstances(t.Context(), EvalTargetInstancesRequest{
		DeclAddr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "build", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Deferred {
		t.Fatal("did not expect deferred instances")
	}

	want := []catalog.Key{catalog.StringKey("alpha"), catalog.StringKey("beta")}
	if diff := cmp.Diff(want, result.Keys); diff != "" {
		t.Fatalf("wrong for_each instance keys (-want +got):\n%s", diff)
	}
}

func TestLoadedEvalTargetInstancesDefersOnDynamicCount(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "base" {}

resource "test_instance" "build" {
  count = test_instance.base.id != "" ? 1 : 0
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTargetInstances(t.Context(), EvalTargetInstancesRequest{
		DeclAddr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "build", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Deferred {
		t.Fatal("expected deferred instances")
	}
	if got := len(result.Keys); got != 0 {
		t.Fatalf("expected no concrete keys for deferred instances, got %d", got)
	}
	wantRefs := []catalog.Addr{
		catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "base", catalog.NoKey()),
	}
	if diff := cmp.Diff(wantRefs, result.Refs, cmp.Comparer(func(a, b catalog.Addr) bool {
		return a.Identity() == b.Identity()
	})); diff != "" {
		t.Fatalf("wrong deferred instance refs (-want +got):\n%s", diff)
	}
	if got := len(loaded.targetRepetitionCache); got != 0 {
		t.Fatalf("expected deferred target repetition to avoid cache entry, got %d", got)
	}
}

func TestLoadedEvalTargetInstancesDefersOnNonQuerySafeProviderFunction(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "build" {
  count = provider::oci::get("cgr.dev/example/image") != "" ? 1 : 0
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTargetInstances(t.Context(), EvalTargetInstancesRequest{
		DeclAddr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "build", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Deferred {
		t.Fatal("expected deferred instances")
	}
	if got, want := len(result.ProviderFunctionCalls), 1; got != want {
		t.Fatalf("wrong provider function call count: got %d want %d", got, want)
	}
	if got, want := result.ProviderFunctionCalls[0].Ref.Function, (addrs.ProviderFunction{ProviderName: "oci", Function: "get"}); got != want {
		t.Fatalf("wrong provider function ref: got %#v want %#v", got, want)
	}
	if result.ProviderFunctionCalls[0].Request == nil {
		t.Fatal("expected lowered provider function request for deferred instance fact")
	}
}

func TestLoadedEvalTargetInstancesRejectsNullEnabled(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "build" {
  lifecycle {
    enabled = null
  }
}
`)

	loaded := loadOutputFixture(t, rootDir)
	_, diags := loaded.EvalTargetInstances(t.Context(), EvalTargetInstancesRequest{
		DeclAddr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "build", catalog.NoKey()),
	})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); got == "" {
		t.Fatal("expected diagnostic error text")
	}
}

func TestLoadedEvalTargetInstancesRunCount(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), fmt.Sprintf(`
run "lookup" {
  count   = 2
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  stdin_json = {
    cache_key = tostring(count.index)
  }
  decode = "json"
}
`, os.Args[0]))

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTargetInstances(t.Context(), EvalTargetInstancesRequest{
		DeclAddr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Deferred {
		t.Fatal("did not expect deferred run instances")
	}

	want := []catalog.Key{catalog.IntKey(0), catalog.IntKey(1)}
	if diff := cmp.Diff(want, result.Keys); diff != "" {
		t.Fatalf("wrong run count instance keys (-want +got):\n%s", diff)
	}

	concrete, concreteDiags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.IntKey(1)),
	})
	if concreteDiags.HasErrors() {
		t.Fatalf("unexpected concrete diagnostics: %s", concreteDiags.Err())
	}
	payload := concrete.Payload
	if payload == nil {
		t.Fatalf("expected payload in result")
	}
	req, decodeDiags := buildrun.DecodeRequest(payload.Request, tfdiags.SourceRange{})
	if decodeDiags.HasErrors() {
		t.Fatalf("unexpected request decode diagnostics: %s", decodeDiags.Err())
	}
	if got, want := string(req.Stdin), `{"cache_key":"1"}`; got != want {
		t.Fatalf("wrong count-based stdin: got %q want %q", got, want)
	}
}

func TestLoadedEvalTargetInstancesRunForEach(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), fmt.Sprintf(`
run "lookup" {
  for_each = {
    beta  = "b"
    alpha = "a"
  }
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  stdin_json = {
    cache_key = each.value
  }
  decode = "json"
}
`, os.Args[0]))

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTargetInstances(t.Context(), EvalTargetInstancesRequest{
		DeclAddr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Deferred {
		t.Fatal("did not expect deferred run instances")
	}

	want := []catalog.Key{catalog.StringKey("alpha"), catalog.StringKey("beta")}
	if diff := cmp.Diff(want, result.Keys); diff != "" {
		t.Fatalf("wrong run for_each instance keys (-want +got):\n%s", diff)
	}

	concrete, concreteDiags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.StringKey("beta")),
	})
	if concreteDiags.HasErrors() {
		t.Fatalf("unexpected concrete diagnostics: %s", concreteDiags.Err())
	}
	payload := concrete.Payload
	if payload == nil {
		t.Fatalf("expected payload in result")
	}
	req, decodeDiags := buildrun.DecodeRequest(payload.Request, tfdiags.SourceRange{})
	if decodeDiags.HasErrors() {
		t.Fatalf("unexpected request decode diagnostics: %s", decodeDiags.Err())
	}
	if got, want := string(req.Stdin), `{"cache_key":"b"}`; got != want {
		t.Fatalf("wrong for_each-based stdin: got %q want %q", got, want)
	}
}

func TestLoadedEvalTargetInstancesRunEnabled(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), fmt.Sprintf(`
run "lookup" {
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  decode  = "json"

  lifecycle {
    enabled = false
  }
}
`, os.Args[0]))

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTargetInstances(t.Context(), EvalTargetInstancesRequest{
		DeclAddr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Deferred {
		t.Fatal("did not expect deferred run instances")
	}
	if got, want := result.Shape, InstanceShapeOptional; got != want {
		t.Fatalf("wrong enabled run shape: got %q want %q", got, want)
	}
	if got := len(result.Keys); got != 0 {
		t.Fatalf("expected no concrete keys for disabled run, got %d", got)
	}
}

func TestLoadedEvalImportInstancesCount(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "child" {
  source = "./child"
  count  = 2
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
output "image" {
  value = "busybox"
}
`)
	writeInstalledModulesManifest(t, rootDir)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalImportInstances(t.Context(), EvalImportInstancesRequest{
		DeclAddr: catalog.ModuleAddr(catalog.RootModule().Child("child", catalog.NoKey())),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Deferred {
		t.Fatal("did not expect deferred module instances")
	}

	want := []catalog.Key{catalog.IntKey(0), catalog.IntKey(1)}
	if diff := cmp.Diff(want, result.Keys); diff != "" {
		t.Fatalf("wrong module instance keys (-want +got):\n%s", diff)
	}
}

func TestLoadedEvalImportInstancesDoesNotCacheDeferredResults(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "base" {}

module "child" {
  source = "./child"
  for_each = {
    amd64 = test_instance.base.id
  }
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
output "value" {
  value = "hello"
}
`)
	writeInstalledModulesManifest(t, rootDir)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalImportInstances(t.Context(), EvalImportInstancesRequest{
		DeclAddr: catalog.ModuleAddr(catalog.RootModule().Child("child", catalog.NoKey())),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Deferred {
		t.Fatal("expected deferred module instances")
	}
	if got := len(loaded.importRepetitionCache); got != 0 {
		t.Fatalf("expected deferred import repetition to avoid cache entry, got %d", got)
	}
}

func TestLoadedEvalImportInstancesUsesRuntimeResolver(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "gate" {}

module "child" {
  source = "./child"
  count  = test_instance.gate.id == "ready" ? 1 : 0
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
output "value" {
  value = "hello"
}
`)
	writeInstalledModulesManifest(t, rootDir)

	loaded := loadOutputFixture(t, rootDir)
	declAddr := catalog.ModuleAddr(catalog.RootModule().Child("child", catalog.NoKey()))

	withoutRuntime, withoutRuntimeDiags := loaded.EvalImportInstances(t.Context(), EvalImportInstancesRequest{
		DeclAddr: declAddr,
	})
	if withoutRuntimeDiags.HasErrors() {
		t.Fatalf("unexpected diagnostics without runtime: %s", withoutRuntimeDiags.Err())
	}
	if !withoutRuntime.Deferred {
		t.Fatal("expected deferred module instances without runtime")
	}

	withRuntime, withRuntimeDiags := loaded.EvalImportInstances(t.Context(), EvalImportInstancesRequest{
		DeclAddr: declAddr,
		Runtime: runtimeResolverFixture{
			targets: map[catalog.AddrKey]cty.Value{
				catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "gate", catalog.NoKey()).Identity(): cty.ObjectVal(map[string]cty.Value{
					"id": cty.StringVal("ready"),
				}),
			},
		},
	})
	if withRuntimeDiags.HasErrors() {
		t.Fatalf("unexpected diagnostics with runtime: %s", withRuntimeDiags.Err())
	}
	if withRuntime.Deferred {
		t.Fatal("did not expect deferred module instances with runtime")
	}

	want := []catalog.Key{catalog.IntKey(0)}
	if diff := cmp.Diff(want, withRuntime.Keys); diff != "" {
		t.Fatalf("wrong runtime module instance keys (-want +got):\n%s", diff)
	}
}

func TestLoadedEvalImportInstancesUsesConcreteRepeatedParentModuleInputs(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
locals {
  builds = {
    crane = {
      repo = "crane"
    }
  }
}

module "parent" {
  source   = "./parent"
  for_each = local.builds
  repo     = each.value.repo
}
`)
	writeTestFile(t, filepath.Join(rootDir, "parent", "main.tf"), `
variable "repo" {
  type = string
}

module "child" {
  source = "../child"
  count  = var.repo == "crane" ? 1 : 0
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
output "value" {
  value = "hello"
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

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalImportInstances(t.Context(), EvalImportInstancesRequest{
		Module: catalog.RootModule().Child("parent", catalog.StringKey("crane")).Child("child", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Deferred {
		t.Fatal("did not expect deferred nested module instances")
	}

	want := []catalog.Key{catalog.IntKey(0)}
	if diff := cmp.Diff(want, result.Keys); diff != "" {
		t.Fatalf("wrong nested module instance keys (-want +got):\n%s", diff)
	}
}

func TestLoadedEvalTargetReusesCachedForEachValues(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "build" {
  for_each = jsondecode(file("${path.module}/values.json"))
  name     = each.value
}
`)
	writeTestFile(t, filepath.Join(rootDir, "values.json"), `{"alpha":"a","beta":"b"}`)

	loaded := loadOutputFixture(t, rootDir)
	_, diags := loaded.EvalTargetInstances(t.Context(), EvalTargetInstancesRequest{
		DeclAddr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "build", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected repetition diagnostics: %s", diags.Err())
	}
	if err := os.Remove(filepath.Join(rootDir, "values.json")); err != nil {
		t.Fatalf("remove values file: %s", err)
	}

	schema := &providers.Schema{
		Block: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"name": {Type: cty.String, Optional: true},
			},
		},
	}
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr:   catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "build", catalog.StringKey("alpha")),
		Schema: schema,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected target diagnostics after deleting values file: %s", diags.Err())
	}
	if result.Deferred {
		t.Fatal("did not expect deferred target request")
	}
	if result.Value == cty.NilVal && result.Payload == nil {
		t.Fatal("expected lowered target request")
	}
}

func TestLoadedEvalOutputReusesCachedModuleForEachValues(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
locals {
  values = jsondecode(file("${path.module}/values.json"))
}

module "child" {
  source   = "./child"
  for_each = local.values
  value    = each.value
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
variable "value" {
  type = string
}

output "value" {
  value = var.value
}
`)
	writeTestFile(t, filepath.Join(rootDir, "values.json"), `{"alpha":"a","beta":"b"}`)
	writeInstalledModulesManifest(t, rootDir)

	loaded := loadOutputFixture(t, rootDir)
	_, diags := loaded.EvalImportInstances(t.Context(), EvalImportInstancesRequest{
		DeclAddr: catalog.ModuleAddr(catalog.RootModule().Child("child", catalog.NoKey())),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected import diagnostics: %s", diags.Err())
	}
	if err := os.Remove(filepath.Join(rootDir, "values.json")); err != nil {
		t.Fatalf("remove values file: %s", err)
	}

	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule().Child("child", catalog.StringKey("alpha")), "value"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected output diagnostics after deleting values file: %s", diags.Err())
	}
	if !result.Known {
		t.Fatal("expected known output value")
	}
	if !result.Value.RawEquals(cty.StringVal("a")) {
		t.Fatalf("wrong output value: got %#v", result.Value)
	}
}
