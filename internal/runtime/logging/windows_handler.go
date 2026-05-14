package logging

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/grafana/alloy/internal/runtime/logging/eventlog"
)

// eventLogState is shared by a windowsEventLogHandler and every handler
// derived from it via WithAttrs/WithGroup. It holds the underlying event
// log handle and synchronizes its lifecycle.
//
// mu is acquired for read by Handle/Enabled and for write by SetEventLog
// and Close. Because Close takes the write lock, it waits for any in-flight
// Handle calls to finish — so we can safely close the OS handle without
// the risk of another goroutine calling Info/Warning/Error on a just-closed
// handle.
type eventLogState struct {
	mu sync.RWMutex
	el eventlog.EventLog // nil when not open
}

// windowsEventLogHandler is a slog.Handler that writes logs to the Windows
// Event Log. All per-instance state (attrs, groups, replacer, level) is
// immutable after construction; WithAttrs/WithGroup return new handlers that
// share the same *eventLogState, so a Close on any derived handler observes
// every in-flight Handle.
type windowsEventLogHandler struct {
	state    *eventLogState
	level    slog.Leveler
	attrs    []slog.Attr
	groups   []string
	replacer func(groups []string, a slog.Attr) slog.Attr
}

var _ slog.Handler = (*windowsEventLogHandler)(nil)

// newWindowsEventLogHandler creates a handler with no underlying event log
// installed. Call SetEventLog to open dispatching; Close to release it.
func newWindowsEventLogHandler(level slog.Leveler, replacer func(groups []string, a slog.Attr) slog.Attr) *windowsEventLogHandler {
	return &windowsEventLogHandler{
		state:    &eventLogState{},
		level:    level,
		replacer: replacer,
	}
}

// SetEventLog installs the underlying Windows Event Log handle. Safe to
// call concurrently with Handle/Enabled on any handler sharing the same
// state.
func (h *windowsEventLogHandler) SetEventLog(el eventlog.EventLog) {
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	h.state.el = el
}

// IsOpen reports whether an event log handle is currently installed.
func (h *windowsEventLogHandler) IsOpen() bool {
	h.state.mu.RLock()
	defer h.state.mu.RUnlock()
	return h.state.el != nil
}

// Enabled reports whether the handler dispatches records at the given level.
// Returns false when the handle has been closed.
func (h *windowsEventLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	h.state.mu.RLock()
	defer h.state.mu.RUnlock()
	return h.state.el != nil && level >= h.level.Level()
}

// Handle dispatches the record to the Windows Event Log. Holds the state
// RLock for the entire call, so a concurrent Close blocks until this call
// returns — no risk of calling Info/Warning/Error on a closed handle.
func (h *windowsEventLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.state.mu.RLock()
	defer h.state.mu.RUnlock()
	if h.state.el == nil {
		return nil
	}
	el := h.state.el

	var buf strings.Builder
	if r.Message != "" {
		buf.WriteString(r.Message)
	}

	attrs := make([]slog.Attr, 0, len(h.attrs)+r.NumAttrs())
	attrs = append(attrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})

	for _, attr := range attrs {
		if h.replacer != nil {
			attr = h.replacer(h.groups, attr)
		}
		if attr.Key == "" {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString(" ")
		}
		buf.WriteString(attr.Key)
		buf.WriteString("=")
		buf.WriteString(attr.Value.String())
	}

	message := buf.String()
	if message == "" {
		return nil
	}

	switch r.Level {
	case slog.LevelDebug, slog.LevelInfo:
		return el.Info(1, message)
	case slog.LevelWarn:
		return el.Warning(1, message)
	case slog.LevelError:
		return el.Error(1, message)
	default:
		return el.Info(1, message)
	}
}

// WithAttrs returns a new handler with additional attributes, sharing the
// same eventLogState so it honors lifecycle changes.
func (h *windowsEventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	newAttrs = append(newAttrs, h.attrs...)
	newAttrs = append(newAttrs, attrs...)

	newGroups := make([]string, len(h.groups))
	copy(newGroups, h.groups)

	return &windowsEventLogHandler{
		state:    h.state,
		level:    h.level,
		attrs:    newAttrs,
		groups:   newGroups,
		replacer: h.replacer,
	}
}

// WithGroup returns a new handler with the additional group, sharing the
// same eventLogState.
func (h *windowsEventLogHandler) WithGroup(name string) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs))
	copy(newAttrs, h.attrs)

	newGroups := make([]string, 0, len(h.groups)+1)
	newGroups = append(newGroups, h.groups...)
	newGroups = append(newGroups, name)

	return &windowsEventLogHandler{
		state:    h.state,
		level:    h.level,
		attrs:    newAttrs,
		groups:   newGroups,
		replacer: h.replacer,
	}
}

// Close closes the underlying event log handle and disables dispatching.
// Takes the write lock, which waits for any in-flight Handle calls to
// finish — so the handle is never closed while another goroutine is using
// it. Idempotent.
func (h *windowsEventLogHandler) Close() error {
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	if h.state.el == nil {
		return nil
	}
	err := h.state.el.Close()
	h.state.el = nil
	return err
}
