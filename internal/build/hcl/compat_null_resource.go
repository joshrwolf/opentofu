// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/build/catalog"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

var nullResourceCompatibilitySchema = &configschema.Block{
	Attributes: map[string]*configschema.Attribute{
		"triggers": {
			Type:     cty.Map(cty.String),
			Optional: true,
		},
	},
}

var localExecCompatibilitySchema = &configschema.Block{
	Attributes: map[string]*configschema.Attribute{
		"command": {
			Type:     cty.String,
			Required: true,
		},
		"interpreter": {
			Type:     cty.List(cty.String),
			Optional: true,
		},
		"working_dir": {
			Type:     cty.String,
			Optional: true,
		},
		"environment": {
			Type:     cty.Map(cty.String),
			Optional: true,
		},
		"quiet": {
			Type:     cty.Bool,
			Optional: true,
		},
	},
}

var nullProviderAddr = addrs.NewDefaultProvider("null")

func isNullResourceCompatibilityTarget(resource *configs.Resource) bool {
	if resource == nil {
		return false
	}
	if resource.Mode != addrs.ManagedResourceMode || resource.Type != "null_resource" {
		return false
	}
	return resource.Provider == nullProviderAddr
}

func validateNullResourceCompatibility(resource *configs.Resource) tfdiags.Diagnostics {
	if resource == nil {
		return nil
	}
	if resource.Managed == nil {
		return tfdiags.Diagnostics{}.Append(unsupportedDiag(
			`Only managed "null_resource" declarations with a single create-time "local-exec" provisioner are supported by tofu build.`,
			resource.DeclRange,
		))
	}
	if resource.Managed.Connection != nil {
		return tfdiags.Diagnostics{}.Append(unsupportedDiag(
			`"null_resource" compatibility does not support resource-level connection blocks in tofu build.`,
			resource.Managed.Connection.DeclRange,
		))
	}
	if len(resource.Managed.Provisioners) != 1 {
		return tfdiags.Diagnostics{}.Append(unsupportedDiag(
			`"null_resource" compatibility currently requires exactly one create-time "local-exec" provisioner in tofu build.`,
			resource.DeclRange,
		))
	}

	provisioner := resource.Managed.Provisioners[0]
	var diags tfdiags.Diagnostics
	if provisioner.Type != "local-exec" {
		diags = diags.Append(unsupportedDiag(
			`"null_resource" compatibility only supports the "local-exec" provisioner in tofu build.`,
			provisioner.DeclRange,
		))
	}
	if provisioner.When != configs.ProvisionerWhenCreate {
		diags = diags.Append(unsupportedDiag(
			`"null_resource" compatibility only supports create-time "local-exec" provisioners in tofu build.`,
			provisioner.DeclRange,
		))
	}
	if provisioner.OnFailure != configs.ProvisionerOnFailureFail {
		diags = diags.Append(unsupportedDiag(
			`"null_resource" compatibility only supports on_failure = fail in tofu build.`,
			provisioner.DeclRange,
		))
	}
	if provisioner.Connection != nil {
		diags = diags.Append(unsupportedDiag(
			`"null_resource" compatibility does not support "connection" blocks in tofu build.`,
			provisioner.Connection.DeclRange,
		))
	}
	return diags
}

