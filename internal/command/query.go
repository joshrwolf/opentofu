// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/opentofu/opentofu/internal/addrs"
	buildsys "github.com/opentofu/opentofu/internal/build"
	"github.com/opentofu/opentofu/internal/build/catalog"
	buildhcl "github.com/opentofu/opentofu/internal/build/hcl"
	buildselector "github.com/opentofu/opentofu/internal/build/selector"
	"github.com/opentofu/opentofu/internal/build/solve"
	"github.com/opentofu/opentofu/internal/command/arguments"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/initwd"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/repl"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

type QueryCommand struct {
	Meta
}

type querySelection struct {
	Requested string
	Addr      catalog.Addr
	Status    string
	Reason    string
}

type querySelectorInput struct {
	Raw string
	Set buildselector.Set
}

type queryBlock struct {
	Addr  string
	Lines []string
}

func (b *queryBlock) Add(name, value string) {
	if value == "" {
		return
	}
	b.Lines = append(b.Lines, name+": "+value)
}

func (b queryBlock) String() string {
	var out strings.Builder
	out.WriteString(b.Addr)
	for _, line := range b.Lines {
		out.WriteByte('\n')
		out.WriteString("  ")
		out.WriteString(strings.ReplaceAll(line, "\n", "\n  "))
	}
	return out.String()
}

func (c *QueryCommand) Run(rawArgs []string) int {
	ctx := c.CommandContext()

	common, rawArgs := arguments.ParseView(rawArgs)
	c.View.Configure(common)
	c.View.DiagsWithNewline()

	args, closer, diags := arguments.ParseQuery(rawArgs)
	defer closer()

	queryView := views.NewQuery(c.View)
	if diags.HasErrors() {
		queryView.Diagnostics(diags)
		c.View.HelpPrompt("query")
		return 1
	}

	c.Meta.variableArgs = args.Vars.All()
	ctx, done := c.InterruptibleContext(ctx)
	defer done()

	rootDir := c.WorkingDir.NormalizePath(c.WorkingDir.RootModuleDir())
	abort, installDiags := c.installModules(ctx, rootDir, configs.DefaultTestDirectory, false, true, initwd.ModuleInstallHooksImpl{}, c.View)
	diags = diags.Append(installDiags)
	if abort || diags.HasErrors() {
		queryView.Diagnostics(diags)
		return 1
	}

	loader, err := c.initConfigLoader()
	if err != nil {
		queryView.Diagnostics(diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Error loading the configuration",
			err.Error(),
		)))
		return 1
	}
	if err := loader.RefreshModules(); err != nil {
		queryView.Diagnostics(diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to read module manifest",
			fmt.Sprintf("After installing modules, OpenTofu could not re-read the manifest of installed modules. This is a bug in OpenTofu. %s.", err),
		)))
		return 1
	}

	rootCall, callDiags := c.rootModuleCall(ctx, rootDir)
	diags = diags.Append(callDiags)
	if callDiags.HasErrors() {
		queryView.Diagnostics(diags)
		return 1
	}

	loaded, loadDiags := buildsys.Load(ctx, loader, buildhcl.LoadRequest{RootDir: rootDir}, rootCall)
	diags = diags.Append(loadDiags)
	if loaded == nil {
		queryView.Diagnostics(diags)
		return 1
	}

	var providerFactories map[addrs.Provider]providers.Factory
	var providerFactoryErr error
	if c.testingOverrides != nil && c.testingOverrides.Providers != nil {
		providerFactories = c.testingOverrides.Providers
	} else {
		providerFactories, providerFactoryErr = c.providerFactories()
	}

	inspector, inspectorDiags := buildsys.NewInspector(ctx, loaded, buildsys.InspectorConfig{
		ProviderFactories:    providerFactories,
		ProviderFactoryError: providerFactoryErr,
	})
	diags = diags.Append(inspectorDiags)
	if inspector == nil {
		queryView.Diagnostics(diags)
		return 1
	}

	selectorInputs, selectorInputDiags := parseQuerySelectorInputs(args.RawSelectors)
	diags = diags.Append(selectorInputDiags)
	if selectorInputDiags.HasErrors() {
		diags = diags.Append(inspector.Close(ctx))
		queryView.Diagnostics(diags)
		return 1
	}

	first := true
	selectionDiags := c.streamQuerySelections(ctx, inspector.Catalog(), inspector.Solver(), selectorInputs, args.Selectors, func(sel querySelection) bool {
		block := c.querySelection(ctx, inspector.Solver(), sel)
		if args.ReverseDeps {
			c.queryReverseDeps(ctx, inspector.Solver(), sel, &block)
		}
		if !first {
			queryView.Result("")
		}
		first = false
		queryView.Result(block.String())
		return true
	})
	diags = diags.Append(selectionDiags)

	diags = diags.Append(inspector.Close(ctx))
	queryView.Diagnostics(diags)
	if diags.HasErrors() {
		return 1
	}
	return 0
}

