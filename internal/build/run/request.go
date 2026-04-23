// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	"github.com/hashicorp/hcl/v2"

	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

const (
	requestVersion = "build-run-request-v1"
	resultVersion  = "build-run-result-v1"
)

type InputMode string

const (
	InputModeNone InputMode = "none"
	InputModeText InputMode = "text"
	InputModeJSON InputMode = "json"
)

type DecodeMode string

const (
	DecodeModeNone DecodeMode = "none"
	DecodeModeText DecodeMode = "text"
	DecodeModeJSON DecodeMode = "json"
)

type Request struct {
	Key     digest.Digest
	Payload []byte
}

type RequestArgs struct {
	Argv             []string
	Cwd              string
	Env              map[string]string
	InputMode        InputMode
	Stdin            []byte
	Decode           DecodeMode
	AllowedExitCodes []int
}

type EnvVar struct {
	Name  string
	Value string
}

type DecodedRequest struct {
	Argv             []string
	Cwd              string
	Env              []EnvVar
	InputMode        InputMode
	Stdin            []byte
	Decode           DecodeMode
	AllowedExitCodes []int
}

func (r DecodedRequest) Environment() []string {
	ret := make([]string, 0, len(r.Env))
	for _, env := range r.Env {
		ret = append(ret, env.Name+"="+env.Value)
	}
	return ret
}

type DecodedValue struct {
	Mode DecodeMode
	Text string
	JSON []byte
}

type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Decoded  *DecodedValue
}

type requestEnvelope struct {
	Version          string   `json:"version"`
	Argv             []string `json:"argv"`
	Cwd              string   `json:"cwd"`
	Env              []EnvVar `json:"env,omitempty"`
	InputMode        string   `json:"input_mode"`
	Stdin            []byte   `json:"stdin,omitempty"`
	Decode           string   `json:"decode"`
	AllowedExitCodes []int    `json:"allowed_exit_codes"`
}

type resultEnvelope struct {
	Version        string           `json:"version"`
	RequestVersion string           `json:"request_version"`
	ExitCode       int              `json:"exit_code"`
	Stdout         []byte           `json:"stdout,omitempty"`
	Stderr         []byte           `json:"stderr,omitempty"`
	Decoded        *decodedEnvelope `json:"decoded,omitempty"`
}

type decodedEnvelope struct {
	Mode string `json:"mode"`
	Text string `json:"text,omitempty"`
	JSON []byte `json:"json,omitempty"`
}

func BuildRequest(args RequestArgs, rng tfdiags.SourceRange) (*Request, tfdiags.Diagnostics) {
	if len(args.Argv) == 0 || args.Argv[0] == "" {
		return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			"Build run requests require a non-empty argv with an explicit executable path in argv[0].",
		))
	}
	executable := filepath.Clean(args.Argv[0])
	if !filepath.IsAbs(executable) {
		return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			"Build run requests require an explicit absolute executable path in argv[0].",
		))
	}

	cwd := filepath.Clean(args.Cwd)
	if args.Cwd == "" || !filepath.IsAbs(cwd) {
		return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			"Build run requests require an explicit absolute working directory.",
		))
	}

	inputMode := args.InputMode
	if inputMode == "" {
		inputMode = InputModeNone
	}
	if !validInputMode(inputMode) {
		return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			fmt.Sprintf("Build run request uses unsupported input mode %q.", inputMode),
		))
	}

	stdin, stdinDiags := normalizeInput(inputMode, args.Stdin, rng)
	if stdinDiags.HasErrors() {
		return nil, stdinDiags
	}

	decode := args.Decode
	if decode == "" {
		decode = DecodeModeNone
	}
	if !validDecodeMode(decode) {
		return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			fmt.Sprintf("Build run request uses unsupported decode mode %q.", decode),
		))
	}

	envelope := requestEnvelope{
		Version:          requestVersion,
		Argv:             slices.Clone(args.Argv),
		Cwd:              cwd,
		Env:              normalizeEnv(args.Env),
		InputMode:        string(inputMode),
		Stdin:            stdin,
		Decode:           string(decode),
		AllowedExitCodes: normalizeExitCodes(args.AllowedExitCodes),
	}
	envelope.Argv[0] = executable
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			fmt.Sprintf("Failed to encode build run request: %s", err),
		))
	}

	key, err := digest.FromValue(struct {
		Version string
		Payload []byte
	}{
		Version: requestVersion,
		Payload: payload,
	})
	if err != nil {
		return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			fmt.Sprintf("Failed to key build run request: %s", err),
		))
	}

	return &Request{
		Key:     key,
		Payload: payload,
	}, nil
}

