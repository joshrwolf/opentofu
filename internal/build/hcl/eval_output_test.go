// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestLoadedEvalOutputStaticValue(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
locals {
  tag = "busybox"
}

output "tag" {
  value = local.tag
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "tag"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Known {
		t.Fatal("expected known output value")
	}
	if result.Deferred {
		t.Fatal("did not expect deferred output value")
	}

	got := result.Value
	if !got.RawEquals(cty.StringVal("busybox")) {
		t.Fatalf("wrong output value: got %s", got.GoString())
	}
}

func TestLoadedEvalOutputDefersOnDynamicReference(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "base" {}

output "image" {
  value = test_instance.base.id
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "image"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Known {
		t.Fatal("did not expect known output value")
	}
	if !result.Deferred {
		t.Fatal("expected deferred output value")
	}

	wantRefs := []catalog.Addr{
		catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "base", catalog.NoKey()),
	}
	if diff := cmp.Diff(wantRefs, result.Refs, cmp.Comparer(func(a, b catalog.Addr) bool {
		return a.Identity() == b.Identity()
	})); diff != "" {
		t.Fatalf("wrong output refs (-want +got):\n%s", diff)
	}
}

func TestLoadedEvalOutputIncludesExplicitDependsOn(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "base" {}

output "tag" {
  value      = "busybox"
  depends_on = [test_instance.base]
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "tag"),
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
		t.Fatalf("wrong output refs (-want +got):\n%s", diff)
	}
}

