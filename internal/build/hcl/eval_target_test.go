// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/modsdir"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestLoadedEvalTargetCollectsRefsAndProviderFunctionCalls(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "base" {}

resource "test_instance" "build" {
  parent = test_instance.base.id
  image  = provider::oci::get("cgr.dev/example/image")

  precondition {
    condition     = provider::time::rfc3339_parse("2023-07-25T23:43:16Z").year > 0
    error_message = "timestamp must parse"
  }
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "build", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}

	wantRefs := []catalog.Addr{
		catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "base", catalog.NoKey()),
	}
	if diff := cmp.Diff(wantRefs, result.Refs, cmp.Comparer(func(a, b catalog.Addr) bool {
		return a.Identity() == b.Identity()
	})); diff != "" {
		t.Fatalf("wrong target refs (-want +got):\n%s", diff)
	}

	if got, want := len(result.ProviderFunctionCalls), 2; got != want {
		t.Fatalf("wrong provider function call count: got %d want %d", got, want)
	}
	var sawOCIGetRequest bool
	for _, call := range result.ProviderFunctionCalls {
		if call.Key == (digest.Digest{}) {
			t.Fatalf("provider function %s has zero call key", call.Ref.Function)
		}
		switch call.Ref.Function {
		case (addrs.ProviderFunction{ProviderName: "oci", Function: "get"}):
			if call.Request == nil {
				t.Fatalf("provider function %s is missing a lowered request", call.Ref.Function)
			}
			sawOCIGetRequest = true
		case (addrs.ProviderFunction{ProviderName: "time", Function: "rfc3339_parse"}):
			if call.Request != nil {
				t.Fatalf("query-safe provider function %s should not have a lowered action request", call.Ref.Function)
			}
		}
	}
	if !sawOCIGetRequest {
		t.Fatal("expected lowered request for provider::oci::get")
	}
}

func TestLoadedEvalTargetLowersExternalCompatibilityTarget(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
terraform {
  required_providers {
    external = {
      source = "hashicorp/external"
    }
  }
}

data "external" "lookup" {
  program = ["sh", "-c", "cat >/dev/null; echo '{\"image_ref\":\"ok\"}'"]
  query = {
    cache_key = "abc"
  }
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindData, "external", "lookup", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	payload := result.Payload
	if payload == nil {
		t.Fatalf("expected payload in result")
	}
	if got, want := payload.ValueAdapter, buildrun.ValueAdapterExternalV1; got != want {
		t.Fatalf("wrong value adapter: got %q want %q", got, want)
	}
	req, decodeDiags := buildrun.DecodeRequest(payload.Request, tfdiags.SourceRange{})
	if decodeDiags.HasErrors() {
		t.Fatalf("unexpected request decode diagnostics: %s", decodeDiags.Err())
	}
	wantExe, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("failed to resolve sh: %s", err)
	}
	if !filepath.IsAbs(wantExe) {
		wantExe, err = filepath.Abs(wantExe)
		if err != nil {
			t.Fatalf("failed to absolutize sh path: %s", err)
		}
	}
	if diff := cmp.Diff([]string{wantExe, "-c", "cat >/dev/null; echo '{\"image_ref\":\"ok\"}'"}, req.Argv); diff != "" {
		t.Fatalf("wrong argv (-want +got):\n%s", diff)
	}
	if !filepath.IsAbs(req.Argv[0]) {
		t.Fatalf("wrong executable path: got %q want absolute path", req.Argv[0])
	}
	if got, want := req.Decode, buildrun.DecodeModeJSON; got != want {
		t.Fatalf("wrong decode mode: got %q want %q", got, want)
	}
	if got, want := string(req.Stdin), `{"cache_key":"abc"}`; got != want {
		t.Fatalf("wrong stdin: got %q want %q", got, want)
	}
}

