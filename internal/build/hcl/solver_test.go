// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/dice"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	"github.com/opentofu/opentofu/internal/build/solve"

	commandtesting "github.com/opentofu/opentofu/internal/command/testing"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/modsdir"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestActionSpecUsesTargetCapabilityPolicyAndRevision(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_resource" "build" {
  value = "hello"
}
`)

	loaded := loadTestConfig(t, rootDir)
	provider := commandtesting.NewProvider(nil)
	makeSolver := func(revision string) *solve.Solver {
		reg := buildprovider.NewTargetCapabilityRegistry()
		reg.Register(buildprovider.TargetCapability{
			Provider:  addrs.NewDefaultProvider("test"),
			Kind:      catalog.RunnerKindProviderResource,
			TypeName:  "test_resource",
			Supported: true,
			Revision:  revision,
			PolicyFunc: func(buildprovider.TargetRequest, buildprovider.Binding) (buildprovider.TargetCapabilityPolicy, tfdiags.Diagnostics) {
				return buildprovider.TargetCapabilityPolicy{
					Cacheable: true,
					LockKeys:  []string{"registry/test"},
				}, nil
			},
		})

		solver := solve.New(func() *dice.Session { s, _ := dice.NewSession(t.Context()); return s }(), loaded.Catalog, loaded, solve.Config{
			Providers: buildprovider.NewSession(buildprovider.SessionConfig{
				Factories: map[addrs.Provider]providers.Factory{
					addrs.NewDefaultProvider("test"): providers.FactoryFixed(provider.Provider),
				},
				TargetCapabilities: reg,
			}),
		})
		return solver
	}

	addr := catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_resource", "build", catalog.NoKey())
	v1Spec, v1Diags := makeSolver("policy-v1").ActionSpec(t.Context(), addr)
	if v1Diags.HasErrors() {
		t.Fatalf("unexpected v1 action diagnostics: %s", v1Diags.Err())
	}
	if !v1Spec.Cacheable {
		t.Fatal("expected cacheable action spec from capability policy")
	}
	if diff := cmp.Diff([]engine.LockKey{"provider/hashicorp/test/registry/test"}, v1Spec.Locks); diff != "" {
		t.Fatalf("wrong lock keys (-want +got):\n%s", diff)
	}

	v2Spec, v2Diags := makeSolver("policy-v2").ActionSpec(t.Context(), addr)
	if v2Diags.HasErrors() {
		t.Fatalf("unexpected v2 action diagnostics: %s", v2Diags.Err())
	}
	if got, want := v1Spec.Key == v2Spec.Key, false; got != want {
		t.Fatal("expected capability revision to influence provider target action identity")
	}
}

func TestActionSpecUsesDefaultApkoBuildCapabilityPolicy(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
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

	loaded := loadTestConfig(t, rootDir)
	apkoProvider := commandtesting.NewApkoProvider()
	solver := solve.New(func() *dice.Session { s, _ := dice.NewSession(t.Context()); return s }(), loaded.Catalog, loaded, solve.Config{
		Providers: buildprovider.NewSession(buildprovider.SessionConfig{
			Factories: map[addrs.Provider]providers.Factory{
				addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko"): providers.FactoryFixed(apkoProvider.Provider),
			},
		}),
	})

	addr := catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "apko_build", "image", catalog.NoKey())
	spec, diags := solver.ActionSpec(t.Context(), addr)
	if diags.HasErrors() {
		t.Fatalf("unexpected action diagnostics: %s", diags.Err())
	}
	if spec.Cacheable {
		t.Fatal("did not expect apko_build actions to be cacheable by default")
	}
	if diff := cmp.Diff([]engine.LockKey{"provider/chainguard-dev/apko/repo/cgr.dev/example/image"}, spec.Locks); diff != "" {
		t.Fatalf("wrong apko lock keys (-want +got):\n%s", diff)
	}
}

func TestActionSpecUsesDefaultApkoBuildRawCapabilityPolicy(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
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
`)

	loaded := loadTestConfig(t, rootDir)
	apkoProvider := commandtesting.NewApkoProvider()
	solver := solve.New(func() *dice.Session { s, _ := dice.NewSession(t.Context()); return s }(), loaded.Catalog, loaded, solve.Config{
		Providers: buildprovider.NewSession(buildprovider.SessionConfig{
			Factories: map[addrs.Provider]providers.Factory{
				addrs.NewProvider(addrs.DefaultProviderRegistryHost, "chainguard-dev", "apko"): providers.FactoryFixed(apkoProvider.Provider),
			},
		}),
	})

	addr := catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "apko_build_raw", "image", catalog.NoKey())
	spec, diags := solver.ActionSpec(t.Context(), addr)
	if diags.HasErrors() {
		t.Fatalf("unexpected action diagnostics: %s", diags.Err())
	}
	if spec.Cacheable {
		t.Fatal("did not expect apko_build_raw actions to be cacheable by default")
	}
	if diff := cmp.Diff([]engine.LockKey{"provider/chainguard-dev/apko/repo/cgr.dev/example/raw"}, spec.Locks); diff != "" {
		t.Fatalf("wrong apko raw lock keys (-want +got):\n%s", diff)
	}
}

