// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/opentofu/opentofu/internal/addrs"
	buildsys "github.com/opentofu/opentofu/internal/build"
	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildhcl "github.com/opentofu/opentofu/internal/build/hcl"
	"github.com/opentofu/opentofu/internal/command/arguments"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/initwd"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

type BuildCommand struct {
	Meta
}

func (c *BuildCommand) Run(rawArgs []string) int {
	ctx := c.CommandContext()

	common, rawArgs := arguments.ParseView(rawArgs)
	c.View.Configure(common)
	c.View.DiagsWithNewline()

	args, closer, diags := arguments.ParseBuild(rawArgs)
	defer closer()

	outputView := views.NewOutput(args.ViewOptions, c.View)
	if diags.HasErrors() {
		outputView.Diagnostics(diags)
		c.View.HelpPrompt("build")
		return 1
	}

	c.Meta.variableArgs = args.Vars.All()
	ctx, done := c.InterruptibleContext(ctx)
	defer done()

	rootDir := c.WorkingDir.NormalizePath(c.WorkingDir.RootModuleDir())
	abort, installDiags := c.installModules(ctx, rootDir, configs.DefaultTestDirectory, false, true, initwd.ModuleInstallHooksImpl{}, c.View)
	diags = diags.Append(installDiags)
	if abort || diags.HasErrors() {
		outputView.Diagnostics(diags)
		return 1
	}

	loader, err := c.initConfigLoader()
	if err != nil {
		outputView.Diagnostics(diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Error loading the configuration",
			err.Error(),
		)))
		return 1
	}
	if err := loader.RefreshModules(); err != nil {
		outputView.Diagnostics(diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Failed to read module manifest",
			fmt.Sprintf("After installing modules, OpenTofu could not re-read the manifest of installed modules. This is a bug in OpenTofu. %s.", err),
		)))
		return 1
	}

	rootCall, callDiags := c.rootModuleCall(ctx, rootDir)
	diags = diags.Append(callDiags)
	if callDiags.HasErrors() {
		outputView.Diagnostics(diags)
		return 1
	}

	cache := engine.NewFileCache(filepath.Join(rootDir, ".tofu", "build", "cache"))
	if args.CacheClear {
		diags = diags.Append(cache.Clear(ctx))
	}
	var engineCache engine.Cache
	if args.UseCache {
		engineCache = cache
	}

	providerFactories, providerFactoryErr := c.buildProviderFactories()

	log.Printf("[INFO] tofu build: loading configuration from %s", rootDir)
	loaded, loadDiags := buildsys.Load(ctx, loader, buildhcl.LoadRequest{RootDir: rootDir}, rootCall)
	diags = diags.Append(loadDiags)
	if loaded == nil {
		outputView.Diagnostics(diags)
		return 1
	}

	buildEngine, engineDiags := buildsys.NewEngine(ctx, loaded, buildsys.EngineConfig{
		ProviderFactories:    providerFactories,
		ProviderFactoryError: providerFactoryErr,
		Parallelism:          args.Parallelism,
		Cache:                engineCache,
		Logf:                 log.Printf,
	})
	diags = diags.Append(engineDiags)
	if buildEngine == nil {
		outputView.Diagnostics(diags)
		return 1
	}
	defer func() { diags = diags.Append(buildEngine.Close(ctx)) }()
	log.Printf("[INFO] tofu build: configuration loaded")

	log.Printf("[INFO] tofu build: executing build")
	result, buildDiags := buildEngine.Execute(ctx, args.Selectors)
	diags = diags.Append(buildDiags)
	if diags.HasErrors() {
		outputView.Diagnostics(diags)
		return 1
	}
	log.Printf("[INFO] tofu build: execution complete")

	outputName, outputs := formatBuildOutputs(loaded.Catalog, result.Outputs)
	if len(outputs) > 0 {
		diags = diags.Append(outputView.Output(outputName, outputs))
	}

	outputView.Diagnostics(diags)
	if diags.HasErrors() {
		return 1
	}
	return 0
}

func (c *BuildCommand) buildProviderFactories() (map[addrs.Provider]providers.Factory, error) {
	if c.testingOverrides != nil && c.testingOverrides.Providers != nil {
		return c.testingOverrides.Providers, nil
	}
	return c.providerFactories()
}

func formatBuildOutputs(cat *catalog.Catalog, values []buildsys.OutputValue) (string, map[string]*states.OutputValue) {
	if len(values) == 0 {
		return "", nil
	}

	outputs := make(map[string]*states.OutputValue, len(values))
	for _, ov := range values {
		name := outputDisplayName(ov.Addr)
		outputs[name] = &states.OutputValue{
			Value:     ov.Value,
			Sensitive: outputSensitive(cat, ov.Addr),
		}
	}

	singleName := ""
	if len(values) == 1 {
		singleName = outputDisplayName(values[0].Addr)
	}
	return singleName, outputs
}

func outputDisplayName(addr catalog.Addr) string {
	if addr.Module.Len() == 0 {
		return addr.Name
	}
	return addr.String()
}

func outputSensitive(cat *catalog.Catalog, addr catalog.Addr) bool {
	if cat == nil {
		return false
	}

	declAddr := addr
	declAddr.Module = declAddr.Module.Declaration()
	outputDecl, ok := cat.Output(declAddr)
	if !ok {
		return false
	}

	outputCfg, ok := outputDecl.Payload.(*configs.Output)
	if !ok || outputCfg == nil {
		return false
	}
	return outputCfg.Sensitive
}

func (c *BuildCommand) Help() string {
	helpText := `
Usage: tofu [global options] build [options] [SELECTOR...]

  Builds selected targets using the build-native architecture.

  Selectors use Terraform-style addresses with wildcard support, such as:

    **
    module.images.**
    module.images["amd64"].output.digest
    oci_image.base[*]
    data.oci_repository.base

Options:

  -parallelism=N      Limit concurrent build action execution.
  -json               Print selected outputs in JSON format.
  -json-into=out.json Produce the same output as -json, but sent directly
                      to the given file.
  -raw                Print a single selected output as a raw primitive.
  -no-cache           Disable the workspace-local build cache.
  -cache-clear        Clear the workspace-local build cache before building.

  -var 'foo=bar'      Set a value for one of the input variables in the root
                      module of the configuration.
  -var-file=filename  Load variable values from the given file, in addition
                      to the default variable files.
`
	return strings.TrimSpace(helpText)
}

func (c *BuildCommand) Synopsis() string {
	return "Build selected targets without state or backends"
}
