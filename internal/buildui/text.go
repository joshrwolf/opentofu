// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package buildui

import (
	"fmt"
	"io"
	"sync"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/opentofu/opentofu/internal/chofu"
)

// TextObserver writes cargo-style colored output for non-interactive
// terminals (CI, piped output). Each line has a right-aligned status verb.
//
// Example output:
//
//	   Schemas  fetched (200ms)
//	  Expanded  66,412 items (2.1s)
//	     Graph  1,234 vertices, 5,678 edges (500ms)
//	      Walk  started
//	     Built  module.nginx.apko_build.this (2.3s)
//	    Failed  module.redis.apko_build.this: package not found
//
//	  Build complete: 48 resources, 12 data sources, 2 errors (45.0s)
type TextObserver struct {
	mu sync.Mutex
	w  io.Writer

	sVerb  lipgloss.Style
	sGreen lipgloss.Style
	sRed   lipgloss.Style
	sWarn  lipgloss.Style
	sCyan  lipgloss.Style
	sDim   lipgloss.Style
}

func NewTextObserver(w io.Writer, color bool) *TextObserver {
	t := &TextObserver{w: w}
	if color {
		t.sVerb = lipgloss.NewStyle().Bold(true)
		t.sGreen = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightGreen)
		t.sRed = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightRed)
		t.sWarn = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightYellow)
		t.sCyan = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightCyan)
		t.sDim = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	}
	return t
}

func (t *TextObserver) line(style lipgloss.Style, verb, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.w, "%s  %s\n", style.Render(fmt.Sprintf("%10s", verb)), detail)
}

func (t *TextObserver) Event(e chofu.BuildEvent) {
	switch {
	case e.PhaseStart != nil:
		// Silent — phase completion lines are sufficient.

	case e.PhaseComplete != nil:
		p := e.PhaseComplete
		dur := t.sDim.Render(formatDuration(p.Duration))
		switch p.Phase {
		case "schemas":
			t.line(t.sCyan, "Schemas", fmt.Sprintf("fetched %s", dur))
		case "expand":
			t.line(t.sCyan, "Expanded", fmt.Sprintf("%d items %s", p.ItemCount, dur))
		case "graph":
			t.line(t.sCyan, "Graph", fmt.Sprintf("%d vertices, %d edges %s", p.Vertices, p.Edges, dur))
		}

	case e.ResourceStart != nil:
		// Silent — print on complete only.

	case e.ResourceComplete != nil:
		r := e.ResourceComplete
		if r.Err != nil {
			t.line(t.sRed, "Failed", fmt.Sprintf("%s: %s", r.Addr, firstLineOf(r.Err.Error())))
		} else {
			t.line(t.sGreen, "Built", fmt.Sprintf("%s %s", r.Addr,
				t.sDim.Render(formatDuration(r.Duration))))
		}

	case e.BuildComplete != nil:
		bc := e.BuildComplete
		t.mu.Lock()
		defer t.mu.Unlock()
		msg := fmt.Sprintf("Build complete: %d resources, %d data sources, %d errors (%s)",
			bc.Resources, bc.DataSources, bc.Errors, bc.Duration.Round(time.Millisecond))
		if bc.DryRun {
			msg = fmt.Sprintf("Build complete (dry-run): %d resources, %d data sources (%s)",
				bc.Resources, bc.DataSources, bc.Duration.Round(time.Millisecond))
		}
		style := t.sGreen
		if bc.Errors > 0 {
			style = t.sRed
		}
		fmt.Fprintf(t.w, "\n%s\n", style.Render(msg))

		if len(bc.Diagnostics) > 0 {
			cd := ClassifyDiags(bc.Diagnostics)
			// Text mode shows all errors (no cap) — it's the "verbose" fallback.
			rendered := RenderDiags(cd, t.sRed, t.sWarn, t.sDim, 0)
			if rendered != "" {
				fmt.Fprintf(t.w, "\n%s\n", rendered)
			}
		}
	}
}