func (l *Loaded) lowerNullResourceRunPayload(ctx context.Context, addr catalog.Addr, resource *configs.Resource, runtime RuntimeResolver, moduleOptions *configs.StaticEvalOptions) (payload buildrun.Payload, deferred bool, diags tfdiags.Diagnostics) {
	if resource == nil {
		return buildrun.Payload{}, false, nil
	}

	compatDiags := validateNullResourceCompatibility(resource)
	if compatDiags.HasErrors() {
		return buildrun.Payload{}, false, compatDiags
	}

	dec, decDiags := l.newTargetDecoder(ctx, addr, resource.DeclRange, resource.Count, resource.ForEach, resource.Enabled, runtime, moduleOptions)
	if decDiags.HasErrors() {
		return buildrun.Payload{}, false, decDiags
	}

	resourceValue, resourceDeferred, resourceDiags := dec.decode(ctx, resource.Config, nullResourceCompatibilitySchema, addr.String(), resource.DeclRange)
	if resourceDeferred || resourceDiags.HasErrors() {
		return buildrun.Payload{}, resourceDeferred, resourceDiags
	}

	provisioner := resource.Managed.Provisioners[0]
	provisionerValue, provDeferred, provDiags := dec.decode(ctx, provisioner.Config, localExecCompatibilitySchema, addr.String()+` provisioner.local-exec`, provisioner.DeclRange)
	if provDeferred || provDiags.HasErrors() {
		return buildrun.Payload{}, provDeferred, provDiags
	}

	provisionerValues := provisionerValue.AsValueMap()
	commandValue := provisionerValues["command"]
	if !commandValue.IsKnown() || commandValue.IsNull() {
		return buildrun.Payload{}, true, nil
	}
	command := commandValue.AsString()

	argv := defaultLocalExecInterpreter()
	if interpreter := provisionerValues["interpreter"]; interpreter.IsKnown() && !interpreter.IsNull() && interpreter.LengthInt() != 0 {
		argv = make([]string, 0, interpreter.LengthInt())
		for _, value := range interpreter.AsValueSlice() {
			if !value.IsKnown() || value.IsNull() {
				return buildrun.Payload{}, true, nil
			}
			argv = append(argv, value.AsString())
		}
	}
	if len(argv) == 0 {
		return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid null_resource compatibility target",
			`"null_resource" local-exec compatibility requires a non-empty interpreter configuration.`,
		))
	}
	resolved, resolveDiags := resolveExecutable(argv[0], addr, "Invalid null_resource compatibility target")
	if resolveDiags.HasErrors() {
		return buildrun.Payload{}, false, resolveDiags
	}
	argv[0] = resolved
	argv = append(argv, command)

	cwd, cwdDiags := resolveWorkingDir(l.RootDir, dec.sourceDir, provisionerValues["working_dir"], addr, "Invalid null_resource compatibility target")
	if cwdDiags.HasErrors() {
		return buildrun.Payload{}, false, cwdDiags
	}

	env := defaultExecEnvironment()
	if environment := provisionerValues["environment"]; environment.IsKnown() && !environment.IsNull() {
		if env == nil {
			env = make(map[string]string)
		}
		for key, value := range environment.AsValueMap() {
			if !value.IsKnown() || value.IsNull() {
				return buildrun.Payload{}, true, nil
			}
			env[key] = value.AsString()
		}
	}

	req, reqDiags := buildrun.BuildRequest(buildrun.RequestArgs{
		Argv: argv,
		Cwd:  cwd,
		Env:  env,
	}, tfdiags.SourceRangeFromHCL(resource.DeclRange))
	if reqDiags.HasErrors() {
		return buildrun.Payload{}, false, reqDiags
	}

	triggersValue := resourceValue.AsValueMap()["triggers"]
	triggers := map[string]string{}
	if triggersValue.IsKnown() && !triggersValue.IsNull() {
		for key, value := range triggersValue.AsValueMap() {
			if !value.IsKnown() || value.IsNull() {
				return buildrun.Payload{}, true, nil
			}
			triggers[key] = value.AsString()
		}
	}
	valueData, err := json.Marshal(struct {
		Triggers map[string]string `json:"triggers,omitempty"`
	}{
		Triggers: triggers,
	})
	if err != nil {
		return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid null_resource compatibility target",
			fmt.Sprintf("Failed to encode null_resource compatibility value data for %s: %s", addr.String(), err),
		))
	}

	return buildrun.Payload{
		Request:      *req,
		ValueAdapter: buildrun.ValueAdapterNullV1,
		ValueData:    valueData,
	}, false, nil
}

func analyzeNullResourceCompatibilityConfig(analysis *targetAnalysis, resource *configs.Resource) {
	if analysis == nil || resource == nil || resource.Managed == nil {
		return
	}
	for _, provisioner := range resource.Managed.Provisioners {
		for _, attr := range bodyAttributes(provisioner.Config) {
			analysis.analyzeExpr(attr.Expr)
		}
	}
}