func TestOutputValueUsesRuntimeResolverForRepeatedChildModuleWithoutEngineRecords(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
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
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
variable "value" {
  type = string
}

output "value" {
  value = var.value
}
`)
	writeInstalledModulesManifest(t, rootDir)

	loaded := loadTestConfig(t, rootDir)
	solver := newTestSolver(t, loaded)
	addr := catalog.OutputAddr(catalog.RootModule(), "value")

	withoutReader, withoutReaderDiags := solver.OutputValue(t.Context(), addr)
	if withoutReaderDiags.HasErrors() {
		t.Fatalf("unexpected diagnostics without runtime reader: %s", withoutReaderDiags.Err())
	}
	if !withoutReader.Known || withoutReader.Deferred {
		t.Fatalf("expected output to become known through solver-owned runtime resolution, got known=%t deferred=%t", withoutReader.Known, withoutReader.Deferred)
	}

	runtimeSolver := newTestSolverWithEngine(t, loaded, newNoopEngine(t))

	after, afterDiags := runtimeSolver.OutputValue(t.Context(), addr)
	if afterDiags.HasErrors() {
		t.Fatalf("unexpected runtime-aware diagnostics: %s", afterDiags.Err())
	}
	if !after.Known || after.Deferred {
		t.Fatalf("expected runtime-aware output to become known, got known=%t deferred=%t", after.Known, after.Deferred)
	}

	got := after.Value
	if !got.RawEquals(cty.StringVal("hello")) {
		t.Fatalf("wrong output value: got %s", got.GoString())
	}
}

func TestOutputValueWithRuntimeUsesCachedValueWhenConditionalSkipsAbsentCountedModuleInstance(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
terraform {
  required_providers {
    external = {
      source = "hashicorp/external"
    }
  }
}

locals {
  build_dev = true
}

data "external" "dev_cache_lookup" {
  count = local.build_dev ? 1 : 0

  program = ["/bin/sh", "-c", "cat >/dev/null; echo '{\"image_ref\":\"cached\"}'"]
  query = {
    cache_key = "abc"
  }
}

locals {
  cached_dev_ref  = local.build_dev ? try(data.external.dev_cache_lookup[0].result.image_ref, "") : ""
  needs_dev_build = local.build_dev && local.cached_dev_ref == ""
}

module "child" {
  source = "./child"
  count  = local.needs_dev_build ? 1 : 0
}

output "dev_ref" {
  value = local.build_dev ? (local.needs_dev_build ? module.child[0].image : local.cached_dev_ref) : null
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
output "image" {
  value = "hello"
}
`)
	writeInstalledModulesManifest(t, rootDir)

	loaded := loadTestConfig(t, rootDir)
	solver := newTestSolver(t, loaded)

	cacheAddr := catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindData, "external", "dev_cache_lookup", catalog.IntKey(0))
	cacheSpec, diags := solver.ActionSpec(t.Context(), cacheAddr)
	if diags.HasErrors() {
		t.Fatalf("unexpected cache lookup diagnostics: %s", diags.Err())
	}
	payload, ok := cacheSpec.Runner.Payload.(buildrun.Payload)
	if !ok {
		t.Fatalf("wrong cache lookup payload type: got %T", cacheSpec.Runner.Payload)
	}
	recordPayload, err := buildrun.EncodeResult(payload.Request, buildrun.Result{
		ExitCode: 0,
		Stdout:   []byte(`{"image_ref":"cached"}`),
		Decoded: &buildrun.DecodedValue{
			Mode: buildrun.DecodeModeJSON,
			JSON: []byte(`{"image_ref":"cached"}`),
		},
	})
	if err != nil {
		t.Fatalf("encode cache lookup result: %s", err)
	}
	runtimeSolver := newTestSolverWithEngine(t, loaded, newSingleRecordEngine(t, cacheSpec.Key, engine.Record{
			ActionKey: cacheSpec.Key,
			OutputKey: cacheSpec.Key,
			Payload:   recordPayload,
		},
	))

	result, resultDiags := runtimeSolver.OutputValue(t.Context(), catalog.OutputAddr(catalog.RootModule(), "dev_ref"))
	if resultDiags.HasErrors() {
		t.Fatalf("unexpected output diagnostics: %s", resultDiags.Err())
	}
	if !result.Known || result.Deferred {
		t.Fatalf("expected known cached output, got known=%t deferred=%t", result.Known, result.Deferred)
	}

	got := result.Value
	if !got.RawEquals(cty.StringVal("cached")) {
		t.Fatalf("wrong cached output value: got %s", got.GoString())
	}
}

