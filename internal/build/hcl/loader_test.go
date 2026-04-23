// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configload"
)

func TestLoadDoesNotEagerlyValidateBuild(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_resource" "bad" {
  lifecycle {
    create_before_destroy = true
  }
}
`)

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
	if loaded == nil || loaded.Config == nil {
		t.Fatal("expected loaded config")
	}

	validateDiags := ValidateBuild(loaded.Config)
	if !validateDiags.HasErrors() {
		t.Fatal("expected validation errors")
	}
	if got := validateDiags.Err().Error(); !strings.Contains(got, "create_before_destroy") {
		t.Fatalf("expected create_before_destroy diagnostic, got:\n%s", got)
	}
}

func TestLoadBuildV1CleanConfig(t *testing.T) {
	rootDir := t.TempDir()
	writeTestFile(t, filepath.Join(rootDir, "main.tf"), `
variable "name" {
  type    = string
  default = "busybox"

  validation {
    condition     = length(var.name) > 0
    error_message = "name must not be empty"
  }
}

locals {
  tag = "${var.name}:latest"
}

output "tag" {
  value = local.tag

  precondition {
    condition     = length(local.tag) > 0
    error_message = "tag must not be empty"
  }
}
`)

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
	if loaded == nil || loaded.Config == nil {
		t.Fatal("expected loaded config")
	}

	validateDiags := ValidateBuild(loaded.Config)
	if validateDiags.HasErrors() {
		t.Fatalf("unexpected validation diagnostics: %s", validateDiags.Err())
	}
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
