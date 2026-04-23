// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"context"
	"fmt"
	"math/big"
	"path/filepath"

	"github.com/opentofu/opentofu/internal/build/catalog"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

var nativeRunSchema = &configschema.Block{
	Attributes: map[string]*configschema.Attribute{
		"program": {
			Type:     cty.List(cty.String),
			Optional: true,
		},
		"command": {
			Type:     cty.String,
			Optional: true,
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
		"stdin": {
			Type:     cty.String,
			Optional: true,
		},
		"stdin_json": {
			Type:     cty.DynamicPseudoType,
			Optional: true,
		},
		"decode": {
			Type:     cty.String,
			Optional: true,
		},
		"allowed_exit_codes": {
			Type:     cty.List(cty.Number),
			Optional: true,
		},
	},
}

func (l *Loaded) lowerNativeRunPayload(ctx context.Context, addr catalog.Addr, run *configs.Run, runtime RuntimeResolver, moduleOptions *configs.StaticEvalOptions) (payload buildrun.Payload, deferred bool, diags tfdiags.Diagnostics) {
	if run == nil {
		return buildrun.Payload{}, false, nil
	}

	attrs, _ := run.Config.JustAttributes()
	_, hasProgram := attrs["program"]
	_, hasCommand := attrs["command"]
	_, hasInterpreter := attrs["interpreter"]
	switch {
	case hasProgram && hasCommand:
		return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid run block",
			`Run blocks may set only one of "program" or "command".`,
		))
	case !hasProgram && !hasCommand:
		return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid run block",
			`Run blocks require either "program" or "command".`,
		))
	case hasInterpreter && !hasCommand:
		return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Invalid run block",
			`Run blocks may set "interpreter" only when using "command".`,
		))
	}

	dec, decDiags := l.newTargetDecoder(ctx, addr, run.DeclRange, run.Count, run.ForEach, run.Enabled, runtime, moduleOptions)
	if decDiags.HasErrors() {
		return buildrun.Payload{}, false, decDiags
	}

	value, deferred, decodeDiags := dec.decode(ctx, run.Config, nativeRunSchema, addr.String(), run.DeclRange)
	if deferred || decodeDiags.HasErrors() {
		return buildrun.Payload{}, deferred, decodeDiags
	}

	values := value.AsValueMap()
	programValue := values["program"]
	commandValue := values["command"]
	interpreterValue := values["interpreter"]

	var argv []string
	if hasProgram {
		if !programValue.IsKnown() || programValue.IsNull() {
			return buildrun.Payload{}, true, nil
		}
		argv = make([]string, 0, programValue.LengthInt())
		for _, arg := range programValue.AsValueSlice() {
			if !arg.IsKnown() || arg.IsNull() {
				return buildrun.Payload{}, true, nil
			}
			argv = append(argv, arg.AsString())
		}
		if len(argv) == 0 {
			return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid run block",
				`Run blocks require a non-empty "program" list.`,
			))
		}
		if !filepath.IsAbs(argv[0]) {
			return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid run block",
				`Run blocks require an explicit absolute executable path in program[0].`,
			))
		}
		argv[0] = filepath.Clean(argv[0])
	} else {
		if !commandValue.IsKnown() || commandValue.IsNull() {
			return buildrun.Payload{}, true, nil
		}
		command := commandValue.AsString()
		argv = defaultLocalExecInterpreter()
		if hasInterpreter {
			if !interpreterValue.IsKnown() || interpreterValue.IsNull() {
				return buildrun.Payload{}, true, nil
			}
			argv = make([]string, 0, interpreterValue.LengthInt())
			for _, value := range interpreterValue.AsValueSlice() {
				if !value.IsKnown() || value.IsNull() {
					return buildrun.Payload{}, true, nil
				}
				argv = append(argv, value.AsString())
			}
		}
		if len(argv) == 0 {
			return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid run block",
				`Run blocks using "command" require a non-empty interpreter configuration.`,
			))
		}
		resolved, resolveDiags := resolveExecutable(argv[0], addr, "Invalid run block")
		if resolveDiags.HasErrors() {
			return buildrun.Payload{}, false, resolveDiags
		}
		argv[0] = resolved
		argv = append(argv, command)
	}

	cwd, cwdDiags := resolveWorkingDir(l.RootDir, dec.sourceDir, values["working_dir"], addr, "Invalid run block")
	if cwdDiags.HasErrors() {
		return buildrun.Payload{}, false, cwdDiags
	}

	var env map[string]string
	if environment := values["environment"]; environment.IsKnown() && !environment.IsNull() {
		env = make(map[string]string, environment.LengthInt())
		for key, value := range environment.AsValueMap() {
			if !value.IsKnown() || value.IsNull() {
				return buildrun.Payload{}, true, nil
			}
			env[key] = value.AsString()
		}
	}

	inputMode := buildrun.InputModeNone
	var stdin []byte
	stdinValue := values["stdin"]
	stdinJSONValue := values["stdin_json"]
	if stdinValue.IsKnown() && !stdinValue.IsNull() {
		inputMode = buildrun.InputModeText
		stdin = []byte(stdinValue.AsString())
	}
	if stdinJSONValue.IsKnown() && !stdinJSONValue.IsNull() {
		if inputMode != buildrun.InputModeNone {
			return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid run block",
				`Run blocks may set only one of "stdin" or "stdin_json".`,
			))
		}
		jsonPayload, err := ctyjson.Marshal(stdinJSONValue, stdinJSONValue.Type())
		if err != nil {
			return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid run block",
				fmt.Sprintf(`Failed to encode "stdin_json" for %s: %s`, addr.String(), err),
			))
		}
		inputMode = buildrun.InputModeJSON
		stdin = jsonPayload
	}

	decodeMode := buildrun.DecodeModeNone
	if decode := values["decode"]; decode.IsKnown() && !decode.IsNull() {
		switch decode.AsString() {
		case "", string(buildrun.DecodeModeNone):
			decodeMode = buildrun.DecodeModeNone
		case string(buildrun.DecodeModeText):
			decodeMode = buildrun.DecodeModeText
		case string(buildrun.DecodeModeJSON):
			decodeMode = buildrun.DecodeModeJSON
		default:
			return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid run block",
				fmt.Sprintf(`Run block %s uses unsupported decode mode %q.`, addr.String(), decode.AsString()),
			))
		}
	}

	var allowedExitCodes []int
	if exitCodes := values["allowed_exit_codes"]; exitCodes.IsKnown() && !exitCodes.IsNull() {
		allowedExitCodes = make([]int, 0, exitCodes.LengthInt())
		for _, value := range exitCodes.AsValueSlice() {
			if !value.IsKnown() || value.IsNull() {
				return buildrun.Payload{}, true, nil
			}
			intValue, accuracy := value.AsBigFloat().Int64()
			if accuracy != big.Exact {
				return buildrun.Payload{}, false, tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
					tfdiags.Error,
					"Invalid run block",
					fmt.Sprintf(`Run block %s uses a non-integer allowed_exit_codes value.`, addr.String()),
				))
			}
			allowedExitCodes = append(allowedExitCodes, int(intValue))
		}
	}

	req, reqDiags := buildrun.BuildRequest(buildrun.RequestArgs{
		Argv:             argv,
		Cwd:              cwd,
		Env:              env,
		InputMode:        inputMode,
		Stdin:            stdin,
		Decode:           decodeMode,
		AllowedExitCodes: allowedExitCodes,
	}, tfdiags.SourceRangeFromHCL(run.DeclRange))
	if reqDiags.HasErrors() {
		return buildrun.Payload{}, false, reqDiags
	}

	return buildrun.Payload{
		Request:      *req,
		ValueAdapter: buildrun.ValueAdapterRawV1,
	}, false, nil
}
