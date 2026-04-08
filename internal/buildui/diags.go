// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package buildui

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/opentofu/opentofu/internal/tfdiags"
)

// ClassifiedDiags separates build diagnostics into categories for rendering.
type ClassifiedDiags struct {
	// RootErrors are errors from actual provider/eval failures — the real problems.
	RootErrors []tfdiags.Diagnostic

	// CascadeCount is the number of errors that are transitive skips due to
	// upstream failures. These are noise — the user needs to fix the root
	// causes, not read "skipped X due to upstream error" 47 times.
	CascadeCount int

	// Warnings are unique warning messages with occurrence counts.
	Warnings []DedupedDiag
}

// DedupedDiag is a warning that appeared one or more times.
type DedupedDiag struct {
	Diag  tfdiags.Diagnostic
	Count int
}

// ClassifyDiags separates diagnostics into root errors, cascade skips,
// and deduplicated warnings. This is the core of the build-aware
// diagnostic rendering — it turns N raw diagnostics into something
// a human can reason about.
func ClassifyDiags(diags tfdiags.Diagnostics) ClassifiedDiags {
	var result ClassifiedDiags

	// Deduplicate warnings by (summary, detail) hash.
	type warnKey [sha256.Size]byte
	warnSeen := make(map[warnKey]int) // hash → index in result.Warnings

	for _, d := range diags {
		switch d.Severity() {
		case tfdiags.Error:
			desc := d.Description()
			if strings.HasPrefix(desc.Summary, "skipped ") && strings.Contains(desc.Summary, "due to upstream error") {
				result.CascadeCount++
			} else {
				result.RootErrors = append(result.RootErrors, d)
			}

		case tfdiags.Warning:
			desc := d.Description()
			h := sha256.Sum256([]byte(desc.Summary + "\x00" + desc.Detail))
			if idx, ok := warnSeen[h]; ok {
				result.Warnings[idx].Count++
			} else {
				warnSeen[h] = len(result.Warnings)
				result.Warnings = append(result.Warnings, DedupedDiag{Diag: d, Count: 1})
			}
		}
	}

	return result
}

// RenderDiags produces a styled string for classified diagnostics.
// This replaces the legacy ╷│╵ box format with something designed
// for build output: compact, deduplicated, cascade-aware.
//
// maxErrors caps how many root errors are shown in full. 0 means no cap.
func RenderDiags(cd ClassifiedDiags, sErr, sWarn, sDim lipgloss.Style, maxErrors int) string {
	var b strings.Builder

	// Root errors.
	if len(cd.RootErrors) > 0 {
		b.WriteString(sErr.Render(fmt.Sprintf("Errors (%d):", len(cd.RootErrors))))
		b.WriteByte('\n')

		shown := len(cd.RootErrors)
		if maxErrors > 0 && shown > maxErrors {
			shown = maxErrors
		}
		for i := 0; i < shown; i++ {
			renderOneDiag(&b, cd.RootErrors[i], sErr, sDim)
		}
		if shown < len(cd.RootErrors) {
			fmt.Fprintf(&b, "\n  %s\n",
				sDim.Render(fmt.Sprintf("… and %d more errors (use -ui=text for full output)", len(cd.RootErrors)-shown)))
		}
	}

	// Cascade summary.
	if cd.CascadeCount > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s\n",
			sDim.Render(fmt.Sprintf("%d downstream vertices skipped due to above errors.", cd.CascadeCount)))
	}

	// Deduplicated warnings.
	if len(cd.Warnings) > 0 {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(sWarn.Render(fmt.Sprintf("Warnings (%d):", totalWarnings(cd.Warnings))))
		b.WriteByte('\n')
		for _, w := range cd.Warnings {
			desc := w.Diag.Description()
			if w.Count > 1 {
				fmt.Fprintf(&b, "  %s %s %s\n",
					sWarn.Render("⚠"), desc.Summary,
					sDim.Render(fmt.Sprintf("(×%d)", w.Count)))
			} else {
				fmt.Fprintf(&b, "  %s %s\n", sWarn.Render("⚠"), desc.Summary)
			}
			if desc.Detail != "" {
				fmt.Fprintf(&b, "    %s\n", sDim.Render(firstLineOf(desc.Detail)))
			}
		}
	}

	return b.String()
}

func renderOneDiag(b *strings.Builder, d tfdiags.Diagnostic, sErr, sDim lipgloss.Style) {
	desc := d.Description()
	if desc.Address != "" {
		fmt.Fprintf(b, "\n  %s %s: %s", sErr.Render("✗"), desc.Address, desc.Summary)
	} else {
		fmt.Fprintf(b, "\n  %s %s", sErr.Render("✗"), desc.Summary)
	}
	if desc.Detail != "" {
		// Indent multi-line detail.
		for line := range strings.SplitSeq(desc.Detail, "\n") {
			fmt.Fprintf(b, "\n    %s", sDim.Render(line))
		}
	}
	b.WriteByte('\n')
}

func totalWarnings(ws []DedupedDiag) int {
	n := 0
	for _, w := range ws {
		n += w.Count
	}
	return n
}
