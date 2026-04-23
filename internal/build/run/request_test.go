// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opentofu/opentofu/internal/tfdiags"
)

func TestBuildRequestCanonicalizesJSONInputAndDefaultsExitCodes(t *testing.T) {
	req, diags := BuildRequest(RequestArgs{
		Argv:      []string{"/bin/echo", "hello"},
		Cwd:       filepath.Clean(t.TempDir()),
		Env:       map[string]string{"B": "two", "A": "one"},
		InputMode: InputModeJSON,
		Stdin:     []byte("{\"z\":1,\"a\":2}"),
		Decode:    DecodeModeJSON,
	}, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics: %s", diags.Err())
	}
	if req == nil {
		t.Fatal("expected request")
	}

	decoded, diags := DecodeRequest(*req, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected decode diagnostics: %s", diags.Err())
	}
	if got, want := string(decoded.Stdin), `{"a":2,"z":1}`; got != want {
		t.Fatalf("wrong canonical stdin: got %q want %q", got, want)
	}
	if got, want := decoded.AllowedExitCodes, []int{0}; !slicesEqual(got, want) {
		t.Fatalf("wrong default exit codes: got %#v want %#v", got, want)
	}
	if got, want := decoded.Environment(), []string{"A=one", "B=two"}; !slicesEqual(got, want) {
		t.Fatalf("wrong canonical environment: got %#v want %#v", got, want)
	}
}

func TestDecodeRequestRejectsUnsupportedVersion(t *testing.T) {
	req := mustBuildTestRequest(t)
	payload := cloneJSONMap(t, req.Payload)
	payload["version"] = "build-run-request-v99"
	req.Payload = mustJSONMarshal(t, payload)

	_, diags := DecodeRequest(req, tfdiags.SourceRange{})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, `unsupported version "build-run-request-v99"`) {
		t.Fatalf("missing version diagnostic in:\n%s", got)
	}
}

func TestDecodeResultRejectsUnsupportedVersions(t *testing.T) {
	req := mustBuildTestRequest(t)
	payload, err := EncodeResult(req, Result{
		ExitCode: 0,
		Stdout:   []byte("{\"ok\":true}\n"),
		Decoded: &DecodedValue{
			Mode: DecodeModeJSON,
			JSON: []byte("{\"ok\":true}\n"),
		},
	})
	if err != nil {
		t.Fatalf("unexpected encode error: %s", err)
	}

	t.Run("request version", func(t *testing.T) {
		bad := cloneJSONMap(t, payload)
		bad["request_version"] = "build-run-request-v99"

		_, diags := DecodeResult(req, mustJSONMarshal(t, bad), tfdiags.SourceRange{})
		if !diags.HasErrors() {
			t.Fatal("expected diagnostics")
		}
		if got := diags.Err().Error(); !strings.Contains(got, `unsupported request version "build-run-request-v99"`) {
			t.Fatalf("missing request version diagnostic in:\n%s", got)
		}
	})

	t.Run("result version", func(t *testing.T) {
		bad := cloneJSONMap(t, payload)
		bad["version"] = "build-run-result-v99"

		_, diags := DecodeResult(req, mustJSONMarshal(t, bad), tfdiags.SourceRange{})
		if !diags.HasErrors() {
			t.Fatal("expected diagnostics")
		}
		if got := diags.Err().Error(); !strings.Contains(got, `unsupported version "build-run-result-v99"`) {
			t.Fatalf("missing result version diagnostic in:\n%s", got)
		}
	})
}

func TestBuildRequestRejectsRelativeExecutablePath(t *testing.T) {
	_, diags := BuildRequest(RequestArgs{
		Argv: []string{"sh", "-c", "echo ok"},
		Cwd:  filepath.Clean(t.TempDir()),
	}, tfdiags.SourceRange{})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "absolute executable path") {
		t.Fatalf("missing executable-path diagnostic in:\n%s", got)
	}
}

func TestBuildRequestRejectsTrailingJSONInput(t *testing.T) {
	_, diags := BuildRequest(RequestArgs{
		Argv:      []string{"/bin/echo", "hello"},
		Cwd:       filepath.Clean(t.TempDir()),
		InputMode: InputModeJSON,
		Stdin:     []byte(`{"ok":true} trailing`),
	}, tfdiags.SourceRange{})
	if !diags.HasErrors() {
		t.Fatal("expected diagnostics")
	}
	if got := diags.Err().Error(); !strings.Contains(got, "invalid JSON stdin") {
		t.Fatalf("missing trailing-json diagnostic in:\n%s", got)
	}
}

func mustBuildTestRequest(t *testing.T) Request {
	t.Helper()

	req, diags := BuildRequest(RequestArgs{
		Argv:      []string{os.Args[0], "--helper"},
		Cwd:       filepath.Clean(t.TempDir()),
		InputMode: InputModeJSON,
		Stdin:     []byte("{\"hello\":\"world\"}"),
		Decode:    DecodeModeJSON,
	}, tfdiags.SourceRange{})
	if diags.HasErrors() {
		t.Fatalf("unexpected request diagnostics: %s", diags.Err())
	}
	if req == nil {
		t.Fatal("expected request")
	}
	return *req
}

func cloneJSONMap(t *testing.T, src []byte) map[string]any {
	t.Helper()

	var value map[string]any
	if err := json.Unmarshal(src, &value); err != nil {
		t.Fatalf("failed to decode JSON: %s", err)
	}
	return value
}

func mustJSONMarshal(t *testing.T, value any) []byte {
	t.Helper()

	ret, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("failed to encode JSON: %s", err)
	}
	return ret
}

func slicesEqual[T comparable](got, want []T) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
