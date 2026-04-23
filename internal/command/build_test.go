// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	commandtesting "github.com/opentofu/opentofu/internal/command/testing"
	"github.com/opentofu/opentofu/internal/command/workdir"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/zclconf/go-cty/cty"
)

func TestBuildCommandPrintsSelectedModuleOutput(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
module "child" {
  source   = "./child"
  for_each = {
    amd64 = "linux/amd64"
  }
}
`)
	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "child", "main.tf"), `
output "digest" {
  value = "ok"
}
`)

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{`module.child["amd64"].output.digest`})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"ok"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandBuildsSelectedOutputDeps(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
resource "test_instance" "build" {}

output "id" {
  value = test_instance.build.id
}
`)

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{"output.id"})
	output := done(t)
	if code == 0 {
		t.Fatal("expected build to fail")
	}
	if got := output.Stderr(); !strings.Contains(got, `Resource type "test_instance"`) {
		t.Fatalf("missing unsupported target diagnostic in:\n%s", got)
	}
}

func TestBuildCommandPrintsSelectedResourceOutputAfterBuild(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
resource "test_resource" "build" {
  value = "hello"
}

output "value" {
  value = test_resource.build.value
}
`)

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"output.value"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"hello"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandPrintsSelectedDataOutputAfterBuild(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
data "test_data_source" "build" {
  id = "foo"
}

output "value" {
  value = data.test_data_source.build.value
}
`)

	provider := commandtesting.NewProvider(nil)
	provider.Store.Put("foo", cty.ObjectVal(map[string]cty.Value{
		"id":              cty.StringVal("foo"),
		"value":           cty.StringVal("from-data"),
		"interrupt_count": cty.NullVal(cty.Number),
	}))

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"output.value"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"from-data"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandUsesProviderDataConfig(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
provider "test" {
  data_prefix = "images"
}

data "test_data_source" "build" {
  id = "foo"
}

output "value" {
  value = data.test_data_source.build.value
}
`)

	provider := commandtesting.NewProvider(nil)
	provider.Store.Put("images/foo", cty.ObjectVal(map[string]cty.Value{
		"id":              cty.StringVal("foo"),
		"value":           cty.StringVal("from-configured-data"),
		"interrupt_count": cty.NullVal(cty.Number),
	}))

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"output.value"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"from-configured-data"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandPrintsSelectedRepeatedResourceOutputAfterBuild(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
resource "test_resource" "build" {
  for_each = {
    amd64 = "hello"
  }

  value = each.value
}

output "value" {
  value = test_resource.build["amd64"].value
}
`)

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"output.value"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"hello"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandUsesProviderResourceConfig(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
provider "test" {
  resource_prefix = "images"
}

resource "test_resource" "build" {
  value = "hello"
}
`)

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"test_resource.build"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got := provider.ResourceString(); !strings.Contains(got, "images/") {
		t.Fatalf("expected configured resource prefix in stored keys, got %q", got)
	}
}

func TestBuildCommandPrintsSelectedRepeatedModuleOutputAfterBuild(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
module "child" {
  source = "./child"
  for_each = {
    amd64 = "hello"
  }

  value = each.value
}

output "value" {
  value = module.child["amd64"].value
}
`)
	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "child", "main.tf"), `
variable "value" {
  type = string
}

output "value" {
  value = var.value
}
`)

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{"output.value"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"hello"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandRunsApkoBuildThroughGenericProviderPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
terraform {
  required_providers {
    apko = {
      source = "chainguard-dev/apko"
    }
  }
}

resource "apko_build" "image" {
  repo   = "cgr.dev/example/image"
  config = "contents:\n  packages:\n    - wolfi-base"
}

output "image_ref" {
  value = apko_build.image.image_ref
}
`)

	providerAddr := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko")
	apkoProvider := commandtesting.NewApkoProvider()
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
			testingOverrides: &testingOverrides{
				Providers: map[addrs.Provider]providers.Factory{
					providerAddr: providers.FactoryFixed(apkoProvider.Provider),
				},
			},
		},
	}

	code := c.Run([]string{"output.image_ref"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	got := strings.TrimSpace(output.Stdout())
	if !strings.Contains(got, `"cgr.dev/example/image@sha256:`) {
		t.Fatalf("wrong image_ref output: got %q", got)
	}
}

func TestBuildCommandRunsApkoBuildRawThroughGenericProviderPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
terraform {
  required_providers {
    apko = {
      source = "chainguard-dev/apko"
    }
  }
}

resource "apko_build_raw" "image" {
  repo = "cgr.dev/example/raw"
  configs_raw = {
    amd64 = jsonencode({ archs = ["amd64"] })
  }
}