func (c *QueryCommand) streamQuerySelections(ctx context.Context, cat *catalog.Catalog, solver *solve.Solver, inputs []querySelectorInput, selectors buildselector.Set, yield func(querySelection) bool) tfdiags.Diagnostics {
	if cat == nil || solver == nil || yield == nil {
		return nil
	}

	seen := make(map[catalog.AddrKey]struct{})
	buildsys.StreamCandidateMatches(cat, selectors, func(addr catalog.Addr) bool {
		return c.streamQueryCandidate(ctx, solver, inputs, selectors, addr, seen, yield)
	})
	return nil
}

func (c *QueryCommand) streamQueryCandidate(ctx context.Context, solver *solve.Solver, inputs []querySelectorInput, selectors buildselector.Set, addr catalog.Addr, seen map[catalog.AddrKey]struct{}, yield func(querySelection) bool) bool {
	requested := requestedQuerySubject(inputs, addr)
	modules, status, reason := c.querySelectionModules(ctx, solver, addr.Module)
	if status != "" {
		return yieldUniqueQuerySelection(querySelection{
			Requested: requested,
			Addr:      addr,
			Status:    status,
			Reason:    reason,
		}, seen, yield)
	}

	switch addr.Kind {
	case catalog.TargetKindModule:
		for _, module := range modules {
			concrete := catalog.ModuleAddr(module)
			if !selectors.Match(concrete).Exact() {
				continue
			}
			if !yieldUniqueQuerySelection(querySelection{Requested: concrete.String(), Addr: concrete}, seen, yield) {
				return false
			}
		}
	case catalog.TargetKindOutput:
		for _, module := range modules {
			concrete := addr
			concrete.Module = module
			if !selectors.Match(concrete).Exact() {
				continue
			}
			if !yieldUniqueQuerySelection(querySelection{Requested: concrete.String(), Addr: concrete}, seen, yield) {
				return false
			}
		}
	case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
		for _, module := range modules {
			concreteDecl := addr
			concreteDecl.Module = module
			concreteDecl.Key = catalog.NoKey()

			instances, instanceDiags := solver.TargetInstances(ctx, concreteDecl)
			if instanceDiags.HasErrors() {
				if !yieldUniqueQuerySelection(querySelection{
					Requested: requested,
					Addr:      concreteDecl,
					Status:    "blocked",
					Reason:    describeDiagnostics(instanceDiags),
				}, seen, yield) {
					return false
				}
				continue
			}
			for _, instance := range instances.Addrs {
				if !selectors.Match(instance).Exact() {
					continue
				}
				if !yieldUniqueQuerySelection(querySelection{Requested: instance.String(), Addr: instance}, seen, yield) {
					return false
				}
			}
		}
	}

	return true
}

func (c *QueryCommand) querySelectionModules(ctx context.Context, solver *solve.Solver, module catalog.ModulePath) ([]catalog.ModulePath, string, string) {
	if module.Len() == 0 {
		return []catalog.ModulePath{catalog.RootModule()}, "", ""
	}

	packages, diags := solver.PackageInstances(ctx, module)
	if diags.HasErrors() {
		return nil, "blocked", describeDiagnostics(diags)
	}
	return packages.Modules, "", ""
}

func yieldUniqueQuerySelection(sel querySelection, seen map[catalog.AddrKey]struct{}, yield func(querySelection) bool) bool {
	if _, ok := seen[sel.Addr.Identity()]; ok {
		return true
	}
	seen[sel.Addr.Identity()] = struct{}{}
	return yield(sel)
}

func (c *QueryCommand) querySelection(ctx context.Context, solver *solve.Solver, sel querySelection) queryBlock {
	subject := sel.Addr.String()
	if sel.Status != "" && sel.Requested != "" {
		subject = sel.Requested
	}
	block := queryBlock{Addr: subject}
	block.Add("kind", sel.Addr.Kind.String())

	if sel.Status != "" {
		block.Add("status", sel.Status)
		block.Add("reason", sel.Reason)
		if sel.Requested != "" && sel.Requested != sel.Addr.String() {
			block.Add("resolved", sel.Addr.String())
		}
		return block
	}

	switch sel.Addr.Kind {
	case catalog.TargetKindModule:
		block.Add("status", "selected")
	case catalog.TargetKindOutput:
		c.queryOutput(ctx, solver, sel.Addr, &block)
	case catalog.TargetKindResource, catalog.TargetKindData, catalog.TargetKindRun:
		c.queryTarget(ctx, solver, sel.Addr, &block)
	default:
		block.Add("status", "blocked")
		block.Add("reason", "unsupported selection kind")
	}

	return block
}

