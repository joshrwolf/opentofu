// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"
	"fmt"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

var externalCompatibilitySchema = &configschema.Block{
	Attributes: map[string]*configschema.Attribute{
		"program": {
			Type:     cty.List(cty.String),
			Required: true,
		},
		"query": {
			Type:     cty.Map(cty.String),
			Optional: true,
		},
		"working_dir": {
			Type:     cty.String,
			Optional: true,
		},
	},
}

var externalProviderAddr = addrs.NewDefaultProvider("external")

func builtinRunTargetSpec(resource *configs.Resource) *catalog.RunnerSpec {
	switch {
	case isExternalCompatibilityTarget(resource), isNullResourceCompatibilityTarget(resource):
	default:
		return nil
	}
	return &catalog.RunnerSpec{
		Kind:     catalog.RunnerKindBuiltinRun,
		TypeName: resource.Type,
	}
}

func isBuiltinRunCompatibilityTarget(target *catalog.TargetDecl) bool {
	return target != nil && target.Runner != nil && target.Runner.Kind == catalog.RunnerKindBuiltinRun
}

func isExternalCompatibilityTarget(resource *configs.Resource) bool {
	if resource == nil {
		return false
	}
	if resource.Mode != addrs.DataResourceMode || resource.Type != "external" {
		return false
	}
	return resource.Provider == externalProviderAddr
}

func (l *Loaded) lowerBuiltinRunPayload(ctx context.Context, addr catalog.Addr, targetDecl *catalog.TargetDecl, resource *configs.Resource, runtime RuntimeResolver, moduleOptions *configs.StaticEvalOptions) (payload buildrun.Payload, deferred bool, diags tfdiags.Diagnostics) {
	if targetDecl == nil || targetDecl.Runner == nil {
		return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Target "+addr.String()+" does not declare a builtin-run lowering path.",
		))
	}

	switch {
	case isExternalCompatibilityTarget(resource):
		return l.lowerExternalRunPayload(ctx, addr, resource, runtime, moduleOptions)
	case isNullResourceCompatibilityTarget(resource):
		return l.lowerNullResourceRunPayload(ctx, addr, resource, runtime, moduleOptions)
	default:
		return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Build source error",
			"Target "+addr.String()+" does not have a supported builtin-run lowering implementation.",
		))
	}
}

func (l *Loaded) lowerExternalRunPayload(ctx context.Context, addr catalog.Addr, resource *configs.Resource, runtime RuntimeResolver, moduleOptions *configs.StaticEvalOptions) (payload buildrun.Payload, deferred bool, diags tfdiags.Diagnostics) {
	if resource == nil {
		return buildrun.Payload{}, false, nil
	}

	dec, decDiags := l.newTargetDecoder(ctx, addr, resource.DeclRange, resource.Count, resource.ForEach, resource.Enabled, runtime, moduleOptions)
	if decDiags.HasErrors() {
		return buildrun.Payload{}, false, decDiags
	}

	value, deferred, decodeDiags := dec.decode(ctx, resource.Config, externalCompatibilitySchema, addr.String(), resource.DeclRange)
	if deferred || decodeDiags.HasErrors() {
		return buildrun.Payload{}, deferred, decodeDiags
	}

	values := value.AsValueMap()
	program := values["program"]
	if program.IsNull() || !program.IsKnown() {
		return buildrun.Payload{}, true, nil
	}
	argv := make([]string, 0, program.LengthInt())
	for _, arg := range program.AsValueSlice() {
		if !arg.IsKnown() || arg.IsNull() {
			return buildrun.Payload{}, true, nil
		}
		argv = append(argv, arg.AsString())
	}
	resolved, resolveDiags := resolveExecutable(argv[0], addr, "Invalid external compatibility target")
	if resolveDiags.HasErrors() {
		return buildrun.Payload{}, false, resolveDiags
	}
	argv[0] = resolved

	query := values["query"]
	stdin := []byte("{}")
	if query.IsKnown() && !query.IsNull() {
		queryJSON, err := ctyjson.Marshal(query, query.Type())
		if err != nil {
			return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid external compatibility target",
				fmt.Sprintf("Failed to encode query payload for %s: %s", addr.String(), err),
			))
		}
		stdin = queryJSON
	}

	cwd, cwdDiags := resolveWorkingDir(l.RootDir, dec.sourceDir, values["working_dir"], addr, "Invalid external compatibility target")
	if cwdDiags.HasErrors() {
		return buildrun.Payload{}, false, cwdDiags
	}

	req, reqDiags := buildrun.BuildRequest(buildrun.RequestArgs{
		Argv:      argv,
		Cwd:       cwd,
		Env:       defaultExecEnvironment(),
		InputMode: buildrun.InputModeJSON,
		Stdin:     stdin,
		Decode:    buildrun.DecodeModeJSON,
	}, tfdiags.SourceRangeFromHCL(resource.DeclRange))
	if reqDiags.HasErrors() {
		return buildrun.Payload{}, false, reqDiags
	}

	return buildrun.Payload{
		Request:      *req,
		ValueAdapter: buildrun.ValueAdapterExternalV1,
	}, false, nil
}