func TestLoadedEvalTargetLowersNullResourceCompatibilityTarget(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
terraform {
  required_providers {
    null = {
      source = "hashicorp/null"
    }
  }
}

resource "null_resource" "cache_record" {
  triggers = {
    image = "cgr.dev/example:latest"
  }

  provisioner "local-exec" {
    command = "echo ok"
  }
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "null_resource", "cache_record", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	payload := result.Payload
	if payload == nil {
		t.Fatalf("expected payload in result")
	}
	if got, want := payload.ValueAdapter, buildrun.ValueAdapterNullV1; got != want {
		t.Fatalf("wrong value adapter: got %q want %q", got, want)
	}
	req, decodeDiags := buildrun.DecodeRequest(payload.Request, tfdiags.SourceRange{})
	if decodeDiags.HasErrors() {
		t.Fatalf("unexpected request decode diagnostics: %s", decodeDiags.Err())
	}
	if diff := cmp.Diff([]string{"/bin/sh", "-c", "echo ok"}, req.Argv); diff != "" {
		t.Fatalf("wrong argv (-want +got):\n%s", diff)
	}

	var valueData struct {
		Triggers map[string]string `json:"triggers"`
	}
	if err := json.Unmarshal(payload.ValueData, &valueData); err != nil {
		t.Fatalf("failed to decode value data: %s", err)
	}
	if diff := cmp.Diff(map[string]string{"image": "cgr.dev/example:latest"}, valueData.Triggers); diff != "" {
		t.Fatalf("wrong value data (-want +got):\n%s", diff)
	}
}

func TestLoadedEvalTargetLowersNativeRunTarget(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), fmt.Sprintf(`
run "lookup" {
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  stdin_json = {
    cache_key = "abc"
  }
  decode = "json"
}
`, os.Args[0]))

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	payload := result.Payload
	if payload == nil {
		t.Fatalf("expected payload in result")
	}
	if got, want := payload.ValueAdapter, buildrun.ValueAdapterRawV1; got != want {
		t.Fatalf("wrong value adapter: got %q want %q", got, want)
	}
	req, decodeDiags := buildrun.DecodeRequest(payload.Request, tfdiags.SourceRange{})
	if decodeDiags.HasErrors() {
		t.Fatalf("unexpected request decode diagnostics: %s", decodeDiags.Err())
	}
	if diff := cmp.Diff([]string{os.Args[0], "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"}, req.Argv); diff != "" {
		t.Fatalf("wrong argv (-want +got):\n%s", diff)
	}
	if got, want := req.Decode, buildrun.DecodeModeJSON; got != want {
		t.Fatalf("wrong decode mode: got %q want %q", got, want)
	}
	if got, want := string(req.Stdin), `{"cache_key":"abc"}`; got != want {
		t.Fatalf("wrong stdin: got %q want %q", got, want)
	}
}

func TestLoadedEvalTargetLowersNativeRunCommandTarget(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
run "lookup" {
  command = "printf native-command"
  decode  = "text"
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	payload := result.Payload
	if payload == nil {
		t.Fatalf("expected payload in result")
	}
	req, decodeDiags := buildrun.DecodeRequest(payload.Request, tfdiags.SourceRange{})
	if decodeDiags.HasErrors() {
		t.Fatalf("unexpected request decode diagnostics: %s", decodeDiags.Err())
	}
	interpreter := defaultLocalExecInterpreter()
	resolved, resolveDiags := resolveExecutable(interpreter[0], catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()), "Invalid run block")
	if resolveDiags.HasErrors() {
		t.Fatalf("unexpected interpreter resolution diagnostics: %s", resolveDiags.Err())
	}
	wantArgv := append([]string{resolved}, interpreter[1:]...)
	wantArgv = append(wantArgv, "printf native-command")
	if diff := cmp.Diff(wantArgv, req.Argv); diff != "" {
		t.Fatalf("wrong command argv (-want +got):\n%s", diff)
	}
	if got, want := req.Decode, buildrun.DecodeModeText; got != want {
		t.Fatalf("wrong decode mode: got %q want %q", got, want)
	}
}