output "image_ref" {
  value = apko_build_raw.image.image_ref
}
`)

	providerAddr := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko")
	apkoProvider := commandtesting.NewApkoProvider()
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
			testingOverrides: &testingOverrides{
				Providers: map[addrs.Provider]providers.Factory{
					providerAddr: providers.FactoryFixed(apkoProvider.Provider),
				},
			},
		},
	}

	code := c.Run([]string{"output.image_ref"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	got := strings.TrimSpace(output.Stdout())
	if !strings.Contains(got, `"cgr.dev/example/raw@sha256:`) {
		t.Fatalf("wrong image_ref output: got %q", got)
	}
}

func TestBuildCommandRunsApkoConfigThroughProviderDefaultPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
terraform {
  required_providers {
    apko = {
      source = "chainguard-dev/apko"
    }
  }
}

data "apko_config" "image" {
  config_contents = "contents:\n  packages:\n    - wolfi-base"
}

output "config" {
  value = data.apko_config.image.config
}
`)

	providerAddr := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko")
	apkoProvider := commandtesting.NewApkoProvider()
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
			testingOverrides: &testingOverrides{
				Providers: map[addrs.Provider]providers.Factory{
					providerAddr: providers.FactoryFixed(apkoProvider.Provider),
				},
			},
		},
	}

	code := c.Run([]string{"output.config"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	got := strings.TrimSpace(output.Stdout())
	if !strings.Contains(got, "contents:") || !strings.Contains(got, "packages:") || !strings.Contains(got, "wolfi-base") {
		t.Fatalf("wrong output: got %q", got)
	}
}

func TestBuildCommandRunsOciExecTestThroughProviderDefaultPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
terraform {
  required_providers {
    oci = {
      source = "chainguard-dev/oci"
    }
  }
}

data "oci_exec_test" "check" {
  script = "echo ok"
}

output "result" {
  value = data.oci_exec_test.check.result
}
`)

	providerAddr := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "oci")
	ociProvider := commandtesting.NewOciProvider()
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
			testingOverrides: &testingOverrides{
				Providers: map[addrs.Provider]providers.Factory{
					providerAddr: providers.FactoryFixed(ociProvider.Provider),
				},
			},
		},
	}

	code := c.Run([]string{"output.result"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"echo ok"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandRunsCosignCopyThroughProviderDefaultPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
terraform {
  required_providers {
    cosign = {
      source = "chainguard-dev/cosign"
    }
  }
}

resource "cosign_copy" "copy" {
  source      = "cgr.dev/example/src@sha256:deadbeef"
  destination = "cgr.dev/example/dst"
}

output "id" {
  value = cosign_copy.copy.id
}
`)

	providerAddr := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "cosign")
	cosignProvider := commandtesting.NewCosignProvider()
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
			testingOverrides: &testingOverrides{
				Providers: map[addrs.Provider]providers.Factory{
					providerAddr: providers.FactoryFixed(cosignProvider.Provider),
				},
			},
		},
	}

	code := c.Run([]string{"output.id"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	got := strings.TrimSpace(output.Stdout())
	if !strings.HasPrefix(got, `"`) || len(got) < 10 {
		t.Fatalf("wrong output: got %q", got)
	}
}

func TestBuildCommandRunsImagetestHarnessThroughProviderDefaultPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
terraform {
  required_providers {
    imagetest = {
      source = "chainguard-dev/imagetest"
    }
  }
}

resource "imagetest_harness_docker" "this" {
  name = "smoke"
}

output "id" {
  value = imagetest_harness_docker.this.id
}
`)

	providerAddr := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "imagetest")
	imagetestProvider := commandtesting.NewImagetestProvider()
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
			testingOverrides: &testingOverrides{
				Providers: map[addrs.Provider]providers.Factory{
					providerAddr: providers.FactoryFixed(imagetestProvider.Provider),
				},
			},
		},
	}

	code := c.Run([]string{"output.id"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	got := strings.TrimSpace(output.Stdout())
	if !strings.HasPrefix(got, `"`) || len(got) < 10 {
		t.Fatalf("wrong output: got %q", got)
	}
}

func TestBuildCommandPrintsRuntimeFedTargetOutputAfterBuild(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
resource "test_resource" "base" {
  value = "hello"
}

resource "test_resource" "consumer" {
  value = test_resource.base.value
}

output "value" {
  value = test_resource.consumer.value
}
`)

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"output.value"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"hello"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandBuildsSelectedRuntimeFedTarget(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
resource "test_resource" "base" {
  value = "hello"
}

resource "test_resource" "consumer" {
  value = test_resource.base.value
}
`)

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"test_resource.consumer"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got := provider.ResourceCount(); got != 2 {
		t.Fatalf("wrong built resource count: got %d want %d", got, 2)
	}
}