func DecodeRequest(req Request, rng tfdiags.SourceRange) (DecodedRequest, tfdiags.Diagnostics) {
	envelope, diags := decodeRequestEnvelope(req, rng)
	if diags.HasErrors() {
		return DecodedRequest{}, diags
	}

	env := make([]EnvVar, 0, len(envelope.Env))
	seen := make(map[string]struct{}, len(envelope.Env))
	for _, entry := range envelope.Env {
		if entry.Name == "" {
			return DecodedRequest{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
				rng,
				"Invalid build run request",
				"Build run request contains an empty environment variable name.",
			))
		}
		if _, ok := seen[entry.Name]; ok {
			return DecodedRequest{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
				rng,
				"Invalid build run request",
				fmt.Sprintf("Build run request contains duplicate environment variable %q.", entry.Name),
			))
		}
		seen[entry.Name] = struct{}{}
		env = append(env, entry)
	}

	return DecodedRequest{
		Argv:             slices.Clone(envelope.Argv),
		Cwd:              envelope.Cwd,
		Env:              env,
		InputMode:        InputMode(envelope.InputMode),
		Stdin:            slices.Clone(envelope.Stdin),
		Decode:           DecodeMode(envelope.Decode),
		AllowedExitCodes: slices.Clone(envelope.AllowedExitCodes),
	}, nil
}

func EncodeResult(req Request, result Result) ([]byte, error) {
	envelope, diags := decodeRequestEnvelope(req, tfdiags.SourceRange{})
	if diags.HasErrors() {
		return nil, diags.Err()
	}

	var decoded *decodedEnvelope
	switch DecodeMode(envelope.Decode) {
	case DecodeModeNone:
		if result.Decoded != nil {
			return nil, fmt.Errorf("build run result carries decoded data for request with decode mode %q", envelope.Decode)
		}
	case DecodeModeText:
		if result.Decoded == nil || result.Decoded.Mode != DecodeModeText {
			return nil, fmt.Errorf("build run result must carry decoded text for request with decode mode %q", envelope.Decode)
		}
		decoded = &decodedEnvelope{
			Mode: string(DecodeModeText),
			Text: result.Decoded.Text,
		}
	case DecodeModeJSON:
		if result.Decoded == nil || result.Decoded.Mode != DecodeModeJSON {
			return nil, fmt.Errorf("build run result must carry decoded json for request with decode mode %q", envelope.Decode)
		}
		canonicalJSON, err := canonicalizeJSON(result.Decoded.JSON)
		if err != nil {
			return nil, err
		}
		decoded = &decodedEnvelope{
			Mode: string(DecodeModeJSON),
			JSON: canonicalJSON,
		}
	default:
		return nil, fmt.Errorf("build run request uses unsupported decode mode %q", envelope.Decode)
	}

	return json.Marshal(resultEnvelope{
		Version:        resultVersion,
		RequestVersion: envelope.Version,
		ExitCode:       result.ExitCode,
		Stdout:         slices.Clone(result.Stdout),
		Stderr:         slices.Clone(result.Stderr),
		Decoded:        decoded,
	})
}

func DecodeResult(req Request, payload []byte, rng tfdiags.SourceRange) (Result, tfdiags.Diagnostics) {
	envelope, diags := decodeRequestEnvelope(req, rng)
	if diags.HasErrors() {
		return Result{}, diags
	}

	var result resultEnvelope
	if err := json.Unmarshal(payload, &result); err != nil {
		return Result{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run result",
			fmt.Sprintf("Failed to decode build run result: %s", err),
		))
	}
	if result.RequestVersion != envelope.Version {
		return Result{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run result",
			fmt.Sprintf("Build run result uses unsupported request version %q.", result.RequestVersion),
		))
	}
	if result.Version != resultVersion {
		return Result{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run result",
			fmt.Sprintf("Build run result uses unsupported version %q.", result.Version),
		))
	}

	decoded, decodedDiags := decodeResultValue(result, DecodeMode(envelope.Decode), rng)
	if decodedDiags.HasErrors() {
		return Result{}, decodedDiags
	}

	return Result{
		ExitCode: result.ExitCode,
		Stdout:   slices.Clone(result.Stdout),
		Stderr:   slices.Clone(result.Stderr),
		Decoded:  decoded,
	}, nil
}

