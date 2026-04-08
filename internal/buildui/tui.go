// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package buildui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/opentofu/opentofu/internal/chofu"
)

// TUIObserver drives an interactive terminal UI via bubbletea. The display
// is a single live tree that grows as resources start and complete:
//
//	build["crane"]
//	  ✓ data.external.cache_lookup                     56ms
//	  this
//	    ✓ apko_build.this                              5.3s
//	    ▶ cosign_sign.signature                          3s
//	      │ signing with cosign...
//	test["crane"]
//	  ▶ bash_sandbox/data.apko_config.sandbox[0]         2s
//	──────────────────────────────────────────────────
//	[18/26 ✓ · 1 ✗] 22s
//
// Phase headers scroll up as permanent lines (tea.Println). The tree and
// progress counter are redrawn in place via View(). On completion, View()
// renders the final tree + summary as the last frame — no tea.Println
// duplication.
type TUIObserver struct {
	program *tea.Program
}

func NewTUIObserver(ctx context.Context) *TUIObserver {
	m := newModel()
	p := tea.NewProgram(m, tea.WithFPS(10), tea.WithContext(ctx))
	t := &TUIObserver{program: p}
	go func() { p.Run() }() //nolint:errcheck
	return t
}

func (t *TUIObserver) Wait()                    { t.program.Wait() }
func (t *TUIObserver) Event(e chofu.BuildEvent) { t.program.Send(e) }

// maxTreeWidth caps the rendering width so tree output stays readable
// on very wide terminals.
const maxTreeWidth = 100

// --- bubbletea model ---

type tuiModel struct {
	buildStart time.Time

	// Counters — maintained alongside the tree for O(1) access.
	built    int
	errored  int
	inflight int
	total    int // total resources started

	// The live tree — single source of truth for display.
	tree       *DisplayTree
	entryIndex map[string]*DisplayEntry // addr.String() → entry

	// Provider log ring buffers keyed by provider type name.
	providerLogs map[string]*logRing

	// Set on BuildComplete — View() renders the final output.
	buildComplete *chofu.BuildCompleteEvent

	width int
	tick  int // incremented every 500ms, drives the spinner
	done  bool
}

// spinner frames — braille dot pattern, standard in modern CLIs.
var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// logRing is a fixed-size ring buffer of recent log lines.
const logRingSize = 3

type logRing struct {
	lines [logRingSize]string
	head  int
	count int
}

func (r *logRing) push(line string) {
	r.lines[r.head] = line
	r.head = (r.head + 1) % logRingSize
	if r.count < logRingSize {
		r.count++
	}
}

func (r *logRing) recent() []string {
	if r.count == 0 {
		return nil
	}
	out := make([]string, r.count)
	start := (r.head - r.count + logRingSize) % logRingSize
	for i := range r.count {
		out[i] = r.lines[(start+i)%logRingSize]
	}
	return out
}

func newModel() *tuiModel {
	return &tuiModel{
		tree:         NewDisplayTree(),
		entryIndex:   make(map[string]*DisplayEntry),
		providerLogs: make(map[string]*logRing),
	}
}

func (m *tuiModel) Init() tea.Cmd { return nil }

type tickMsg struct{}

func tickCmd() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.KeyPressMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	case tickMsg:
		if !m.done {
			m.tick++
			return m, tickCmd()
		}
	case chofu.BuildEvent:
		return m.handleBuildEvent(msg)
	}
	return m, nil
}

func (m *tuiModel) handleBuildEvent(e chofu.BuildEvent) (tea.Model, tea.Cmd) {
	switch {
	case e.PhaseStart != nil:
		if m.buildStart.IsZero() {
			m.buildStart = time.Now()
			return m, tickCmd()
		}
	case e.PhaseComplete != nil:
		p := e.PhaseComplete
		dur := formatDuration(p.Duration)
		var line string
		switch p.Phase {
		case "schemas":
			line = fmt.Sprintf("%s fetched %s", sPhase.Render("Schemas"), sDim.Render(dur))
		case "expand":
			line = fmt.Sprintf("%s %d items %s", sPhase.Render("Expanded"), p.ItemCount, sDim.Render(dur))
		case "graph":
			line = fmt.Sprintf("%s %d vertices, %d edges %s", sPhase.Render("Graph"), p.Vertices, p.Edges, sDim.Render(dur))
		}
		if line != "" {
			return m, tea.Println(line)
		}

	case e.ResourceStart != nil:
		addr := e.ResourceStart.Addr
		entry := m.tree.InsertStart(addr, e.ResourceStart.Provider.Type, e.ResourceStart.Action)
		m.entryIndex[addr.String()] = entry
		m.inflight++
		m.total++

	case e.ProviderLog != nil:
		typeName := providerTypeFromSource(e.ProviderLog.Source)
		ring := m.providerLogs[typeName]
		if ring == nil {
			ring = &logRing{}
			m.providerLogs[typeName] = ring
		}
		ring.push(e.ProviderLog.Message)

	case e.ResourceComplete != nil:
		r := e.ResourceComplete
		if entry, ok := m.entryIndex[r.Addr.String()]; ok {
			entry.Done = true
			entry.Duration = r.Duration
			entry.Err = r.Err
			delete(m.entryIndex, r.Addr.String())
		}
		m.inflight--
		if r.Err != nil {
			m.errored++
		} else {
			m.built++
		}

	case e.BuildComplete != nil:
		m.done = true
		m.buildComplete = e.BuildComplete
		// View() will render the final tree + summary on the next frame.
		// Quit after that frame is drawn.
		return m, tea.Quit
	}

	return m, nil
}

// --- styles ---

