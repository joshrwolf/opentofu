// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package buildui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/opentofu/opentofu/internal/addrs"
)

// DisplayTree is a trie keyed by ModuleInstanceStep. Resources are inserted
// when they start executing and updated when they complete. The tree is the
// single source of truth for the TUI — View() renders it directly.
//
// Mutated only by bubbletea's Update goroutine; read only by View. No
// synchronization needed.
type DisplayTree struct {
	step     addrs.ModuleInstanceStep // identity; zero for root
	children []*DisplayTree
	index    map[string]int // stepKey → index into children
	entries  []*DisplayEntry
}

// DisplayEntry tracks a resource from start through completion.
type DisplayEntry struct {
	Resource addrs.ResourceInstance
	Provider string // provider type name for log correlation
	Action   string
	Started  time.Time
	Duration time.Duration // set on completion
	Err      error         // set on completion (nil = success)
	Done     bool
}

// isData returns true if this entry is a data source read.
func (e *DisplayEntry) isData() bool {
	return e.Resource.Resource.Mode == addrs.DataResourceMode
}

// NewDisplayTree creates an empty root node.
func NewDisplayTree() *DisplayTree {
	return &DisplayTree{index: make(map[string]int)}
}

// InsertStart adds an in-flight resource and returns the entry for later
// update. O(module depth) — typically 3-4 steps.
func (t *DisplayTree) InsertStart(addr addrs.AbsResourceInstance, provider, action string) *DisplayEntry {
	node := t.ensurePath(addr.Module)
	e := &DisplayEntry{
		Resource: addr.Resource,
		Provider: provider,
		Action:   action,
		Started:  time.Now(),
	}
	node.entries = append(node.entries, e)
	return e
}

// ensurePath walks/creates the trie path for a module instance.
func (t *DisplayTree) ensurePath(mod addrs.ModuleInstance) *DisplayTree {
	node := t
	for _, step := range mod {
		key := StepLabel(step)
		idx, ok := node.index[key]
		if !ok {
			child := &DisplayTree{
				step:  step,
				index: make(map[string]int),
			}
			idx = len(node.children)
			node.children = append(node.children, child)
			node.index[key] = idx
		}
		node = node.children[idx]
	}
	return node
}

// StepLabel renders a ModuleInstanceStep as "name" or "name[key]".
// Used both as map key and display label.
func StepLabel(s addrs.ModuleInstanceStep) string {
	if s.InstanceKey == addrs.NoKey {
		return s.Name
	}
	return s.Name + s.InstanceKey.String()
}

// CommonPrefix returns the longest single-child chain from the root.
func (t *DisplayTree) CommonPrefix() []addrs.ModuleInstanceStep {
	var prefix []addrs.ModuleInstanceStep
	node := t
	for len(node.entries) == 0 && len(node.children) == 1 {
		child := node.children[0]
		prefix = append(prefix, child.step)
		node = child
	}
	return prefix
}

// Subtree returns the node at the given path, or nil.
func (t *DisplayTree) Subtree(path []addrs.ModuleInstanceStep) *DisplayTree {
	node := t
	for _, step := range path {
		idx, ok := node.index[StepLabel(step)]
		if !ok {
			return nil
		}
		node = node.children[idx]
	}
	return node
}

// RenderOpts controls tree rendering.
type RenderOpts struct {
	Width  int
	Indent string

	// Style callbacks — variadic to match lipgloss.Style.Render.
	StyleBuilt   func(...string) string
	StyleFailed  func(...string) string
	StyleActive  func(...string) string
	StyleDim     func(...string) string
	StyleHeading func(...string) string

	// Now is the current time, used for in-flight elapsed display.
	Now time.Time

	// ProviderLogs returns recent log lines for a provider type.
	ProviderLogs func(providerType string) []string

	// SpinnerFrame is the current spinner character for in-flight resources.
	SpinnerFrame string

	// CollapseThreshold: completed resources faster than this are collapsed
	// into a summary line per module. 0 disables collapsing.
	CollapseThreshold time.Duration

	// indentCache is precomputed indent strings, set by Render().
	indentCache []string
}

func (o *RenderOpts) defaults() {
	if o.Indent == "" {
		o.Indent = "  "
	}
	if o.Width == 0 {
		o.Width = 80
	}
	noop := func(s ...string) string { return strings.Join(s, "") }
	if o.StyleBuilt == nil {
		o.StyleBuilt = noop
	}
	if o.StyleFailed == nil {
		o.StyleFailed = noop
	}
	if o.StyleActive == nil {
		o.StyleActive = noop
	}
	if o.StyleDim == nil {
		o.StyleDim = noop
	}
	if o.StyleHeading == nil {
		o.StyleHeading = noop
	}
	if o.SpinnerFrame == "" {
		o.SpinnerFrame = "▶"
	}
}

// maxIndentDepth is the precomputed indent cache size. Deeper nesting
// falls back to strings.Repeat.
const maxIndentDepth = 16

// Render produces the tree display.
func (t *DisplayTree) Render(opts RenderOpts) string {
	opts.defaults()
	// Precompute indent strings to avoid per-entry allocation.
	opts.indentCache = make([]string, maxIndentDepth)
	for i := range maxIndentDepth {
		opts.indentCache[i] = strings.Repeat(opts.Indent, i)
	}
	var b strings.Builder
	t.renderNode(&b, &opts, 0, "")
	return b.String()
}