func TestBuildCommandBuildsSelectedModuleTargets(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
module "child" {
  source = "./child"
}
`)
	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "child", "main.tf"), `
resource "test_resource" "build" {
  value = "hello"
}
`)

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"module.child"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got := provider.ResourceCount(); got != 1 {
		t.Fatalf("wrong built resource count: got %d want %d", got, 1)
	}
}

func TestBuildCommandRunsExternalThroughBuiltinRunPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), fmt.Sprintf(`
terraform {
  required_providers {
    external = {
      source = "hashicorp/external"
    }
  }
}

data "external" "lookup" {
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  query = {
    cache_key = "cgr.dev/example:latest"
  }
}

output "image_ref" {
  value = data.external.lookup.result.image_ref
}
`, os.Args[0]))

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{"output.image_ref"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"cgr.dev/example:latest"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandRunsNullResourceLocalExecThroughBuiltinRunPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	markerPath := filepath.Join(wd.RootModuleDir(), "marker.txt")
	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), fmt.Sprintf(`
terraform {
  required_providers {
    null = {
      source = "hashicorp/null"
    }
  }
}

resource "null_resource" "cache_record" {
  triggers = {
    greeting = "hello"
  }

  provisioner "local-exec" {
    command = %q
  }
}

output "greeting" {
  value = null_resource.cache_record.triggers.greeting
}
`, fmt.Sprintf("%q -test.run=TestBuildCommandExternalHelperProcess -- write %q hello", os.Args[0], markerPath)))

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{"output.greeting"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"hello"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
	content, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("failed to read marker file: %s", err)
	}
	if got, want := strings.TrimSpace(string(content)), "hello"; got != want {
		t.Fatalf("wrong marker content: got %q want %q", got, want)
	}
}

func TestBuildCommandRunsNativeRunThroughBuiltinRunPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), fmt.Sprintf(`
run "lookup" {
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  stdin_json = {
    cache_key = "cgr.dev/example:native"
  }
  decode = "json"
}

output "image_ref" {
  value = run.lookup.json.image_ref
}
`, os.Args[0]))

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{"output.image_ref"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"cgr.dev/example:native"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandRunsNativeRunCommandThroughBuiltinRunPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), fmt.Sprintf(`
run "lookup" {
  command = %q
  stdin_json = {
    cache_key = "cgr.dev/example:command"
  }
  decode = "json"
}

output "image_ref" {
  value = run.lookup.json.image_ref
}
`, fmt.Sprintf("%q -test.run=TestBuildCommandExternalHelperProcess -- emit", os.Args[0])))

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{"output.image_ref"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"cgr.dev/example:command"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandRunsRepeatedNativeRunThroughBuiltinRunPath(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), fmt.Sprintf(`
run "lookup" {
  for_each = {
    amd64 = "cgr.dev/example:amd64"
    arm64 = "cgr.dev/example:arm64"
  }
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "emit"]
  stdin_json = {
    cache_key = each.value
  }
  decode = "json"
}

output "image_ref" {
  value = run.lookup["arm64"].json.image_ref
}
`, os.Args[0]))

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{"output.image_ref"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	if got, want := strings.TrimSpace(output.Stdout()), `"cgr.dev/example:arm64"`; got != want {
		t.Fatalf("wrong output: got %q want %q", got, want)
	}
}

func TestBuildCommandBuildsSelectedNativeRunTarget(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	markerPath := filepath.Join(wd.RootModuleDir(), "run-marker.txt")
	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), fmt.Sprintf(`
run "write" {
  program = [%q, "-test.run=TestBuildCommandExternalHelperProcess", "--", "write", %q, "hello"]
}
`, os.Args[0], markerPath))

	view, done := testView(t)
	c := &BuildCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{"run.write"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	content, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("failed to read marker file: %s", err)
	}
	if got, want := strings.TrimSpace(string(content)), "hello"; got != want {
		t.Fatalf("wrong marker content: got %q want %q", got, want)
	}
}

func TestBuildCommandExternalHelperProcess(t *testing.T) {
	mode := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	if mode == "" {
		return
	}

	switch mode {
	case "emit":
		stdin, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatalf("failed to read stdin: %s", err)
		}
		var query map[string]string
		if err := json.Unmarshal(stdin, &query); err != nil {
			t.Fatalf("failed to decode stdin: %s", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
			"image_ref": query["cache_key"],
		}); err != nil {
			t.Fatalf("failed to encode stdout: %s", err)
		}
		os.Exit(0)
	case "write":
		if len(os.Args) < 3 {
			t.Fatal("missing write path or content")
		}
		path := os.Args[len(os.Args)-2]
		content := os.Args[len(os.Args)-1]
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("failed to write file: %s", err)
		}
		os.Exit(0)
	default:
		t.Fatalf("unsupported helper mode %q", mode)
	}
}

func writeBuildTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("failed to create directory: %s", err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)), 0o644); err != nil {
		t.Fatalf("failed to write file: %s", err)
	}
}
