// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// End-to-end txtar-driven tests. Each case in testdata/*.txtar declares a
// directory tree, a `build` selector list, and an optional `want.json` golden
// that captures the expected diagnostics, selected targets, outputs, and
// action counts. TOFU_UPDATE_GOLDEN=1 regenerates the goldens in place.
//
// This file shares Runner.Run (run.go) with the benchmarks but is otherwise
// independent of them.

package bench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/tools/txtar"
)

type txtarCase struct {
	Name      string
	Selectors []string
	WantJSON  string
	HasWant   bool
	archive   *txtar.Archive
}

func parseTxtar(path string) (*txtarCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	archive := txtar.Parse(data)

	tc := &txtarCase{
		Name:    strings.TrimSuffix(filepath.Base(path), ".txtar"),
		archive: archive,
	}
	for _, f := range archive.Files {
		switch f.Name {
		case "build":
			for line := range strings.SplitSeq(strings.TrimSpace(string(f.Data)), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					tc.Selectors = append(tc.Selectors, line)
				}
			}
		case "want.json":
			tc.WantJSON = strings.TrimSpace(string(f.Data))
			tc.HasWant = true
		}
	}
	if len(tc.Selectors) == 0 {
		tc.Selectors = []string{"**"}
	}
	return tc, nil
}

func (tc *txtarCase) extract(dir string) error {
	for _, f := range tc.archive.Files {
		if f.Name == "build" || f.Name == "want.json" {
			continue
		}
		path := filepath.Join(dir, f.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, f.Data, 0o644); err != nil {
			return err
		}
	}
	return rewriteModuleManifest(dir)
}

// rewriteModuleManifest rewrites any relative paths in the manifest to absolute
// paths rooted at the extracted directory, so the loader sees the modules where
// we actually placed them in this test's temp dir.
func rewriteModuleManifest(dir string) error {
	manifestPath := filepath.Join(dir, ".terraform", "modules", "modules.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}

	var manifest struct {
		Modules []struct {
			Key    string `json:"Key"`
			Source string `json:"Source"`
			Dir    string `json:"Dir"`
		} `json:"Modules"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	for i := range manifest.Modules {
		d := manifest.Modules[i].Dir
		if d == "." || d == "" {
			manifest.Modules[i].Dir = dir
		} else if !filepath.IsAbs(d) {
			manifest.Modules[i].Dir = filepath.Join(dir, d)
		}
	}
	out, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, out, 0o644)
}

type txtarResult struct {
	Errors   []string          `json:"errors,omitempty"`
	Warnings []string          `json:"warnings,omitempty"`
	Targets  int               `json:"targets"`
	Outputs  map[string]string `json:"outputs,omitempty"`
	Actions  txtarActions      `json:"actions"`
}

type txtarActions struct {
	Submitted int `json:"submitted"`
	Executed  int `json:"executed"`
	Cached    int `json:"cached"`
	Failed    int `json:"failed"`
}

func resultToTxtarJSON(result Result) (string, error) {
	submitted, executed, cached, failed := ActionCounts(result.Spans)
	tr := txtarResult{
		Errors:   result.Errors,
		Warnings: result.Warnings,
		Targets:  SpanAttrInt(result.Spans, "build.Execute", "build.selected_targets") + SpanAttrInt(result.Spans, "build.Execute", "build.selected_outputs"),
		Outputs:  result.Outputs,
		Actions: txtarActions{
			Submitted: submitted,
			Executed:  executed,
			Cached:    cached,
			Failed:    failed,
		},
	}
	if tr.Outputs == nil {
		tr.Outputs = map[string]string{}
	}
	data, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func updateTxtarGolden(path string, gotJSON string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	archive := txtar.Parse(data)

	found := false
	for i, f := range archive.Files {
		if f.Name == "want.json" {
			archive.Files[i].Data = []byte(gotJSON + "\n")
			found = true
			break
		}
	}
	if !found {
		archive.Files = append(archive.Files, txtar.File{
			Name: "want.json",
			Data: []byte(gotJSON + "\n"),
		})
	}
	return os.WriteFile(path, txtar.Format(archive), 0o644)
}

func compareTxtarJSON(got, want string) string {
	var gotVal, wantVal any
	if err := json.Unmarshal([]byte(got), &gotVal); err != nil {
		return "got is not valid JSON: " + err.Error()
	}
	if err := json.Unmarshal([]byte(want), &wantVal); err != nil {
		return "want is not valid JSON: " + err.Error()
	}
	return cmp.Diff(wantVal, gotVal)
}

func TestBuildTxtar(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no txtar test files in testdata/")
	}

	for _, path := range files {
		tc, err := parseTxtar(path)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		t.Run(tc.Name, func(t *testing.T) {
			runTxtarCase(t, path, tc)
		})
	}
}

func runTxtarCase(t *testing.T, path string, tc *txtarCase) {
	t.Helper()

	dir := t.TempDir()
	if err := tc.extract(dir); err != nil {
		t.Fatalf("extract: %v", err)
	}

	runner := NewRunner(Options{
		ProviderFactories: DefaultProviderFactories(),
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	result, err := runner.Run(ctx, dir, Scenario{
		Name:      tc.Name,
		Selectors: tc.Selectors,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	gotJSON, err := resultToTxtarJSON(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	if os.Getenv("TOFU_UPDATE_GOLDEN") != "" {
		if err := updateTxtarGolden(path, gotJSON); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		t.Logf("updated golden in %s", path)
		return
	}

	if !tc.HasWant {
		t.Fatalf("no want.json in %s; run with TOFU_UPDATE_GOLDEN=1 to generate", path)
	}

	if diff := compareTxtarJSON(gotJSON, tc.WantJSON); diff != "" {
		t.Errorf("result mismatch in %s (-want +got):\n%s\n\ngot:\n%s", path, diff, gotJSON)
	}
}
