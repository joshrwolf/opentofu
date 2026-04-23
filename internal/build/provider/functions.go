// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"
	ctyfunction "github.com/zclconf/go-cty/cty/function"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type QueryFunctionResolver struct {
	registry *QuerySafetyRegistry
}

func NewQueryFunctionResolver(registry *QuerySafetyRegistry) *QueryFunctionResolver {
	if registry == nil {
		registry = NewQuerySafetyRegistry()
	}
	return &QueryFunctionResolver{registry: registry}
}

func DefaultQueryFunctionResolver() *QueryFunctionResolver {
	return NewQueryFunctionResolver(DefaultQuerySafetyRegistry())
}

func (r *QueryFunctionResolver) Resolve(_ context.Context, fn addrs.ProviderFunction, rng tfdiags.SourceRange) (*ctyfunction.Function, tfdiags.Diagnostics) {
	if r == nil {
		r = DefaultQueryFunctionResolver()
	}
	if !r.registry.IsQuerySafe(fn) {
		return nil, tfdiags.Diagnostics{}.Append(queryFunctionDiagnostic(
			rng,
			"Non-query-safe provider function in build solve",
			"Provider function "+fn.String()+" is not query-safe and cannot be executed during build solve.",
		))
	}

	var resolved ctyfunction.Function
	switch {
	case fn.ProviderName == "oci" && fn.Function == "parse":
		resolved = ociParseFunction()
	case fn.ProviderName == "time" && fn.Function == "rfc3339_parse":
		resolved = rfc3339ParseFunction()
	default:
		return nil, tfdiags.Diagnostics{}.Append(queryFunctionDiagnostic(
			rng,
			"Unsupported query-safe provider function",
			"Provider function "+fn.String()+" is marked query-safe but does not have a build-side implementation yet.",
		))
	}

	return &resolved, nil
}

func queryFunctionDiagnostic(rng tfdiags.SourceRange, summary, detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  summary,
		Detail:   detail,
		Subject:  rng.ToHCL().Ptr(),
	}
}

func ociParseFunction() ctyfunction.Function {
	return ctyfunction.New(&ctyfunction.Spec{
		Params: []ctyfunction.Parameter{{
			Name: "input",
			Type: cty.String,
		}},
		Type: ctyfunction.StaticReturnType(cty.Object(map[string]cty.Type{
			"registry":      cty.String,
			"repo":          cty.String,
			"registry_repo": cty.String,
			"digest":        cty.String,
			"pseudo_tag":    cty.String,
			"ref":           cty.String,
		})),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			input := args[0].AsString()

			ref, err := name.ParseReference(input)
			if err != nil {
				return cty.NilVal, fmt.Errorf("Failed to parse OCI reference: %v", err)
			}
			if _, ok := ref.(name.Tag); ok {
				return cty.NilVal, fmt.Errorf("Reference %s contains only a tag, but a digest is required", input)
			}

			return cty.ObjectVal(map[string]cty.Value{
				"registry":      cty.StringVal(ref.Context().RegistryStr()),
				"repo":          cty.StringVal(ref.Context().RepositoryStr()),
				"registry_repo": cty.StringVal(ref.Context().RegistryStr() + "/" + ref.Context().RepositoryStr()),
				"digest":        cty.StringVal(ref.Identifier()),
				"pseudo_tag":    cty.StringVal("unused@" + ref.Identifier()),
				"ref":           cty.StringVal(ref.String()),
			}), nil
		},
	})
}

func rfc3339ParseFunction() ctyfunction.Function {
	return ctyfunction.New(&ctyfunction.Spec{
		Params: []ctyfunction.Parameter{{
			Name: "timestamp",
			Type: cty.String,
		}},
		Type: ctyfunction.StaticReturnType(cty.Object(map[string]cty.Type{
			"year":         cty.Number,
			"year_day":     cty.Number,
			"day":          cty.Number,
			"month":        cty.Number,
			"month_name":   cty.String,
			"weekday":      cty.Number,
			"weekday_name": cty.String,
			"hour":         cty.Number,
			"minute":       cty.Number,
			"second":       cty.Number,
			"unix":         cty.Number,
			"iso_year":     cty.Number,
			"iso_week":     cty.Number,
		})),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			timestamp := args[0].AsString()

			parsed, err := time.Parse(time.RFC3339, timestamp)
			if err != nil {
				return cty.NilVal, ctyfunction.NewArgErrorf(0, "Error parsing RFC3339 timestamp: %q is not a valid RFC3339 timestamp", timestamp)
			}

			isoYear, isoWeek := parsed.ISOWeek()
			return cty.ObjectVal(map[string]cty.Value{
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
			}), nil
		},
	})
}
