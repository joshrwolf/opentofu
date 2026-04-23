// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestDecodeConfigRequestRejectsUnsupportedVersion(t *testing.T) {
	_, diags := DecodeConfigRequest(ConfigRequest{
		Provider: addrs.NewDefaultProvider("test"),
		Payload:  []byte(`{"version":"build-provider-config-request-v0","config_type":"e30=","config":"e30="}`),
	}, tfdiags.SourceRange{})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "unsupported version") {
		t.Fatalf("missing unsupported version diagnostic in:\n%s", got)
	}
}
