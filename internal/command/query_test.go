// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	commandtesting "github.com/opentofu/opentofu/internal/command/testing"
	"github.com/opentofu/opentofu/internal/command/workdir"
	"github.com/opentofu/opentofu/internal/providers"
)

func TestQueryCommandShowsKnownRepeatedModuleOutput(t *testing.T) {
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
	c := &QueryCommand{
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
	stdout := output.Stdout()
	if !strings.Contains(stdout, "output.value") {
		t.Fatalf("missing output address in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "status: known") {
		t.Fatalf("missing known status in:\n%s", stdout)
	}
	if !strings.Contains(stdout, `value: "hello"`) {
		t.Fatalf("missing known value in:\n%s", stdout)
	}
}

func TestQueryCommandShowsDeferredRuntimeFedOutput(t *testing.T) {
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
	c := &QueryCommand{
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
	stdout := output.Stdout()
	if !strings.Contains(stdout, "status: deferred") {
		t.Fatalf("missing deferred status in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "action_deps: 1") {
		t.Fatalf("missing output action deps in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "depends on deferred build facts") {
		t.Fatalf("missing deferred output reason in:\n%s", stdout)
	}
}

func TestQueryCommandShowsDeferredSelectionForRepeatedModuleOutput(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
resource "test_resource" "base" {
  value = "hello"
}

module "child" {
  source = "./child"
  for_each = {
    amd64 = test_resource.base.value
  }

  value = each.value
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

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &QueryCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{`module.child["amd64"].output.value`})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	stdout := output.Stdout()
	if !strings.Contains(stdout, `module.child["amd64"].output.value`) {
		t.Fatalf("missing requested output address in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "status: deferred") {
		t.Fatalf("missing deferred status in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Module instance expansion is deferred") {
		t.Fatalf("missing deferred selection reason in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "resolved: module.child.output.value") {
		t.Fatalf("missing resolved declaration address in:\n%s", stdout)
	}
}

func TestQueryCommandShowsLowerableTargetAction(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
resource "test_resource" "build" {
  value = "hello"
}
`)

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &QueryCommand{
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
	stdout := output.Stdout()
	if !strings.Contains(stdout, "status: lowerable") {
		t.Fatalf("missing lowerable status in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "runner: provider.resource") {
		t.Fatalf("missing runner in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "provider: registry.opentofu.org/hashicorp/test") {
		t.Fatalf("missing provider binding in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "action_key: ") {
		t.Fatalf("missing action key in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_match: exact") {
		t.Fatalf("missing capability match in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_revision: v1") {
		t.Fatalf("missing capability revision in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_supported: true") {
		t.Fatalf("missing capability supported flag in:\n%s", stdout)
	}
}

func TestQueryCommandShowsApkoTargetCapabilityMetadata(t *testing.T) {
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
`)

	providerAddr := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko")
	apkoProvider := commandtesting.NewApkoProvider()
	view, done := testView(t)
	c := &QueryCommand{
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

	code := c.Run([]string{"apko_build.image"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	stdout := output.Stdout()
	if !strings.Contains(stdout, "capability_match: exact") {
		t.Fatalf("missing capability match in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_revision: v2") {
		t.Fatalf("missing capability revision in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_supported: true") {
		t.Fatalf("missing capability supported flag in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_note: Serialized by target repository") {
		t.Fatalf("missing capability note in:\n%s", stdout)
	}
}

func TestQueryCommandShowsProviderDefaultApkoCapabilityMetadata(t *testing.T) {
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
`)

	providerAddr := addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko")
	apkoProvider := commandtesting.NewApkoProvider()
	view, done := testView(t)
	c := &QueryCommand{
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

	code := c.Run([]string{"data.apko_config.image"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	stdout := output.Stdout()
	if !strings.Contains(stdout, "capability_match: provider") {
		t.Fatalf("missing provider capability match in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_revision: apko-provider-v1") {
		t.Fatalf("missing provider capability revision in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_supported: true") {
		t.Fatalf("missing capability supported flag in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_note: Build-capable by reviewed provider family default.") {
		t.Fatalf("missing provider capability note in:\n%s", stdout)
	}
}

func TestQueryCommandShowsReverseDepsForTarget(t *testing.T) {
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
  value = test_resource.base.value
}
`)

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &QueryCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"-rdeps", "test_resource.base"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	stdout := output.Stdout()
	if !strings.Contains(stdout, "reverse_deps:") {
		t.Fatalf("missing reverse deps block in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "test_resource.consumer") {
		t.Fatalf("missing reverse target dependency in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "output.value") {
		t.Fatalf("missing reverse output dependency in:\n%s", stdout)
	}
}

func TestQueryCommandShowsPartialReverseDepsWhenDeferredSubtreesAreOmitted(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
resource "test_resource" "base" {
  value = "hello"
}

resource "test_resource" "consumer" {
  value = test_resource.base.value
}

module "child" {
  source = "./child"
  for_each = {
    amd64 = test_resource.base.value
  }

  value = each.value
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

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &QueryCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"-rdeps", "test_resource.base"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	stdout := output.Stdout()
	if !strings.Contains(stdout, "reverse_deps_status: partial") {
		t.Fatalf("missing partial reverse deps status in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Deferred module instances were omitted") {
		t.Fatalf("missing partial reverse deps reason in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "test_resource.consumer") {
		t.Fatalf("missing known reverse dependency in:\n%s", stdout)
	}
}

func TestQueryCommandShowsReverseDepsForOutput(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
module "child" {
  source = "./child"
}

output "consumer" {
  value = module.child.base
}
`)
	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "child", "main.tf"), `
output "base" {
  value = "hello"
}
`)

	view, done := testView(t)
	c := &QueryCommand{
		Meta: Meta{
			WorkingDir: workdir.NewDir(wd.RootModuleDir()),
			View:       view,
		},
	}

	code := c.Run([]string{"-rdeps", "module.child.output.base"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	stdout := output.Stdout()
	if !strings.Contains(stdout, "reverse_deps:") {
		t.Fatalf("missing reverse deps block in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "output.consumer") {
		t.Fatalf("missing reverse output dependency in:\n%s", stdout)
	}
}

func TestQueryCommandIncludesDiagnosticDetailForBlockedTarget(t *testing.T) {
	wd := tempWorkingDir(t)
	t.Chdir(wd.RootModuleDir())

	writeBuildTestFile(t, filepath.Join(wd.RootModuleDir(), "main.tf"), `
resource "test_instance" "build" {}
`)

	provider := commandtesting.NewProvider(nil)
	view, done := testView(t)
	c := &QueryCommand{
		Meta: Meta{
			WorkingDir:       workdir.NewDir(wd.RootModuleDir()),
			View:             view,
			testingOverrides: metaOverridesForProvider(provider.Provider),
		},
	}

	code := c.Run([]string{"test_instance.build"})
	output := done(t)
	if code != 0 {
		t.Fatalf("unexpected error:\n%s", output.Stderr())
	}
	stdout := output.Stdout()
	if !strings.Contains(stdout, "status: blocked") {
		t.Fatalf("missing blocked status in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Unsupported provider target in build mode") {
		t.Fatalf("missing diagnostic summary in:\n%s", stdout)
	}
	if !strings.Contains(stdout, `Resource type "test_instance" from provider registry.opentofu.org/hashicorp/test is not supported in build mode.`) {
		t.Fatalf("missing diagnostic detail in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_provider: registry.opentofu.org/hashicorp/test") {
		t.Fatalf("missing capability provider in:\n%s", stdout)
	}
	if !strings.Contains(stdout, "capability_match: none") {
		t.Fatalf("missing capability match fallback in:\n%s", stdout)
	}
}
