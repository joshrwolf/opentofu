// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package buildui

import (
	"strings"

	"github.com/hashicorp/go-hclog"

	"github.com/opentofu/opentofu/internal/chofu"
	"github.com/opentofu/opentofu/internal/logging"
)

// ProviderLogSink implements hclog.SinkAdapter to capture provider log
// output in real time. It filters for logger names starting with "provider"
// and forwards matching entries as ProviderLogEvent to the BuildUI.
//
// This is registered on the global InterceptLogger for the duration of
// a build, so provider logs that arrive during ApplyResourceChange are
// immediately visible in the TUI's action bar.
//
// Usage:
//
//	sink := buildui.NewProviderLogSink(ui)
//	defer sink.Close()
//	// ... run build ...
type ProviderLogSink struct {
	ui       chofu.BuildUI
	minLevel hclog.Level
}

var _ hclog.SinkAdapter = (*ProviderLogSink)(nil)

// NewProviderLogSink creates a sink and registers it on the global logger.
// Call Close() to deregister when the build completes.
func NewProviderLogSink(ui chofu.BuildUI) *ProviderLogSink {
	s := &ProviderLogSink{
		ui:       ui,
		minLevel: hclog.Info, // Match the provider log level override
	}
	logging.RegisterSinkAdapter(s)
	return s
}

// Close deregisters the sink from the global logger.
func (s *ProviderLogSink) Close() {
	logging.DeregisterSinkAdapter(s)
}

// Accept receives every log entry that passes the global logger's level
// filter. We further filter to only provider-originated logs and forward
// them as structured BuildEvents.
func (s *ProviderLogSink) Accept(name string, level hclog.Level, msg string, args ...any) {
	// Only capture provider logs — not core, cloud, or other subsystems.
	if !strings.HasPrefix(name, "provider") {
		return
	}

	// Skip levels below our threshold to avoid flooding the UI.
	if level < s.minLevel {
		return
	}

	s.ui.Event(chofu.BuildEvent{
		ProviderLog: &chofu.ProviderLogEvent{
			Source:  name,
			Level:   level.String(),
			Message: msg,
			KVPairs: args,
		},
	})
}
