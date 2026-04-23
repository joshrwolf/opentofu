// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func unsupportedTargetSummary() string {
	return "Unsupported provider target in build mode"
}

func unsupportedTargetDetail(kind catalog.RunnerKind, provider addrs.Provider, typeName string) string {
	noun := "Target type"
	switch kind {
	case catalog.RunnerKindProviderResource:
		noun = "Resource type"
	case catalog.RunnerKindProviderData:
		noun = "Data source"
	}

	providerDetail := ""
	if provider != (addrs.Provider{}) {
		providerDetail = " from provider " + provider.String()
	}
	return fmt.Sprintf("%s %q%s is not supported in build mode.", noun, typeName, providerDetail)
}

func UnsupportedTargetError(kind catalog.RunnerKind, provider addrs.Provider, typeName string) tfdiags.Diagnostics {
	return tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
		tfdiags.Error,
		unsupportedTargetSummary(),
		unsupportedTargetDetail(kind, provider, typeName),
	))
}

func UnsupportedTargetDiagnostics(kind catalog.RunnerKind, provider addrs.Provider, typeName string, src catalog.SourceRef) tfdiags.Diagnostics {
	return tfdiags.Diagnostics{}.Append(&hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  unsupportedTargetSummary(),
		Detail:   unsupportedTargetDetail(kind, provider, typeName),
		Subject:  sourceRangeFromSourceRef(src).Ptr(),
	})
}

func sourceRangeFromSourceRef(src catalog.SourceRef) hcl.Range {
	return hcl.Range{
		Filename: src.Filename,
		Start: hcl.Pos{
			Line:   src.Start.Line,
			Column: src.Start.Column,
			Byte:   src.Start.Byte,
		},
		End: hcl.Pos{
			Line:   src.End.Line,
			Column: src.End.Column,
			Byte:   src.End.Byte,
		},
	}
}