var (
	sPhase   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightCyan)
	sBuilt   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightGreen)
	sFailed  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightRed)
	sWarn    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightYellow)
	sActive  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.BrightYellow)
	sDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sBold = lipgloss.NewStyle().Bold(true)
)

// renderTree appends the tree output (stripped to common prefix) to b.
func (m *tuiModel) renderTree(b *strings.Builder, tw int) {
	prefix := m.tree.CommonPrefix()
	if root := m.tree.Subtree(prefix); root != nil {
		b.WriteString(root.Render(m.renderOpts(tw)))
	}
}

// treeWidth returns the capped width for tree rendering.
func (m *tuiModel) treeWidth() int {
	w := m.width
	if w == 0 {
		w = 80
	}
	return min(w, maxTreeWidth)
}

// View renders the live tree + progress counter, or the final summary.
func (m *tuiModel) View() tea.View {
	if m.buildStart.IsZero() {
		return tea.NewView("")
	}

	if m.done {
		return tea.NewView(m.renderFinal())
	}

	var b strings.Builder
	tw := m.treeWidth()

	// Live tree.
	m.renderTree(&b, tw)

	// Separator + progress counter.
	b.WriteString(sDim.Render(strings.Repeat("─", tw)))
	b.WriteByte('\n')

	elapsed := time.Since(m.buildStart).Round(time.Second)
	progress := fmt.Sprintf("[%d/%d ✓", m.built, m.total)
	if m.inflight > 0 {
		frame := spinnerFrames[m.tick%len(spinnerFrames)]
		progress += fmt.Sprintf(" · %d %s", m.inflight, sActive.Render(frame))
	}
	if m.errored > 0 {
		progress += fmt.Sprintf(" · %s", sFailed.Render(fmt.Sprintf("%d ✗", m.errored)))
	}
	progress += fmt.Sprintf("] %s", elapsed)
	b.WriteString(sBold.Render(progress))

	return tea.NewView(b.String())
}

func (m *tuiModel) renderOpts(w int) RenderOpts {
	return RenderOpts{
		Width:             w,
		Indent:            "  ",
		StyleBuilt:        sBuilt.Render,
		StyleFailed:       sFailed.Render,
		StyleActive:       sActive.Render,
		StyleDim:          sDim.Render,
		// StyleHeading left nil — defaults to noop (plain text).
		Now:               time.Now(),
		SpinnerFrame:      spinnerFrames[m.tick%len(spinnerFrames)],
		CollapseThreshold: 50 * time.Millisecond,
		ProviderLogs: func(providerType string) []string {
			if ring, ok := m.providerLogs[providerType]; ok {
				return ring.recent()
			}
			return nil
		},
	}
}

// renderFinal produces the final frame: tree + stats + diagnostics.
func (m *tuiModel) renderFinal() string {
	tw := m.treeWidth()

	var b strings.Builder

	// Final tree — all entries are Done, no ▶ markers.
	m.renderTree(&b, tw)

	bc := m.buildComplete

	// Separator + summary.
	b.WriteString(sDim.Render(strings.Repeat("─", tw)))
	b.WriteByte('\n')

	if bc.DryRun {
		b.WriteString(sBold.Render("Build complete (dry-run)"))
	} else {
		b.WriteString(sBold.Render("Build complete"))
	}
	b.WriteByte('\n')

	fmt.Fprintf(&b, "\n  Vertices:     %d (%s resources, %s data sources)",
		bc.Resources+bc.DataSources,
		sBuilt.Render(fmt.Sprintf("%d", bc.Resources)),
		sBuilt.Render(fmt.Sprintf("%d", bc.DataSources)))
	fmt.Fprintf(&b, "\n  Providers:    %d configured", bc.Providers)
	if bc.Errors > 0 {
		fmt.Fprintf(&b, "\n  Errors:       %s", sFailed.Render(fmt.Sprintf("%d", bc.Errors)))
	}
	if bc.Warnings > 0 {
		fmt.Fprintf(&b, "\n  Warnings:     %d", bc.Warnings)
	}
	fmt.Fprintf(&b, "\n")
	fmt.Fprintf(&b, "\n  Schemas:      %s", sDim.Render(bc.SchemasDuration.Round(time.Millisecond).String()))
	fmt.Fprintf(&b, "\n  Expansion:    %s", sDim.Render(bc.ExpansionDuration.Round(time.Millisecond).String()))
	fmt.Fprintf(&b, "\n  Graph:        %s", sDim.Render(bc.GraphDuration.Round(time.Millisecond).String()))
	fmt.Fprintf(&b, "\n  Walk:         %s", sDim.Render(bc.WalkDuration.Round(time.Millisecond).String()))
	fmt.Fprintf(&b, "\n  Total:        %s", bc.Duration.Round(time.Millisecond))

	if len(bc.Diagnostics) > 0 {
		cd := ClassifyDiags(bc.Diagnostics)
		rendered := RenderDiags(cd, sFailed, sWarn, sDim, 10)
		if rendered != "" {
			fmt.Fprintf(&b, "\n\n%s", rendered)
		}
	}

	return b.String()
}

// providerTypeFromSource extracts a provider type name from the hclog
// logger source name. The source looks like:
//
//	"provider.terraform-provider-imagetest_v1.2.3"
//
// We extract "imagetest" — the part between "terraform-provider-" and
// the version suffix. This matches the Provider.Type field on ResourceStartEvent.
func providerTypeFromSource(source string) string {
	_, after, ok := strings.Cut(source, ".")
	if !ok {
		return source
	}
	after = strings.TrimPrefix(after, "terraform-provider-")
	if idx := strings.Index(after, "_v"); idx >= 0 {
		after = after[:idx]
	}
	return after
}
