// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package chofu

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/tfdiags"
)

// ExecResult holds the computed output values from running a chofu_exec
// resource. All file-like outputs are paths, not content.
type ExecResult struct {
	StdoutPath string
	StderrPath string
	ExitCode   int
	// Files maps declared output file names to their validated paths.
	Files map[string]string
}

// RunExec parses the evaluated config for a chofu_exec resource, runs
// the command, and returns the result. The caller (the engine) is
// responsible for config evaluation, content hashing, dry-run, hooks,
// and storing the result in the graph.
func RunExec(ctx context.Context, configVal cty.Value) (*ExecResult, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	// Parse command (required).
	cmdVal := configVal.GetAttr("command")
	if cmdVal.IsNull() || !cmdVal.IsKnown() {
		return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error,
			"command is required", "The command attribute must be a known, non-null list of strings."))
	}

	var command []string
	for it := cmdVal.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		if !elem.IsKnown() {
			return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error,
				"command contains unknown value",
				"All command elements must be known at exec time."))
		}
		if !elem.IsNull() {
			command = append(command, elem.AsString())
		}
	}
	if len(command) == 0 {
		return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error,
			"command is empty", "The command attribute must contain at least one element."))
	}

	// Parse env (optional). nil means inherit; non-nil means only declared vars.
	var env []string
	if envVal := configVal.GetAttr("env"); !envVal.IsNull() && envVal.IsKnown() {
		for it := envVal.ElementIterator(); it.Next(); {
			k, v := it.Element()
			if k.IsKnown() && v.IsKnown() && !v.IsNull() {
				env = append(env, k.AsString()+"="+v.AsString())
			}
		}
	}

	// Parse stdin (optional).
	var stdin string
	if v := configVal.GetAttr("stdin"); !v.IsNull() && v.IsKnown() {
		stdin = v.AsString()
	}

	// Parse working_dir (optional).
	var workingDir string
	if v := configVal.GetAttr("working_dir"); !v.IsNull() && v.IsKnown() {
		workingDir = v.AsString()
	}

	// Parse timeout (optional).
	var timeout time.Duration
	if v := configVal.GetAttr("timeout"); !v.IsNull() && v.IsKnown() {
		d, err := time.ParseDuration(v.AsString())
		if err != nil {
			return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error,
				"invalid timeout", fmt.Sprintf("Cannot parse timeout %q: %s", v.AsString(), err)))
		}
		timeout = d
	}

	// Parse output_files (optional).
	type outputFile struct{ Name, Path string }
	var outputFiles []outputFile
	if ofVal := configVal.GetAttr("output_files"); !ofVal.IsNull() && ofVal.IsKnown() {
		for it := ofVal.ElementIterator(); it.Next(); {
			_, elem := it.Element()
			name := elem.GetAttr("name")
			path := elem.GetAttr("path")
			if name.IsKnown() && !name.IsNull() && path.IsKnown() && !path.IsNull() {
				outputFiles = append(outputFiles, outputFile{name.AsString(), path.AsString()})
			}
		}
	}

	// Set up timeout context.
	execCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// Create temp files for stdout/stderr capture.
	stdoutFile, err := os.CreateTemp("", "chofu-stdout-*")
	if err != nil {
		return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error, "exec setup failed", err.Error()))
	}
	defer stdoutFile.Close()

	stderrFile, err := os.CreateTemp("", "chofu-stderr-*")
	if err != nil {
		os.Remove(stdoutFile.Name())
		return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error, "exec setup failed", err.Error()))
	}
	defer stderrFile.Close()

	// Run the command.
	cmd := exec.CommandContext(execCtx, command[0], command[1:]...)
	cmd.Dir = workingDir
	cmd.Env = env
	cmd.Stdout = stdoutFile
	cmd.Stderr = stderrFile
	if stdin != "" {
		cmd.Stdin = bytes.NewBufferString(stdin)
	}

	result := &ExecResult{
		StdoutPath: stdoutFile.Name(),
		StderrPath: stderrFile.Name(),
	}

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			f, _ := os.Open(stderrFile.Name())
			if f != nil {
				snippet, _ := io.ReadAll(io.LimitReader(f, 64*1024))
				f.Close()
				diags = diags.Append(tfdiags.Sourceless(tfdiags.Error,
					fmt.Sprintf("command exited with code %d", result.ExitCode),
					string(snippet)))
			} else {
				diags = diags.Append(tfdiags.Sourceless(tfdiags.Error,
					fmt.Sprintf("command exited with code %d", result.ExitCode), ""))
			}
			return result, diags
		}
		return nil, diags.Append(tfdiags.Sourceless(tfdiags.Error, "exec failed", err.Error()))
	}

	// Validate declared output files exist (REAPI contract).
	result.Files = make(map[string]string, len(outputFiles))
	for _, of := range outputFiles {
		if _, err := os.Stat(of.Path); err != nil {
			diags = diags.Append(tfdiags.Sourceless(tfdiags.Error,
				fmt.Sprintf("declared output file %q not found", of.Name),
				fmt.Sprintf("Expected file at %s after exec completed, but: %s", of.Path, err)))
			continue
		}
		result.Files[of.Name] = of.Path
	}

	return result, diags
}
