// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestQueryFunctionResolverResolveOCIParse(t *testing.T) {
	resolver := DefaultQueryFunctionResolver()
	fn, diags := resolver.Resolve(t.Context(), addrs.ProviderFunction{ProviderName: "oci", Function: "parse"}, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}

	got, err := fn.Call([]cty.Value{cty.StringVal("cgr.dev/chainguard/wolfi-base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	want := cty.ObjectVal(map[string]cty.Value{
		"registry":      cty.StringVal("cgr.dev"),
		"repo":          cty.StringVal("chainguard/wolfi-base"),
		"registry_repo": cty.StringVal("cgr.dev/chainguard/wolfi-base"),
		"digest":        cty.StringVal("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		"pseudo_tag":    cty.StringVal("unused@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		"ref":           cty.StringVal("cgr.dev/chainguard/wolfi-base@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	})
	if !got.RawEquals(want) {
		t.Fatalf("wrong parsed value: got %s want %s", got.GoString(), want.GoString())
	}
}

func TestQueryFunctionResolverResolveRFC3339Parse(t *testing.T) {
	resolver := DefaultQueryFunctionResolver()
	fn, diags := resolver.Resolve(t.Context(), addrs.ProviderFunction{ProviderName: "time", Function: "rfc3339_parse"}, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}

	got, err := fn.Call([]cty.Value{cty.StringVal("2023-07-25T23:43:16Z")})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

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
		t.Fatalf("wrong parsed value: got %s want %s", got.GoString(), want.GoString())
	}
}

func TestQueryFunctionResolverPreservesDiagnosticRange(t *testing.T) {
	resolver := DefaultQueryFunctionResolver()
	rng := tfdiags.SourceRange{
		Filename: "main.tf",
		Start:    tfdiags.SourcePos{Line: 3, Column: 11, Byte: 27},
		End:      tfdiags.SourcePos{Line: 3, Column: 29, Byte: 45},
	}

	_, diags := resolver.Resolve(t.Context(), addrs.ProviderFunction{ProviderName: "oci", Function: "get"}, rng)
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	got := diags[0].Source().Subject
	if got == nil || !got.Equal(&rng) {
		t.Fatalf("wrong diagnostic range: got %#v want %#v", got, rng)
	}
}
