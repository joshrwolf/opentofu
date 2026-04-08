// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	chofuProvider "github.com/opentofu/opentofu/internal/builtin/providers/chofu"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/plans/objchange"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/opentofu/opentofu/internal/tracing"
	"github.com/opentofu/opentofu/internal/tracing/traceattrs"
)

// chofuProviderAddr is the built-in address for the chofu provider.
var chofuProviderAddr = addrs.NewBuiltInProvider("chofu")

// evalBuiltinResource evaluates a built-in chofu_* resource. The engine
// handles config eval, content hashing, dry-run, hooks, and UI events.
// The provider package owns parsing, execution, and output validation.
func (wc *walkContext) evalBuiltinResource(
	ctx context.Context,
	scope *lang.Scope,
	v *Vertex,
	id ID,
) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	if v.ResourceCfg == nil || v.Schema == nil {
		wc.results.Values[id] = cty.DynamicVal
		return nil
	}

	configVal, evalDiags := evalResourceConfig(ctx, scope, v)
	diags = diags.Append(evalDiags)
	if evalDiags.HasErrors() {
		wc.results.Values[id] = cty.DynamicVal
		return diags
	}
	resType := v.ResourceAddr.Resource.Resource.Type
	depHashes := wc.collectDepHashes(id)
	contentHash := ComputeContentHash(resType, configVal, depHashes)
	wc.hashIndex.Record(v.ResourceAddr, contentHash)

	if wc.dryRun {
		wc.results.Values[id] = objchange.PlannedUnknownObject(v.Schema, configVal)
		return diags
	}

	// Hooks + UI — same as any resource.
	action := "exec"
	if wc.hooks != nil {
		wc.hooks.PreBuildResource(v.ResourceAddr, action)
	}
	wc.ui.Event(BuildEvent{ResourceStart: &ResourceStartEvent{
		Addr:     v.ResourceAddr,
		Action:   action,
		Provider: v.ProviderAddr,
	}})
	execStart := time.Now()

	// Dispatch to the provider for execution.
	_, execSpan := tracing.Tracer().Start(ctx, "chofu.EvalExec",
		tracing.SpanAttributes(
			traceattrs.String("chofu.exec.addr", v.ResourceAddr.String()),
		),
	)

	var computed map[string]cty.Value
	var execErr error

	switch resType {
	case "chofu_exec":
		result, runDiags := chofuProvider.RunExec(ctx, configVal)
		diags = diags.Append(runDiags)
		if runDiags.HasErrors() {
			execErr = runDiags.Err()
		} else {
			filesMap := make(map[string]cty.Value, len(result.Files))
			for name, path := range result.Files {
				filesMap[name] = cty.StringVal(path)
			}
			computed = map[string]cty.Value{
				"stdout":    cty.StringVal(result.StdoutPath),
				"stderr":    cty.StringVal(result.StderrPath),
				"exit_code": cty.NumberIntVal(int64(result.ExitCode)),
			}
			if len(filesMap) > 0 {
				computed["files"] = cty.MapVal(filesMap)
			} else {
				computed["files"] = cty.MapValEmpty(cty.String)
			}
		}

	default:
		diags = diags.Append(tfdiags.Sourceless(tfdiags.Error,
			fmt.Sprintf("unknown built-in resource type %q", resType),
			"This is a bug in the build engine."))
		execErr = fmt.Errorf("unknown type %q", resType)
	}

	execSpan.End()

	if wc.hooks != nil {
		wc.hooks.PostBuildResource(v.ResourceAddr, action, execErr)
	}
	wc.ui.Event(BuildEvent{ResourceComplete: &ResourceCompleteEvent{
		Addr:        v.ResourceAddr,
		Action:      action,
		Duration:    time.Since(execStart),
		ContentHash: contentHash,
		Err:         execErr,
	}})

	if execErr != nil {
		wc.results.Values[id] = cty.DynamicVal
		return stampAddress(diags, v.ResourceAddr.String())
	}

	// Merge config attrs with computed outputs.
	attrs := configVal.AsValueMap()
	if attrs == nil {
		attrs = make(map[string]cty.Value)
	}
	maps.Copy(attrs, computed)
	wc.results.Values[id] = cty.ObjectVal(attrs)

	return stampAddress(diags, v.ResourceAddr.String())
}