func decodeRequestEnvelope(req Request, rng tfdiags.SourceRange) (requestEnvelope, tfdiags.Diagnostics) {
	var envelope requestEnvelope
	if err := json.Unmarshal(req.Payload, &envelope); err != nil {
		return requestEnvelope{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			fmt.Sprintf("Failed to decode build run request: %s", err),
		))
	}
	if envelope.Version != requestVersion {
		return requestEnvelope{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			fmt.Sprintf("Build run request uses unsupported version %q.", envelope.Version),
		))
	}
	if len(envelope.Argv) == 0 || envelope.Argv[0] == "" {
		return requestEnvelope{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			"Build run request has an empty argv or executable path.",
		))
	}
	envelope.Argv[0] = filepath.Clean(envelope.Argv[0])
	if !filepath.IsAbs(envelope.Argv[0]) {
		return requestEnvelope{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			"Build run request has a non-absolute executable path.",
		))
	}
	if envelope.Cwd == "" || !filepath.IsAbs(envelope.Cwd) {
		return requestEnvelope{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			"Build run request has a non-absolute working directory.",
		))
	}
	if !validInputMode(InputMode(envelope.InputMode)) {
		return requestEnvelope{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			fmt.Sprintf("Build run request uses unsupported input mode %q.", envelope.InputMode),
		))
	}
	if !validDecodeMode(DecodeMode(envelope.Decode)) {
		return requestEnvelope{}, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			fmt.Sprintf("Build run request uses unsupported decode mode %q.", envelope.Decode),
		))
	}

	stdin, stdinDiags := normalizeInput(InputMode(envelope.InputMode), envelope.Stdin, rng)
	if stdinDiags.HasErrors() {
		return requestEnvelope{}, stdinDiags
	}
	envelope.Stdin = stdin
	envelope.AllowedExitCodes = normalizeExitCodes(envelope.AllowedExitCodes)

	return envelope, nil
}

func decodeResultValue(result resultEnvelope, decode DecodeMode, rng tfdiags.SourceRange) (*DecodedValue, tfdiags.Diagnostics) {
	switch decode {
	case DecodeModeNone:
		if result.Decoded != nil {
			return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
				rng,
				"Invalid build run result",
				"Build run result returned decoded data for a request without result decoding.",
			))
		}
		return nil, nil
	case DecodeModeText:
		if result.Decoded == nil || result.Decoded.Mode != string(DecodeModeText) {
			return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
				rng,
				"Invalid build run result",
				"Build run result did not return decoded text for a text-decoding request.",
			))
		}
		return &DecodedValue{
			Mode: DecodeModeText,
			Text: result.Decoded.Text,
		}, nil
	case DecodeModeJSON:
		if result.Decoded == nil || result.Decoded.Mode != string(DecodeModeJSON) {
			return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
				rng,
				"Invalid build run result",
				"Build run result did not return decoded JSON for a json-decoding request.",
			))
		}
		canonicalJSON, err := canonicalizeJSON(result.Decoded.JSON)
		if err != nil {
			return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
				rng,
				"Invalid build run result",
				fmt.Sprintf("Failed to decode JSON result payload: %s", err),
			))
		}
		return &DecodedValue{
			Mode: DecodeModeJSON,
			JSON: canonicalJSON,
		}, nil
	default:
		return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run result",
			fmt.Sprintf("Build run request uses unsupported decode mode %q.", decode),
		))
	}
}

func normalizeInput(mode InputMode, stdin []byte, rng tfdiags.SourceRange) ([]byte, tfdiags.Diagnostics) {
	switch mode {
	case InputModeNone:
		if len(stdin) != 0 {
			return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
				rng,
				"Invalid build run request",
				"Build run request cannot carry stdin when input mode is none.",
			))
		}
		return nil, nil
	case InputModeText:
		return slices.Clone(stdin), nil
	case InputModeJSON:
		canonicalJSON, err := canonicalizeJSON(stdin)
		if err != nil {
			return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
				rng,
				"Invalid build run request",
				fmt.Sprintf("Build run request has invalid JSON stdin: %s", err),
			))
		}
		return canonicalJSON, nil
	default:
		return nil, tfdiags.Diagnostics{}.Append(requestDiagnostic(
			rng,
			"Invalid build run request",
			fmt.Sprintf("Build run request uses unsupported input mode %q.", mode),
		))
	}
}

func normalizeEnv(env map[string]string) []EnvVar {
	if len(env) == 0 {
		return nil
	}

	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	ret := make([]EnvVar, 0, len(keys))
	for _, key := range keys {
		ret = append(ret, EnvVar{
			Name:  key,
			Value: env[key],
		})
	}
	return ret
}

func normalizeExitCodes(exitCodes []int) []int {
	if len(exitCodes) == 0 {
		return []int{0}
	}

	normalized := slices.Clone(exitCodes)
	slices.Sort(normalized)
	normalized = slices.Compact(normalized)
	return normalized
}

func validInputMode(mode InputMode) bool {
	switch mode {
	case InputModeNone, InputModeText, InputModeJSON:
		return true
	default:
		return false
	}
}

func validDecodeMode(mode DecodeMode) bool {
	switch mode {
	case DecodeModeNone, DecodeModeText, DecodeModeJSON:
		return true
	default:
		return false
	}
}

func canonicalizeJSON(src []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(src))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return json.Marshal(value)
}

func requestDiagnostic(rng tfdiags.SourceRange, summary, detail string) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  summary,
		Detail:   detail,
		Subject:  rng.ToHCL().Ptr(),
	}
}