func (c *QueryCommand) queryReverseDeps(ctx context.Context, solver *solve.Solver, sel querySelection, block *queryBlock) {
	if sel.Status != "" {
		return
	}

	rdeps, diags := solver.ReverseDeps(ctx, sel.Addr)
	if diags.HasErrors() {
		block.Add("reverse_deps_status", "blocked")
		block.Add("reverse_deps_reason", describeDiagnostics(diags))
		return
	}
	if !rdeps.Complete {
		block.Add("reverse_deps_status", "partial")
		block.Add("reverse_deps_reason", describeDiagnostics(rdeps.Reason))
	}
	if len(rdeps.Addrs) == 0 {
		block.Add("reverse_deps", "[]")
		return
	}

	lines := make([]string, 0, len(rdeps.Addrs))
	for _, addr := range rdeps.Addrs {
		lines = append(lines, addr.String())
	}
	block.Add("reverse_deps", strings.Join(lines, "\n"))
}

func (c *QueryCommand) queryOutput(ctx context.Context, solver *solve.Solver, addr catalog.Addr, block *queryBlock) {
	result, diags := solver.OutputValue(ctx, addr)
	if diags.HasErrors() {
		block.Add("status", "blocked")
		block.Add("reason", describeDiagnostics(diags))
		return
	}
	if result.Known && !result.Deferred {
		block.Add("status", "known")
		if result.Value != cty.NilVal {
			block.Add("value", repl.FormatValue(result.Value, 0))
		}
		return
	}
	block.Add("status", "deferred")
}

func (c *QueryCommand) queryTarget(ctx context.Context, solver *solve.Solver, addr catalog.Addr, block *queryBlock) {
	spec, diags := solver.ActionSpec(ctx, addr)
	if diags.HasErrors() {
		config, configDiags := solver.TargetConfig(ctx, addr)
		if configDiags.HasErrors() {
			block.Add("status", "blocked")
			block.Add("reason", describeDiagnostics(configDiags))
			return
		}
		if config.Deferred {
			block.Add("status", "deferred")
			block.Add("reason", describeDiagnostics(diags))
			return
		}
		block.Add("status", "blocked")
		block.Add("reason", describeDiagnostics(diags))
		return
	}
	block.Add("status", "lowerable")
	block.Add("runner", string(spec.Runner.Kind))
	block.Add("action_key", spec.Key.String())
	block.Add("exec_deps", strconv.Itoa(len(spec.ExecDeps)))
	block.Add("after", strconv.Itoa(len(spec.After)))
}

func describeDiagnostics(diags tfdiags.Diagnostics) string {
	if len(diags) == 0 {
		return ""
	}

	var messages []string
	seen := map[string]struct{}{}
	for _, diag := range diags {
		if diag == nil {
			continue
		}

		desc := diag.Description()
		summary := strings.TrimSpace(desc.Summary)
		detail := strings.TrimSpace(desc.Detail)
		if summary == "" && detail == "" {
			continue
		}

		message := summary
		if detail != "" && detail != summary {
			if message == "" {
				message = detail
			} else {
				message = message + ": " + detail
			}
		}
		if _, ok := seen[message]; ok {
			continue
		}
		seen[message] = struct{}{}
		messages = append(messages, message)
	}
	return strings.Join(messages, "\n")
}

func parseQuerySelectorInputs(rawSelectors []string) ([]querySelectorInput, tfdiags.Diagnostics) {
	var (
		ret   []querySelectorInput
		diags tfdiags.Diagnostics
	)

	for _, raw := range rawSelectors {
		set, setErr := buildselector.ParseAll([]string{raw})
		if setErr != nil {
			diags = diags.Append(setErr)
		}
		if setErr != nil || len(set) == 0 {
			continue
		}
		ret = append(ret, querySelectorInput{
			Raw: raw,
			Set: set,
		})
	}

	return ret, diags
}

func requestedQuerySubject(inputs []querySelectorInput, addr catalog.Addr) string {
	for _, input := range inputs {
		if strings.ContainsRune(input.Raw, '*') {
			continue
		}
		if input.Set.Match(addr).Declaration() {
			return input.Raw
		}
	}
	return addr.String()
}

func (c *QueryCommand) Help() string {
	helpText := `
Usage: tofu [global options] query [SELECTOR...]

  Queries build-native facts for the selected addresses without executing any
  build actions.

  Selectors use Terraform-style addresses with wildcard support, such as:

    **
    module.images.**
    module.images["amd64"].output.digest
    test_resource.base[*]
    data.test_data_source.base

  Query may load and solve pure or query-safe facts, but it will not execute
  provider resources, data sources, or non-query-safe provider functions.

Options:

  -rdeps               Include direct reverse dependencies for each selected
                       address when they can be determined without execution.
  -var 'foo=bar'      Set a value for one of the input variables in the root
                      module of the configuration.
  -var-file=filename  Load variable values from the given file, in addition
                      to the default variable files.
`
	return strings.TrimSpace(helpText)
}

func (c *QueryCommand) Synopsis() string {
	return "Inspect build-native facts without executing build actions"
}