func TestLoadedEvalOutputOverrideSkipsOriginalExpressionAnalysis(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "base" {}

output "image" {
  value      = provider::oci::get("foo")
  depends_on = [test_instance.base]
}
`)

	loaded := loadOutputFixture(t, rootDir)
	outputDecl, ok := loaded.Catalog.Output(catalog.OutputAddr(catalog.RootModule(), "image"))
	if !ok {
		t.Fatal("expected output declaration")
	}
	outputCfg, ok := outputDecl.Payload.(*configs.Output)
	if !ok || outputCfg == nil {
		t.Fatalf("wrong output payload type: got %T", outputDecl.Payload)
	}
	override := cty.StringVal("override")
	outputCfg.IsOverridden = true
	outputCfg.OverrideValue = &override

	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "image"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Known {
		t.Fatal("expected known override output value")
	}
	if got := len(result.ProviderFunctionCalls); got != 0 {
		t.Fatalf("expected no provider function calls from overridden output, got %d", got)
	}

	wantRefs := []catalog.Addr{
		catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "base", catalog.NoKey()),
	}
	if diff := cmp.Diff(wantRefs, result.Refs, cmp.Comparer(func(a, b catalog.Addr) bool {
		return a.Identity() == b.Identity()
	})); diff != "" {
		t.Fatalf("wrong overridden output refs (-want +got):\n%s", diff)
	}
}

func TestLoadedEvalOutputDefersOnNonQuerySafeProviderFunction(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
output "image" {
  value = provider::oci::get("foo")
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "image"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Known {
		t.Fatal("did not expect known output value")
	}
	if !result.Deferred {
		t.Fatal("expected deferred output value")
	}
	if got, want := len(result.ProviderFunctionCalls), 1; got != want {
		t.Fatalf("wrong provider function call count: got %d want %d", got, want)
	}
	if result.ProviderFunctionCalls[0].Ref.Function != (addrs.ProviderFunction{ProviderName: "oci", Function: "get"}) {
		t.Fatalf("wrong provider function: got %s", result.ProviderFunctionCalls[0].Ref.Function)
	}
	if result.ProviderFunctionCalls[0].Request == nil {
		t.Fatal("expected lowered provider-function request")
	}
	gotRange := result.ProviderFunctionCalls[0].Ref.Range
	if gotRange.Start.Line != 2 {
		t.Fatalf("wrong provider function range: got start line %d want 2", gotRange.Start.Line)
	}
}

func TestLoadedEvalOutputEvaluatesQuerySafeProviderFunction(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
output "parsed" {
  value = provider::time::rfc3339_parse("2023-07-25T23:43:16Z")
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "parsed"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Known {
		t.Fatal("expected known output value")
	}
	if result.Deferred {
		t.Fatal("did not expect deferred output value")
	}

	got := result.Value
	parsed := time.Date(2023, time.July, 25, 23, 43, 16, 0, time.UTC)
	isoYear, isoWeek := parsed.ISOWeek()
	want := cty.ObjectVal(map[string]cty.Value{
		"year":         cty.NumberIntVal(int64(parsed.Year())),
		"year_day":     cty.NumberIntVal(int64(parsed.YearDay())),
		"day":          cty.NumberIntVal(int64(parsed.Day())),
		"month":        cty.NumberIntVal(int64(parsed.Month())),
		"month_name":   cty.StringVal(parsed.Month().String()),
		"weekday":      cty.NumberIntVal(int64(parsed.Weekday())),
		"weekday_name": cty.StringVal(parsed.Weekday().String()),
		"hour":         cty.NumberIntVal(int64(parsed.Hour())),
		"minute":       cty.NumberIntVal(int64(parsed.Minute())),
		"second":       cty.NumberIntVal(int64(parsed.Second())),
		"unix":         cty.NumberIntVal(parsed.Unix()),
		"iso_year":     cty.NumberIntVal(int64(isoYear)),
		"iso_week":     cty.NumberIntVal(int64(isoWeek)),
	})
	if !got.RawEquals(want) {
		t.Fatalf("wrong parsed output value: got %s want %s", got.GoString(), want.GoString())
	}
}

func TestLoadedEvalOutputEvaluatesAliasedQuerySafeProviderFunction(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
output "parsed" {
  value = provider::time::alias::rfc3339_parse("2023-07-25T23:43:16Z")
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "parsed"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Known {
		t.Fatal("expected known output value")
	}
}

func TestLoadedEvalOutputEvaluatesQuerySafeProviderFunctionInLocalForExpr(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
locals {
  parsed = {
    for k, v in {
      latest = "cgr.dev/example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    } : k => provider::oci::parse(v)
  }
}

output "digest" {
  value = local.parsed.latest.digest
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "digest"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if !result.Known {
		t.Fatal("expected known output value")
	}

	got := result.Value
	if !got.RawEquals(cty.StringVal("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")) {
		t.Fatalf("wrong digest output: got %s", got.GoString())
	}
}

func TestLoadedEvalOutputFailsFalsePrecondition(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
locals {
  enabled = false
}

output "tag" {
  value = "busybox"

  precondition {
    condition     = local.enabled
    error_message = "output disabled"
  }
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "tag"),
	})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "output disabled") {
		t.Fatalf("missing precondition diagnostic in:\n%s", got)
	}
	if result.Known {
		t.Fatal("did not expect known output value")
	}
}

func TestLoadedEvalOutputDefersOnDynamicPrecondition(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "base" {}

output "tag" {
  value = "busybox"

  precondition {
    condition     = test_instance.base.id != ""
    error_message = "instance id must be known"
  }
}
`)

	loaded := loadOutputFixture(t, rootDir)
	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "tag"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if result.Known {
		t.Fatal("did not expect known output value")
	}
	if !result.Deferred {
		t.Fatal("expected deferred output value")
	}
}

