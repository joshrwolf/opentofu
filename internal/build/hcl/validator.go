// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"maps"
	"slices"

	"github.com/hashicorp/hcl/v2"

	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func ValidateBuild(cfg *configs.Config) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	if cfg == nil {
		return nil
	}

	cfg.DeepEach(func(node *configs.Config) {
		module := node.Module
		if module == nil {
			return
		}

		for _, key := range slices.Sorted(maps.Keys(module.Checks)) {
			diags = diags.Append(unsupportedDiag("Check blocks are not supported by tofu build.", module.Checks[key].DeclRange))
		}
		for _, key := range slices.Sorted(maps.Keys(module.EphemeralResources)) {
			diags = diags.Append(unsupportedDiag("Ephemeral blocks are not supported by tofu build.", module.EphemeralResources[key].DeclRange))
		}
		for _, moved := range module.Moved {
			diags = diags.Append(unsupportedDiag("Moved blocks are not supported in build mode because they are plan/apply state constructs.", moved.DeclRange))
		}
		for _, removed := range module.Removed {
			diags = diags.Append(unsupportedDiag("Removed blocks are not supported in build mode because they are plan/apply state constructs.", removed.DeclRange))
		}
		for _, imp := range module.Import {
			diags = diags.Append(unsupportedDiag("Import blocks are not supported in build mode because they are plan/apply state constructs.", imp.DeclRange))
		}

		for _, key := range slices.Sorted(maps.Keys(module.ManagedResources)) {
			resource := module.ManagedResources[key]
			if resource == nil {
				continue
			}
			for _, condition := range resource.Postconditions {
				diags = diags.Append(unsupportedDiag("Postconditions are not supported by tofu build.", condition.DeclRange))
			}
			if resource.Managed == nil {
				continue
			}
			if isNullResourceCompatibilityTarget(resource) {
				diags = diags.Append(validateNullResourceCompatibility(resource))
			} else {
				for _, provisioner := range resource.Managed.Provisioners {
					diags = diags.Append(unsupportedDiag("Provisioners are not supported by tofu build.", provisioner.DeclRange))
				}
			}
			if resource.Managed.CreateBeforeDestroySet {
				diags = diags.Append(unsupportedDiag("lifecycle.create_before_destroy is not supported by tofu build.", resource.DeclRange))
			}
			if len(resource.Managed.IgnoreChanges) > 0 || resource.Managed.IgnoreAllChanges {
				diags = diags.Append(unsupportedDiag("lifecycle.ignore_changes is not supported by tofu build.", resource.DeclRange))
			}
			if len(resource.TriggersReplacement) > 0 {
				diags = diags.Append(unsupportedDiag("lifecycle.replace_triggered_by is not supported by tofu build.", resource.DeclRange))
			}
		}
		for _, key := range slices.Sorted(maps.Keys(module.DataResources)) {
			resource := module.DataResources[key]
			if resource == nil {
				continue
			}
			for _, condition := range resource.Postconditions {
				diags = diags.Append(unsupportedDiag("Postconditions are not supported by tofu build.", condition.DeclRange))
			}
		}
	})

	return diags
}

func unsupportedDiag(detail string, rng hcl.Range) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  "Unsupported syntax in build mode",
		Detail:   detail,
		Subject:  rng.Ptr(),
		Context:  &hcl.Range{Filename: rng.Filename, Start: rng.Start, End: rng.End},
	}
}
