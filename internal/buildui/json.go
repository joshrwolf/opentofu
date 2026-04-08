// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package buildui

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/opentofu/opentofu/internal/chofu"
)

// JSONObserver writes one JSON object per line (JSONL/ndjson). Every line
// includes a human-readable "message" field so the output is useful both
// parsed programmatically and piped through `jq -r .message`.
type JSONObserver struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func NewJSONObserver(w io.Writer) *JSONObserver {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &JSONObserver{enc: enc}
}

type jsonLine struct {
	Timestamp string `json:"ts"`
	Type      string `json:"type"`
	Message   string `json:"message"`

	// Phase fields.
	Phase     string `json:"phase,omitempty"`
	ItemCount *int   `json:"item_count,omitempty"`
	Vertices  *int   `json:"vertices,omitempty"`
	Edges     *int   `json:"edges,omitempty"`

	// Resource fields.
	Resource    string `json:"resource,omitempty"`
	Action      string `json:"action,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	Error       string `json:"error,omitempty"`

	// Duration and summary.
	DurationMS *int64 `json:"duration_ms,omitempty"`
	DryRun     *bool  `json:"dry_run,omitempty"`
	Resources  *int64 `json:"resources,omitempty"`
	DataSources *int64 `json:"data_sources,omitempty"`
	Errors     *int64 `json:"errors,omitempty"`
}

func (j *JSONObserver) emit(l jsonLine) {
	l.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.enc.Encode(l) //nolint:errcheck
}

func durationMS(d time.Duration) *int64 { ms := d.Milliseconds(); return &ms }

func (j *JSONObserver) Event(e chofu.BuildEvent) {
	switch {
	case e.PhaseStart != nil:
		j.emit(jsonLine{
			Type:    "phase_start",
			Phase:   e.PhaseStart.Phase,
			Message: fmt.Sprintf("Phase: %s...", e.PhaseStart.Phase),
		})

	case e.PhaseComplete != nil:
		p := e.PhaseComplete
		l := jsonLine{
			Type:       "phase_complete",
			Phase:      p.Phase,
			DurationMS: durationMS(p.Duration),
			Message:    fmt.Sprintf("Phase: %s (%s)", p.Phase, p.Duration.Round(time.Millisecond)),
		}
		if p.ItemCount > 0 {
			l.ItemCount = &p.ItemCount
		}
		if p.Vertices > 0 {
			l.Vertices = &p.Vertices
		}
		if p.Edges > 0 {
			l.Edges = &p.Edges
		}
		j.emit(l)

	case e.ResourceStart != nil:
		r := e.ResourceStart
		j.emit(jsonLine{
			Type:     "resource_start",
			Resource: r.Addr.String(),
			Action:   r.Action,
			Message:  fmt.Sprintf("%s: %s...", r.Addr, r.Action),
		})

	case e.ProviderLog != nil:
		p := e.ProviderLog
		j.emit(jsonLine{
			Type:    "provider_log",
			Message: cleanProviderLog(p.Message),
			Phase:   p.Source, // reuse phase field for source
			Action:  p.Level,  // reuse action field for level
		})

	case e.ResourceComplete != nil:
		r := e.ResourceComplete
		l := jsonLine{
			Type:        "resource_complete",
			Resource:    r.Addr.String(),
			Action:      r.Action,
			ContentHash: chofu.ContentHashHex(r.ContentHash),
			DurationMS:  durationMS(r.Duration),
		}
		if r.Err != nil {
			l.Error = r.Err.Error()
			l.Message = fmt.Sprintf("%s: failed: %s", r.Addr, r.Err)
		} else {
			l.Message = fmt.Sprintf("%s: %s (%s)", r.Addr, r.Action, r.Duration.Round(time.Millisecond))
		}
		j.emit(l)

	case e.BuildComplete != nil:
		bc := e.BuildComplete
		j.emit(jsonLine{
			Type:        "build_complete",
			DurationMS:  durationMS(bc.Duration),
			DryRun:      &bc.DryRun,
			Resources:   &bc.Resources,
			DataSources: &bc.DataSources,
			Errors:      &bc.Errors,
			Message: fmt.Sprintf("Build complete: %d resources, %d data sources, %d errors (%s)",
				bc.Resources, bc.DataSources, bc.Errors, bc.Duration.Round(time.Millisecond)),
		})

		// Emit classified diagnostics as separate lines for easy filtering.
		if len(bc.Diagnostics) > 0 {
			cd := ClassifyDiags(bc.Diagnostics)
			for _, d := range cd.RootErrors {
				desc := d.Description()
				j.emit(jsonLine{
					Type:    "diagnostic",
					Error:   desc.Summary,
					Message: desc.Summary,
				})
			}
			if cd.CascadeCount > 0 {
				j.emit(jsonLine{
					Type:    "diagnostic_cascade",
					Message: fmt.Sprintf("%d downstream vertices skipped due to upstream errors", cd.CascadeCount),
				})
			}
			for _, w := range cd.Warnings {
				desc := w.Diag.Description()
				l := jsonLine{
					Type:    "diagnostic_warning",
					Message: desc.Summary,
				}
				if w.Count > 1 {
					count := int64(w.Count)
					l.Errors = &count // reuse field for count
				}
				j.emit(l)
			}
		}
	}
}
