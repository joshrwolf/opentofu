// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package hcl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func defaultExecEnvironment() map[string]string {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		env[name] = value
	}
	if len(env) == 0 {
		return nil
	}
	return env
}

func resolveExecutable(executable string, addr catalog.Addr, summary string) (string, tfdiags.Diagnostics) {
	if executable == "" {
		return "", tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			summary,
			"Compatibility targets require a non-empty executable path.",
		))
	}
	if filepath.IsAbs(executable) {
		return filepath.Clean(executable), nil
	}

	resolved, err := exec.LookPath(executable)
	if err != nil {
		return "", tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
			tfdiags.Error,
			summary,
			fmt.Sprintf("Failed to resolve executable %q for %s: %s", executable, addr.String(), err),
		))
	}
	if !filepath.IsAbs(resolved) {
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				summary,
				fmt.Sprintf("Failed to resolve executable path %q for %s: %s", executable, addr.String(), err),
			))
		}
	}
	return filepath.Clean(resolved), nil
}

func resolveWorkingDir(rootDir, moduleDir string, workingDir cty.Value, addr catalog.Addr, summary string) (string, tfdiags.Diagnostics) {
	cwd := rootDir
	if cwd == "" {
		cwd = moduleDir
	}
	cwd = filepath.Clean(cwd)
	if !filepath.IsAbs(cwd) {
		absCwd, err := filepath.Abs(cwd)
		if err != nil {
			return "", tfdiags.Diagnostics{}.Append(tfdiags.Sourceless(
				tfdiags.Error,
				summary,
				fmt.Sprintf("Failed to resolve working directory for %s: %s", addr.String(), err),
			))
		}
		cwd = absCwd
	}
	if workingDir.IsKnown() && !workingDir.IsNull() {
		resolved := workingDir.AsString()
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(moduleDir, resolved)
		}
		cwd = filepath.Clean(resolved)
	}
	return cwd, nil
}

func defaultLocalExecInterpreter() []string {
	if runtime.GOOS == "windows" {
		if comspec := os.Getenv("COMSPEC"); comspec != "" {
			return []string{comspec, "/C"}
		}
		return []string{"cmd", "/C"}
	}
	return []string{"/bin/sh", "-c"}
}

func bodyAttributes(body hcl.Body, skipNames ...string) []*hcl.Attribute {
	if body == nil {
		return nil
	}
	attrs, _ := body.JustAttributes()
	skip := make(map[string]struct{}, len(skipNames))
	for _, name := range skipNames {
		skip[name] = struct{}{}
	}
	names := make([]string, 0, len(attrs))
	for name := range attrs {
		if _, ok := skip[name]; ok {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)

	ret := make([]*hcl.Attribute, 0, len(names))
	for _, name := range names {
		ret = append(ret, attrs[name])
	}
	return ret
}
