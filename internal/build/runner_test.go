// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package build

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/addrs"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/build/digest"
	"github.com/opentofu/opentofu/internal/build/engine"
	buildprovider "github.com/opentofu/opentofu/internal/build/provider"
	buildrun "github.com/opentofu/opentofu/internal/build/run"
	commandtesting "github.com/opentofu/opentofu/internal/command/testing"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

func TestRunnerRunsProviderFunctionAction(t *testing.T) {
	runner := NewRunner(RunnerConfig{
		ProviderInvoker: buildprovider.NewInvoker(buildprovider.InvokerConfig{
			OCIGetResolver: stubRunnerOCIGetResolver{
				resolved: "cgr.dev/example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		}),
	})

	req := buildprovider.FunctionRequest{
		Function: addrs.ProviderFunction{ProviderName: "oci", Function: "get"},
		Key:      digest.FromString("request"),
		Payload:  []byte(`{"ref":"cgr.dev/example/image:latest","pinned":false}`),
	}
	result, err := runner.Run(t.Context(), engine.Spec{
		Name: "test",
		Runner: engine.RunnerSpec{
			Kind: string(catalog.RunnerKindProviderFunction),
			Payload: buildprovider.FunctionPayload{
				Request: req,
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if result.OutputKey == (digest.Digest{}) {
		t.Fatal("expected non-zero output key")
	}
	if got, want := string(result.Payload), `{"version":"build-provider-function-result-v1","type":"string","string":"cgr.dev/example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`; got != want {
		t.Fatalf("wrong payload: got %q want %q", got, want)
	}
}

func TestRunnerRejectsUnsupportedProviderTarget(t *testing.T) {
	_, err := DefaultRunner().Run(t.Context(), engine.Spec{
		Name: "test",
		Source: engine.SourceRef{
			Filename: "main.tf",
			Start:    engine.SourcePos{Line: 1, Column: 1, Byte: 0},
			End:      engine.SourcePos{Line: 1, Column: 20, Byte: 19},
		},
		Runner: engine.RunnerSpec{
			Kind: string(catalog.RunnerKindProviderResource),
			Payload: buildprovider.TargetPayload{
				Binding:  buildprovider.Binding{Provider: addrs.NewDefaultProvider("test")},
				TypeName: "test_instance",
				Request:  buildprovider.TargetRequest{},
			},
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !strings.Contains(got, `Resource type "test_instance"`) {
		t.Fatalf("missing unsupported target error in: %s", got)
	}
}

func TestRunnerRunsProviderResourceAction(t *testing.T) {
	provider := commandtesting.NewProvider(nil)
	schema, _ := provider.Provider.GetProviderSchema(t.Context()).SchemaForResourceType(addrs.ManagedResourceMode, "test_resource")
	req, diags := buildprovider.BuildTargetRequest(
		catalog.RunnerKindProviderResource,
		addrs.NewDefaultProvider("test"),
		"test_resource",
		schema,
		cty.ObjectVal(map[string]cty.Value{
			"id":              cty.NullVal(cty.String),
			"value":           cty.StringVal("hello"),
			"interrupt_count": cty.NullVal(cty.Number),
		}),
		tfdiags.SourceRange{},
	)
	if diags.HasErrors() {
		t.Fatalf("unexpected request diagnostics: %s", diags.Err())
	}

	runner := NewRunner(RunnerConfig{
		ProviderSession: buildprovider.NewSession(buildprovider.SessionConfig{
			Factories: map[addrs.Provider]providers.Factory{
				addrs.NewDefaultProvider("test"): providers.FactoryFixed(provider.Provider),
			},
		}),
	})

	result, err := runner.Run(t.Context(), engine.Spec{
		Name: "test_resource.build",
		Runner: engine.RunnerSpec{
			Kind: string(catalog.RunnerKindProviderResource),
			Payload: buildprovider.TargetPayload{
				Binding: buildprovider.Binding{
					Provider:  addrs.NewDefaultProvider("test"),
					ConfigKey: digest.FromString("binding"),
				},
				TypeName: "test_resource",
				Request:  *req,
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if result.OutputKey == (digest.Digest{}) || len(result.Payload) == 0 {
		t.Fatal("expected non-empty result payload")
	}
}

func TestRunnerRunsBuiltinRunAction(t *testing.T) {
	t.Setenv("BUILD_RUN_AMBIENT", "should-not-leak")

	cwd := t.TempDir()
	resolvedCwd, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatalf("failed to resolve temp dir: %s", err)
	}
	req, diags := buildrun.BuildRequest(buildrun.RequestArgs{
		Argv: []string{
			os.Args[0],
			"-test.run=TestRunnerBuiltinRunHelperProcess",
			"--",
			"emit-json",
		},
		Cwd: cwd,
		Env: map[string]string{
			"BUILD_RUN_EXPLICIT":       "present",
			"GO_WANT_BUILD_RUN_HELPER": "1",
		},
		InputMode: buildrun.InputModeJSON,
		Stdin:     []byte("{\"z\":1,\"a\":2}"),
		Decode:    buildrun.DecodeModeJSON,
	}, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected request diagnostics: %s", diags.Err())
	}

	result, err := DefaultRunner().Run(t.Context(), engine.Spec{
		Name: "builtin.run.json",
		Runner: engine.RunnerSpec{
			Kind: string(catalog.RunnerKindBuiltinRun),
			Payload: buildrun.Payload{
				Request: *req,
			},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if result.OutputKey == (digest.Digest{}) {
		t.Fatal("expected non-zero output key")
	}

	decoded, diags := buildrun.DecodeResult(*req, result.Payload, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected decode diagnostics: %s", diags.Err())
	}
	if got, want := string(decoded.Stdout), "{\"ambient\":\"\",\"cwd\":\""+resolvedCwd+"\",\"explicit\":\"present\",\"stdin\":{\"a\":2,\"z\":1}}\n"; got != want {
		t.Fatalf("wrong raw stdout: got %q want %q", got, want)
	}
	if decoded.Decoded == nil || decoded.Decoded.Mode != buildrun.DecodeModeJSON {
		t.Fatalf("expected decoded json payload, got %#v", decoded.Decoded)
	}
	if got, want := string(decoded.Decoded.JSON), "{\"ambient\":\"\",\"cwd\":\""+resolvedCwd+"\",\"explicit\":\"present\",\"stdin\":{\"a\":2,\"z\":1}}"; got != want {
		t.Fatalf("wrong decoded json payload: got %q want %q", got, want)
	}
}

func TestRunnerRejectsBuiltinRunDisallowedExitCode(t *testing.T) {
	req, diags := buildrun.BuildRequest(buildrun.RequestArgs{
		Argv: []string{
			os.Args[0],
			"-test.run=TestRunnerBuiltinRunHelperProcess",
			"--",
			"exit-three",
		},
		Cwd:    t.TempDir(),
		Env:    map[string]string{"GO_WANT_BUILD_RUN_HELPER": "1"},
		Decode: buildrun.DecodeModeNone,
	}, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected request diagnostics: %s", diags.Err())
	}

	_, err := DefaultRunner().Run(t.Context(), engine.Spec{
		Name: "builtin.run.exit",
		Runner: engine.RunnerSpec{
			Kind: string(catalog.RunnerKindBuiltinRun),
			Payload: buildrun.Payload{
				Request: *req,
			},
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); !strings.Contains(got, "exited with code 3") {
		t.Fatalf("missing exit code error in:\n%s", got)
	}
}

type stubRunnerOCIGetResolver struct {
	resolved string
}

func (r stubRunnerOCIGetResolver) Resolve(_ context.Context, _ string) (string, error) {
	return r.resolved, nil
}

func TestRunnerBuiltinRunHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_BUILD_RUN_HELPER") != "1" {
		return
	}

	mode := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}

	switch mode {
	case "emit-json":
		stdin, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatalf("failed to read stdin: %s", err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("failed to get working directory: %s", err)
		}
		var decoded any
		if err := json.Unmarshal(stdin, &decoded); err != nil {
			t.Fatalf("failed to decode stdin json: %s", err)
		}
		payload := map[string]any{
			"ambient":  os.Getenv("BUILD_RUN_AMBIENT"),
			"cwd":      filepath.Clean(cwd),
			"explicit": os.Getenv("BUILD_RUN_EXPLICIT"),
			"stdin":    decoded,
		}
		if err := json.NewEncoder(os.Stdout).Encode(payload); err != nil {
			t.Fatalf("failed to encode stdout json: %s", err)
		}
		os.Exit(0)
	case "exit-three":
		_, _ = os.Stderr.WriteString("expected failure\n")
		os.Exit(3)
	default:
		t.Fatalf("unsupported helper mode %q", mode)
	}
}