// renderNode renders a tree node: optional heading, entries, then children.
// The heading (if non-empty) gets the trivial collapse count appended.
func (t *DisplayTree) renderNode(b *strings.Builder, opts *RenderOpts, depth int, heading string) {
	// Partition entries.
	var inflight, completed, failed []*DisplayEntry
	for _, e := range t.entries {
		if !e.Done {
			inflight = append(inflight, e)
		} else if e.Err != nil {
			failed = append(failed, e)
		} else {
			completed = append(completed, e)
		}
	}

	// Entries render in insertion order (completion order from the walker).
	// No per-frame sort — O(n log n) per 150ms tick is unacceptable at 66k scale.

	// Split trivial entries.
	var significant, trivial []*DisplayEntry
	if opts.CollapseThreshold > 0 {
		for _, e := range completed {
			if e.Duration < opts.CollapseThreshold {
				trivial = append(trivial, e)
			} else {
				significant = append(significant, e)
			}
		}
	} else {
		significant = completed
	}

	// Render heading with optional trivial count.
	if heading != "" {
		indent := indentAt(opts, max(depth-1, 0))
		if len(trivial) > 0 {
			fmt.Fprintf(b, "%s%s %s\n", indent,
				opts.StyleHeading(heading),
				opts.StyleDim(fmt.Sprintf("(+ %d <%s)", len(trivial), formatDuration(opts.CollapseThreshold))))
		} else {
			fmt.Fprintf(b, "%s%s\n", indent, opts.StyleHeading(heading))
		}
	}

	// Failed first, then significant completed, then in-flight.
	for _, e := range failed {
		renderEntry(b, opts, depth, e)
	}
	for _, e := range significant {
		renderEntry(b, opts, depth, e)
	}
	for _, e := range inflight {
		renderEntry(b, opts, depth, e)
	}

	// Child modules.
	for _, child := range t.children {
		label, leaf := compressPath(child)
		leaf.renderNode(b, opts, depth+1, label)
	}
}

func compressPath(node *DisplayTree) (string, *DisplayTree) {
	parts := []string{StepLabel(node.step)}
	for len(node.entries) == 0 && len(node.children) == 1 {
		node = node.children[0]
		parts = append(parts, StepLabel(node.step))
	}
	return strings.Join(parts, "/"), node
}

func indentAt(opts *RenderOpts, depth int) string {
	if depth < len(opts.indentCache) {
		return opts.indentCache[depth]
	}
	return strings.Repeat(opts.Indent, depth)
}

func renderEntry(b *strings.Builder, opts *RenderOpts, depth int, e *DisplayEntry) {
	indent := indentAt(opts, depth)
	label := e.Resource.String()

	if !e.Done {
		// In-flight: spinner with elapsed time.
		elapsed := opts.Now.Sub(e.Started).Round(time.Second)
		fmt.Fprintf(b, "%s%s %s %s\n", indent,
			opts.StyleActive(opts.SpinnerFrame), label,
			opts.StyleDim(elapsed.String()))

		// Provider log lines under in-flight resource.
		if opts.ProviderLogs != nil {
			for _, line := range opts.ProviderLogs(e.Provider) {
				line = cleanProviderLog(line)
				maxLen := opts.Width - (len(indent) + 6)
				if maxLen > 0 && len(line) > maxLen {
					line = line[:maxLen-3] + "..."
				}
				fmt.Fprintf(b, "%s  %s\n", indent, opts.StyleDim("│ "+line))
			}
		}
		return
	}

	if e.Err != nil {
		// Truncate error to terminal width.
		short := firstLineOf(e.Err.Error())
		line := fmt.Sprintf("%s%s %s: %s", indent, opts.StyleFailed("✗"), label, opts.StyleDim(short))
		if vl := visualLen(line); vl > opts.Width {
			// Re-truncate the dim portion to fit.
			over := vl - opts.Width
			if len(short) > over+3 {
				short = short[:len(short)-over-3] + "..."
			}
			line = fmt.Sprintf("%s%s %s: %s", indent, opts.StyleFailed("✗"), label, opts.StyleDim(short))
		}
		b.WriteString(line)
		b.WriteByte('\n')
		return
	}

	// Completed: icon depends on resource mode.
	dur := formatDuration(e.Duration)
	if e.isData() {
		// Data sources are reads — dimmer, less visually prominent.
		fmt.Fprintf(b, "%s%s %s %s\n", indent, opts.StyleDim("·"), opts.StyleDim(label), opts.StyleDim(dur))
	} else {
		fmt.Fprintf(b, "%s%s %s %s\n", indent, opts.StyleBuilt("✓"), label, opts.StyleDim(dur))
	}
}

func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%.0fs", d.Seconds())
	}
}

func firstLineOf(s string) string {
	if first, _, ok := strings.Cut(s, "\n"); ok {
		return first
	}
	return s
}

// cleanProviderLog strips timestamps and log level prefixes from provider
// log messages. Providers often emit lines like:
//
//	"2026/04/08 16:38:39 INFO finished bundling artifacts target=..."
//
// We strip the leading timestamp and level since the TUI context already
// makes timing and severity clear.
var logTimestampRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} (?:INFO |WARN |DEBUG |ERROR )?`)

func cleanProviderLog(msg string) string {
	return logTimestampRe.ReplaceAllString(msg, "")
}

// visualLen returns the visible width of s, stripping ANSI escapes.
func visualLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		if r == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
			continue
		}
		n++
	}
	return n
}