func TestLoadedEvalTargetLowersNestedTargetThroughKeyedParentModuleInputs(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "images" {
  source = "./images"
}
`)
	writeTestFile(t, filepath.Join(rootDir, "images", "main.tf"), `
module "build" {
  source   = "../publisher"
  for_each = { crane = { value = "hello" } }

  value = each.value.value
}
`)
	writeTestFile(t, filepath.Join(rootDir, "publisher", "main.tf"), `
variable "value" {
  type = string
}

module "child" {
  source = "../child"
  count  = var.value != "" ? 1 : 0
  value  = var.value
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
variable "value" {
  type = string
}

resource "test_instance" "build" {
  name = var.value
}
`)

	modulesDir := filepath.Join(rootDir, ".terraform", "modules")
	if err := os.MkdirAll(modulesDir, 0o755); err != nil {
		t.Fatalf("create modules dir: %s", err)
	}
	manifest := modsdir.Manifest{
		"": {
			Key: "",
			Dir: rootDir,
		},
		"images": {
			Key:        "images",
			SourceAddr: "./images",
			Dir:        filepath.Join(rootDir, "images"),
		},
		"images.build": {
			Key:        "images.build",
			SourceAddr: "../publisher",
			Dir:        filepath.Join(rootDir, "publisher"),
		},
		"images.build.child": {
			Key:        "images.build.child",
			SourceAddr: "../child",
			Dir:        filepath.Join(rootDir, "child"),
		},
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		t.Fatalf("write modules manifest: %s", err)
	}

	loaded := loadOutputFixture(t, rootDir)
	addr := catalog.ResourceAddr(
		catalog.RootModule().
			Child("images", catalog.NoKey()).
			Child("build", catalog.StringKey("crane")).
			Child("child", catalog.IntKey(0)),
		catalog.TargetKindResource,
		"test_instance",
		"build",
		catalog.NoKey(),
	)
	schema := &providers.Schema{
		Block: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"name": {Type: cty.String, Required: true},
			},
		},
	}

	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr:   addr,
		Schema: schema,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Deferred {
		t.Fatal("did not expect deferred target config")
	}
	if result.Value == cty.NilVal {
		t.Fatal("expected cty.Value in result")
	}
	if !result.Known {
		t.Fatal("expected known result")
	}
}

func TestLoadedEvalTargetRejectsNativeRunProgramModeWithUnknownInterpreter(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), fmt.Sprintf(`
resource "test_resource" "base" {}

run "lookup" {
  program     = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  interpreter = [test_resource.base.id]
  decode      = "json"
}
`, os.Args[0]))

	loaded := loadOutputFixture(t, rootDir)
	_, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()),
	})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, `Run blocks may set "interpreter" only when using "command".`) {
		t.Fatalf("missing interpreter-mode diagnostic in:\n%s", got)
	}
}

func TestLoadedEvalTargetDefersProviderTargetRequestWithUnknownConfigValues(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "base" {}

resource "test_instance" "build" {
  name = test_instance.base.id
}
`)

	loaded := loadOutputFixture(t, rootDir)
	schema := &providers.Schema{
		Block: &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"name": {Type: cty.String, Required: true},
			},
		},
	}

	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr:   catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "build", catalog.NoKey()),
		Schema: schema,
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Deferred {
		t.Fatal("expected deferred target config")
	}
	if result.Value != cty.NilVal || result.Payload != nil {
		t.Fatalf("expected no lowered request for deferred config")
	}
}

func TestLoadedEvalTargetRejectsNativeRunConflictingModesBeforeDeferral(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), fmt.Sprintf(`
resource "test_resource" "base" {}

run "lookup" {
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  command = test_resource.base.id
  decode  = "json"
}
`, os.Args[0]))

	loaded := loadOutputFixture(t, rootDir)
	_, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()),
	})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, `Run blocks may set only one of "program" or "command".`) {
		t.Fatalf("missing conflicting-mode diagnostic in:\n%s", got)
	}
}