func TestOutputValueWithRuntimeUsesCachedValueWhenConditionalSkipsAbsentCountedModuleInstanceInKeyedParentModule(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
module "build" {
  source   = "./build"
  for_each = { crane = { dev_configs = ["dev"] } }

  dev_configs = each.value.dev_configs
}
`)
	writeTestFile(t, filepath.Join(rootDir, "build", "main.tf"), `
terraform {
  required_providers {
    external = {
      source = "hashicorp/external"
    }
  }
}

variable "dev_configs" {
  type = list(string)
}

locals {
  build_dev = length(var.dev_configs) > 0
}

data "external" "dev_cache_lookup" {
  count = local.build_dev ? 1 : 0

  program = ["/bin/sh", "-c", "cat >/dev/null; echo '{\"image_ref\":\"cached\"}'"]
  query = {
    cache_key = "abc"
  }
}

locals {
  cached_dev_ref  = local.build_dev ? try(data.external.dev_cache_lookup[0].result.image_ref, "") : ""
  needs_dev_build = local.build_dev && local.cached_dev_ref == ""
}

module "child" {
  source = "../child"
  count  = local.needs_dev_build ? 1 : 0
}

output "dev_ref" {
  value = local.build_dev ? (local.needs_dev_build ? module.child[0].image : local.cached_dev_ref) : null
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
output "image" {
  value = "hello"
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
		"build": {
			Key:        "build",
			SourceAddr: "./build",
			Dir:        filepath.Join(rootDir, "build"),
		},
		"build.child": {
			Key:        "build.child",
			SourceAddr: "../child",
			Dir:        filepath.Join(rootDir, "child"),
		},
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		t.Fatalf("write modules manifest: %s", err)
	}

	loaded := loadTestConfig(t, rootDir)
	solver := newTestSolver(t, loaded)

	cacheAddr := catalog.ResourceAddr(catalog.RootModule().Child("build", catalog.StringKey("crane")), catalog.TargetKindData, "external", "dev_cache_lookup", catalog.IntKey(0))
	cacheSpec, diags := solver.ActionSpec(t.Context(), cacheAddr)
	if diags.HasErrors() {
		t.Fatalf("unexpected cache lookup diagnostics: %s", diags.Err())
	}
	payload, ok := cacheSpec.Runner.Payload.(buildrun.Payload)
	if !ok {
		t.Fatalf("wrong cache lookup payload type: got %T", cacheSpec.Runner.Payload)
	}
	recordPayload, err := buildrun.EncodeResult(payload.Request, buildrun.Result{
		ExitCode: 0,
		Stdout:   []byte(`{"image_ref":"cached"}`),
		Decoded: &buildrun.DecodedValue{
			Mode: buildrun.DecodeModeJSON,
			JSON: []byte(`{"image_ref":"cached"}`),
		},
	})
	if err != nil {
		t.Fatalf("encode cache lookup result: %s", err)
	}
	runtimeSolver := newTestSolverWithEngine(t, loaded, newSingleRecordEngine(t, cacheSpec.Key, engine.Record{
			ActionKey: cacheSpec.Key,
			OutputKey: cacheSpec.Key,
			Payload:   recordPayload,
		},
	))

	result, resultDiags := runtimeSolver.OutputValue(t.Context(), catalog.OutputAddr(catalog.RootModule().Child("build", catalog.StringKey("crane")), "dev_ref"))
	if resultDiags.HasErrors() {
		t.Fatalf("unexpected output diagnostics: %s", resultDiags.Err())
	}
	if !result.Known || result.Deferred {
		t.Fatalf("expected known cached output, got known=%t deferred=%t", result.Known, result.Deferred)
	}

	got := result.Value
	if !got.RawEquals(cty.StringVal("cached")) {
		t.Fatalf("wrong cached output value: got %s", got.GoString())
	}
}


func TestOutputValueDefersForRepeatedChildModuleOutputWithPartialModuleCollection(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

resource "test_resource" "base" {}

module "build" {
  source = "./child"
  for_each = {
    crane = "crane"
  }

  value = test_resource.base.id
}

output "image" {
  value = module.build["crane"].image
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
variable "value" {
  type = string
}

output "image" {
  value = var.value
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
		"build": {
			Key:        "build",
			SourceAddr: "./child",
			Dir:        filepath.Join(rootDir, "child"),
		},
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		t.Fatalf("write modules manifest: %s", err)
	}

	loaded := loadTestConfig(t, rootDir)
	_, diags := newTestSolver(t, loaded).OutputValue(t.Context(), catalog.OutputAddr(catalog.RootModule(), "image"))
	if !diags.HasErrors() {
		t.Fatal("expected error for output depending on runtime value without engine")
	}
}

func TestActionSpecModuleValueRefDependsOnlyOnModuleOutputs(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

module "child" {
  source = "./child"
}

resource "test_resource" "consumer" {
  name = jsonencode(module.child)
}
`)
	writeTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

resource "test_resource" "unused" {}

output "image" {
  value = "stable"
}
`)
	writeInstalledModulesManifest(t, rootDir)

	loaded := loadTestConfig(t, rootDir)
	spec, diags := newTestSolver(t, loaded).ActionSpec(t.Context(), catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_resource", "consumer", catalog.NoKey()))
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if got := len(spec.ExecDeps); got != 0 {
		t.Fatalf("expected no exec deps for bare module ref with known outputs, got %d", got)
	}
}

func TestActionSpecKeyChangesWithTargetConfig(t *testing.T) {
	rootDirA := t.TempDir()
	writeTestFile(t, filepath.Join(rootDirA, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

resource "test_resource" "build" {
  name = "one"
}
`)
	keyA := actionKeyForTestFixture(t, rootDirA)

	rootDirB := t.TempDir()
	writeTestFile(t, filepath.Join(rootDirB, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

resource "test_resource" "build" {
  name = "two"
}
`)
	keyB := actionKeyForTestFixture(t, rootDirB)

	if keyA == keyB {
		t.Fatalf("expected target config changes to perturb action identity, but both keys were %s", keyA)
	}
}

func TestActionSpecKeyChangesWithProviderConfig(t *testing.T) {
	rootDirA := t.TempDir()
	writeTestFile(t, filepath.Join(rootDirA, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

provider "test" {
  alias  = "src"
  region = "one"
}

resource "test_resource" "build" {
  provider = test.src
}
`)
	keyA := actionKeyForTestFixture(t, rootDirA)

	rootDirB := t.TempDir()
	writeTestFile(t, filepath.Join(rootDirB, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

provider "test" {
  alias  = "src"
  region = "two"
}

resource "test_resource" "build" {
  provider = test.src
}
`)
	keyB := actionKeyForTestFixture(t, rootDirB)

	if keyA == keyB {
		t.Fatalf("expected provider config changes to perturb action identity, but both keys were %s", keyA)
	}
}

func TestActionSpecKeyIgnoresCommentOnlyChurn(t *testing.T) {
	rootDirA := t.TempDir()
	writeTestFile(t, filepath.Join(rootDirA, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

provider "test" {
  alias = "src"
  region = "one"
}

resource "test_resource" "build" {
  provider = test.src
  name     = "stable"
}
`)
	keyA := actionKeyForTestFixture(t, rootDirA)

	rootDirB := t.TempDir()
	writeTestFile(t, filepath.Join(rootDirB, "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "foo/test"
    }
  }
}

provider "test" {
  alias = "src"
  # layout-only churn
  region = "one"
}

resource "test_resource" "build" {
  provider = test.src
  # layout-only churn
  name     = "stable"
}
`)
	keyB := actionKeyForTestFixture(t, rootDirB)

	if keyA != keyB {
		t.Fatalf("expected comment-only churn not to perturb action identity, got %s and %s", keyA, keyB)
	}
}

func loadTestConfig(t *testing.T, rootDir string) *Loaded {
	t.Helper()

	loader, err := configload.NewLoader(&configload.Config{
		ModulesDir: filepath.Join(rootDir, ".terraform", "modules"),
	})
	if err != nil {
		t.Fatalf("failed to create loader: %s", err)
	}

	loaded, diags := Load(t.Context(), loader, LoadRequest{RootDir: rootDir}, configs.RootModuleCallForTesting())
	if diags.HasErrors() {
		t.Fatalf("unexpected load diagnostics: %s", diags.Err())
	}
	if loaded == nil {
		t.Fatal("expected loaded config")
	}
	return loaded
}

func newTestSolver(t *testing.T, loaded *Loaded) *solve.Solver {
	t.Helper()
	return newTestSolverWithConfig(t, loaded, loaded, solve.Config{})
}

func newTestSolverWithEngine(t *testing.T, loaded *Loaded, eng *engine.Engine) *solve.Solver {
	t.Helper()
	return newTestSolverWithConfig(t, loaded, loaded, solve.Config{Engine: eng})
}

func newTestSolverWithConfig(t *testing.T, loaded *Loaded, eval solve.Evaluator, cfg solve.Config) *solve.Solver {
	t.Helper()

	provider := commandtesting.NewProvider(nil)
	schema := *provider.Provider.GetProviderSchemaResponse
	providerBlock := *schema.Provider.Block
	providerAttrs := make(map[string]*configschema.Attribute, len(providerBlock.Attributes)+1)
	maps.Copy(providerAttrs, providerBlock.Attributes)
	providerAttrs["region"] = &configschema.Attribute{Type: cty.String, Optional: true}
	providerBlock.Attributes = providerAttrs
	schema.Provider.Block = &providerBlock

	resourceTypes := make(map[string]providers.Schema, len(schema.ResourceTypes)+1)
	maps.Copy(resourceTypes, schema.ResourceTypes)
	resourceSchema := resourceTypes["test_resource"]
	resourceBlock := *resourceSchema.Block
	resourceAttrs := make(map[string]*configschema.Attribute, len(resourceBlock.Attributes)+3)
	maps.Copy(resourceAttrs, resourceBlock.Attributes)
	resourceAttrs["name"] = &configschema.Attribute{Type: cty.String, Optional: true}
	resourceAttrs["image"] = &configschema.Attribute{Type: cty.String, Optional: true}
	resourceAttrs["year"] = &configschema.Attribute{Type: cty.Number, Optional: true}
	resourceBlock.Attributes = resourceAttrs
	resourceSchema.Block = &resourceBlock
	resourceTypes["test_resource"] = resourceSchema
	schema.ResourceTypes = resourceTypes
	provider.Provider.GetProviderSchemaResponse = &schema

	if cfg.Providers == nil {
		cfg.Providers = buildprovider.NewSession(buildprovider.SessionConfig{
			Factories: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("test"):                                    providers.FactoryFixed(provider.Provider),
				addrs.NewProvider(addrs.DefaultProviderRegistryHost, "foo", "test"): providers.FactoryFixed(provider.Provider),
			},
		})
	}
	return solve.New(func() *dice.Session { s, _ := dice.NewSession(t.Context()); return s }(), loaded.Catalog, eval, cfg)
}

func actionKeyForTestFixture(t *testing.T, rootDir string) string {
	t.Helper()

	loaded := loadTestConfig(t, rootDir)
	solver := newTestSolver(t, loaded)
	spec, diags := solver.ActionSpec(t.Context(), catalog.ResourceAddr(catalog.RootModule(), catalog.TargetKindResource, "test_resource", "build", catalog.NoKey()))
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	return spec.Key.String()
}

func newNoopEngine(t *testing.T) *engine.Engine {
	t.Helper()
	return engine.New(engine.Config{
		Context: t.Context(),
		Runner: engine.RunnerFunc(func(_ context.Context, spec engine.Spec) (engine.RunResult, error) {
			return engine.RunResult{Payload: []byte(spec.Name)}, nil
		}),
	})
}

func newSingleRecordEngine(t *testing.T, key digest.Digest, record engine.Record) *engine.Engine {
	t.Helper()
	return engine.New(engine.Config{
		Context: t.Context(),
		Runner: engine.RunnerFunc(func(_ context.Context, spec engine.Spec) (engine.RunResult, error) {
			if spec.Key == key {
				return engine.RunResult{
					OutputKey: record.OutputKey,
					Payload:   record.Payload,
				}, nil
			}
			return engine.RunResult{Payload: []byte(spec.Name)}, nil
		}),
	})
}