func TestLoadedEvalOutputOverrideStillEvaluatesPreconditions(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
locals {
  enabled = false
}

output "image" {
  value = "original"

  precondition {
    condition     = local.enabled
    error_message = "override should still respect preconditions"
  }
}
`)

	loaded := loadOutputFixture(t, rootDir)
	outputDecl, ok := loaded.Catalog.Output(catalog.OutputAddr(catalog.RootModule(), "image"))
	if !ok {
		t.Fatal("expected output declaration")
	}
	outputCfg, ok := outputDecl.Payload.(*configs.Output)
	if !ok || outputCfg == nil {
		t.Fatalf("wrong output payload type: got %T", outputDecl.Payload)
	}
	override := cty.StringVal("override")
	outputCfg.IsOverridden = true
	outputCfg.OverrideValue = &override

	result, diags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: catalog.OutputAddr(catalog.RootModule(), "image"),
	})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "override should still respect preconditions") {
		t.Fatalf("missing override precondition diagnostic in:\n%s", got)
	}
	if result.Known {
		t.Fatal("did not expect known output value")
	}
}

func TestLoadedEvalOutputUsesRuntimeForChildModuleInputsAndQuerySafeLocalProviderFunction(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_instance" "gate" {}

module "child" {
  source    = "./child"
  timestamp = test_instance.gate.id
}

`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
variable "timestamp" {
  type = string
}

locals {
  parsed = provider::time::rfc3339_parse(var.timestamp)
}

output "year" {
  value = local.parsed.year
}
`)
	writeInstalledModulesManifest(t, rootDir)

	loaded := loadOutputFixture(t, rootDir)
	addr := catalog.OutputAddr(catalog.RootModule().Child("child", catalog.NoKey()), "year")

	withoutRuntime, withoutRuntimeDiags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: addr,
	})
	if withoutRuntimeDiags.HasErrors() {
		t.Fatalf("unexpected diagnostics without runtime: %s", withoutRuntimeDiags.Err())
	}
	if !withoutRuntime.Deferred {
		t.Fatal("expected deferred output without runtime")
	}

	withRuntime, withRuntimeDiags := loaded.EvalOutput(t.Context(), EvalOutputRequest{
		Addr: addr,
		Runtime: runtimeResolverFixture{
			targets: map[catalog.AddrKey]cty.Value{
				catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_instance", "gate", catalog.NoKey()).Identity(): cty.ObjectVal(map[string]cty.Value{
					"id": cty.StringVal("2023-07-25T23:43:16Z"),
				}),
			},
		},
	})
	if withRuntimeDiags.HasErrors() {
		t.Fatalf("unexpected diagnostics with runtime: %s", withRuntimeDiags.Err())
	}
	if !withRuntime.Known {
		t.Fatal("expected known output with runtime")
	}
	if withRuntime.Deferred {
		t.Fatal("did not expect deferred output with runtime")
	}

	got := withRuntime.Value
	if !got.RawEquals(cty.NumberIntVal(2023)) {
		t.Fatalf("wrong parsed year: got %s", got.GoString())
	}
}

func loadOutputFixture(t *testing.T, rootDir string) *Loaded {
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
	if loaded == nil {
		t.Fatal("expected loaded config")
	}
	return loaded
}

type runtimeResolverFixture struct {
	targets map[catalog.AddrKey]cty.Value
	outputs map[catalog.AddrKey]cty.Value
	modules map[catalog.ModulePathKey]cty.Value
}

func (r runtimeResolverFixture) TargetValue(_ context.Context, addr catalog.Addr) (cty.Value, bool, tfdiags.Diagnostics) {
	if value, ok := r.targets[addr.Identity()]; ok {
		return value, true, nil
	}
	return cty.NilVal, false, nil
}

func (r runtimeResolverFixture) OutputValue(_ context.Context, addr catalog.Addr) (cty.Value, bool, tfdiags.Diagnostics) {
	if value, ok := r.outputs[addr.Identity()]; ok {
		return value, true, nil
	}
	return cty.NilVal, false, nil
}

func (r runtimeResolverFixture) ModuleValue(_ context.Context, module catalog.ModulePath) (cty.Value, bool, tfdiags.Diagnostics) {
	if value, ok := r.modules[module.Identity()]; ok {
		return value, true, nil
	}
	return cty.NilVal, false, nil
}