func TestLoadedEvalTargetLowersChildModuleNativeRunTargetWithModuleInputs(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "child" {
  source = "./child"
  image  = "cgr.dev/example:child"
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), fmt.Sprintf(`
variable "image" {
  type = string
}

run "lookup" {
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  stdin_json = {
    cache_key = var.image
  }
  decode = "json"
}
`, os.Args[0]))
	writeInstalledModulesManifest(t, rootDir)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.RunAddr(catalog.RootModule().Child("child", catalog.NoKey()), "lookup", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	payload := result.Payload
	if payload == nil {
		t.Fatalf("expected payload in result")
	}
	req, decodeDiags := buildrun.DecodeRequest(payload.Request, tfdiags.SourceRange{})
	if decodeDiags.HasErrors() {
		t.Fatalf("unexpected request decode diagnostics: %s", decodeDiags.Err())
	}
	if got, want := string(req.Stdin), `{"cache_key":"cgr.dev/example:child"}`; got != want {
		t.Fatalf("wrong child-module stdin: got %q want %q", got, want)
	}
}

func TestLoadedEvalTargetLowersNativeRunTargetWithQuerySafeProviderFunction(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), fmt.Sprintf(`
run "lookup" {
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  stdin_json = {
    year = provider::time::rfc3339_parse("2023-07-25T23:43:16Z").year
  }
  decode = "json"
}
`, os.Args[0]))

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	payload := result.Payload
	if payload == nil {
		t.Fatalf("expected payload in result")
	}
	req, decodeDiags := buildrun.DecodeRequest(payload.Request, tfdiags.SourceRange{})
	if decodeDiags.HasErrors() {
		t.Fatalf("unexpected request decode diagnostics: %s", decodeDiags.Err())
	}
	if got, want := string(req.Stdin), `{"year":2023}`; got != want {
		t.Fatalf("wrong query-safe stdin: got %q want %q", got, want)
	}
	if got, want := len(result.ProviderFunctionCalls), 1; got != want {
		t.Fatalf("wrong provider function call count: got %d want %d", got, want)
	}
	call := result.ProviderFunctionCalls[0]
	if call.Ref.Function != (addrs.ProviderFunction{ProviderName: "time", Function: "rfc3339_parse"}) {
		t.Fatalf("wrong provider function: got %s", call.Ref.Function)
	}
	if call.Request != nil {
		t.Fatalf("query-safe provider function %s should not have a lowered action request", call.Ref.Function)
	}
}

func TestLoadedEvalTargetCollectsLoweredProviderFunctionCallsForNativeRun(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), fmt.Sprintf(`
run "lookup" {
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  stdin_json = {
    image_ref = provider::oci::get("cgr.dev/example/image")
  }
  decode = "json"
}
`, os.Args[0]))

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.RunAddr(catalog.RootModule(), "lookup", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Deferred {
		t.Fatal("expected native run with non-query-safe provider function to defer")
	}
	if got, want := len(result.ProviderFunctionCalls), 1; got != want {
		t.Fatalf("wrong provider function call count: got %d want %d", got, want)
	}
	call := result.ProviderFunctionCalls[0]
	if call.Ref.Function != (addrs.ProviderFunction{ProviderName: "oci", Function: "get"}) {
		t.Fatalf("wrong provider function: got %s", call.Ref.Function)
	}
	if call.Request == nil {
		t.Fatalf("provider function %s is missing a lowered request", call.Ref.Function)
	}
	if call.Key == (digest.Digest{}) {
		t.Fatalf("provider function %s has zero call key", call.Ref.Function)
	}
}

func TestLoadedEvalTargetCollectsRefsForNullResourceCompatibilityProvisioner(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_resource" "base" {
  value = "hello"
}

resource "null_resource" "cache_record" {
  triggers = {
    image = test_resource.base.id
  }

  provisioner "local-exec" {
    command = test_resource.base.id
  }
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalTarget(t.Context(), EvalTargetRequest{
		Addr: catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "null_resource", "cache_record", catalog.NoKey()),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}

	wantRefs := []catalog.Addr{
		catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_resource", "base", catalog.NoKey()),
	}
	if diff := cmp.Diff(wantRefs, result.Refs, cmp.Comparer(func(a, b catalog.Addr) bool {
		return a.Identity() == b.Identity()
	})); diff != "" {
		t.Fatalf("wrong target refs (-want +got):\n%s", diff)
	}
}
