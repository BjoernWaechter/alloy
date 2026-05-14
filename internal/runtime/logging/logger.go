package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/common/model"

	"github.com/grafana/alloy/internal/component/common/loki"
	"github.com/grafana/alloy/internal/runtime/logging/eventlog"
	"github.com/grafana/alloy/internal/slogadapter"
	"github.com/grafana/loki/pkg/push"
)

type EnabledAware interface {
	Enabled(context.Context, slog.Level) bool
}

// Logger is the logging subsystem of Alloy. It supports being dynamically
// updated at runtime.
type Logger struct {
	inner io.Writer // Writer passed to New.

	bufferMut    sync.RWMutex
	buffer       []*bufferedItem // Store logs before correctly determine the log format
	hasLogFormat bool            // Confirmation whether log format has been determined

	level        *slog.LevelVar       // Current configured level.
	format       *formatVar           // Current configured format.
	writer       *writerVar           // Current configured multiwriter (inner + write_to).
	bytesHandler *bytesHandler        // slog.Handler that formats records to bytes and writes through writer.
	deferredSlog *deferredSlogHandler // Buffers slog output until config is loaded, then delegates to handler.

	windowsEventLogHandler *windowsEventLogHandler // When destination is windows_event_log (Windows only).
	eventLogOpener         eventlog.EventLogOpener // Opens the Windows event log; set in NewDeferred, overridable via SetEventLogOpener for tests.

	// handler is the slog.Handler dispatched to by both the gokit and slog
	// paths. It is a stable fanoutHandler{windowsEventLogHandler, bytesHandler}
	// set once in NewDeferred and never reassigned. The two children carry
	// their own enable/disable state with drain-on-disable semantics so
	// Update can flip destinations without losing any in-flight record.
	handler slog.Handler
}

var _ EnabledAware = (*Logger)(nil)

// Enabled implements EnabledAware interface.
func (l *Logger) Enabled(ctx context.Context, level slog.Level) bool {
	return l.handler.Enabled(ctx, level)
}

// New creates a New logger with the default log level and format.
func New(w io.Writer, o Options) (*Logger, error) {
	l, err := NewDeferred(w)
	if err != nil {
		return nil, err
	}
	if err = l.Update(o); err != nil {
		return nil, err
	}

	return l, nil
}

// NewNop returns a logger that does nothing
func NewNop() *Logger {
	l, _ := NewDeferred(io.Discard)
	return l
}

// NewSlogNop returns a slog logger backed by a handler that never logs.
func NewSlogNop() *slog.Logger {
	return slog.New(nopSlogHandler{})
}

// NewDeferred creates a new logger with the default log level and format.
// The logger is not updated during initialization.
func NewDeferred(w io.Writer) (*Logger, error) {
	var (
		leveler slog.LevelVar
		format  formatVar
	)
	// innerWriter is stable for the life of the Logger; destinations that
	// want to suppress it (windows_event_log) flip writerVar.suppressInner
	// instead of swapping the writer.
	writer := &writerVar{innerWriter: w}

	l := &Logger{
		inner: w,

		buffer:       []*bufferedItem{},
		hasLogFormat: false,

		level:  &leveler,
		format: &format,
		writer: writer,
		bytesHandler: &bytesHandler{
			w:         writer,
			leveler:   &leveler,
			formatter: &format,
			replacer:  replace,
		},
		eventLogOpener: eventlog.GetEventLogOpener(),
	}
	// The handler structure is stable for the life of the Logger; Update
	// flips state on the children rather than reassigning l.handler.
	l.windowsEventLogHandler = newWindowsEventLogHandler(l.level, replace)
	l.handler = fanoutHandler{a: l.windowsEventLogHandler, b: l.bytesHandler}
	l.deferredSlog = newDeferredHandler(l)

	return l, nil
}

// Handler returns a [slog.Handler]. The returned Handler remains valid if l is
// updated.
func (l *Logger) Handler() slog.Handler { return l.deferredSlog }

// Slog returns a [slog.Logger]. The returned logger remains valid if l is
// updated.
func (l *Logger) Slog() *slog.Logger { return slog.New(l.deferredSlog) }

type nopSlogHandler struct{}

func (nopSlogHandler) Enabled(context.Context, slog.Level) bool { return false }

func (nopSlogHandler) Handle(context.Context, slog.Record) error { return nil }

func (nopSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return nopSlogHandler{} }

func (nopSlogHandler) WithGroup(string) slog.Handler { return nopSlogHandler{} }

// Update re-configures the options used for the logger.
func (l *Logger) Update(o Options) error {
	switch o.Format {
	case FormatLogfmt, FormatJSON:
	default:
		return fmt.Errorf("unrecognized log format %q", o.Format)
	}

	l.bufferMut.Lock()
	l.level.Set(slogLevel(o.Level).Level())
	l.format.Set(o.Format)
	if err := l.applyDestination(o.Destination); err != nil {
		l.bufferMut.Unlock()
		return err
	}
	if len(o.WriteTo) > 0 {
		l.writer.SetLokiWriter(&lokiWriter{o.WriteTo})
	}
	l.bufferMut.Unlock()

	// Rebuild deferred slog handlers outside bufferMut to avoid a deadlock
	// with concurrent Handle() calls (they hold a child handler's RLock
	// while waiting for bufferMut via addRecord).
	if l.deferredSlog != nil {
		l.deferredSlog.buildHandlers(nil)
	}
	l.flushBuffer()
	return nil
}

// applyDestination toggles state on the (stable) child handlers to match
// the new destination. Must be called with l.bufferMut held by the caller.
//
// The architecture is loss-free by construction:
//   - l.handler is a stable fanout over windowsEventLogHandler and
//     bytesHandler — Update never reassigns it.
//   - Each child carries its own state behind a sync.RWMutex with
//     drain-on-disable semantics. Handle/Write take RLock; the mutators
//     (Close, SetSuppressInner) take Lock so they wait for in-flight calls
//     to finish before flipping state.
//
// Every in-flight Log dispatch therefore completes against the
// configuration that was active when it started, and never observes a
// closed handle or a half-changed writer.
//
// Adding a future "none" destination: extend the suppress condition to
// cover it — one line.
func (l *Logger) applyDestination(d LogDestination) error {
	willHaveEventLog := d == LogDestinationWindowsEventLog

	if willHaveEventLog && !l.windowsEventLogHandler.IsOpen() {
		el, err := l.eventLogOpener("Alloy")
		if err != nil {
			return fmt.Errorf("failed to open Windows Event Log: %w", err)
		}
		l.windowsEventLogHandler.SetEventLog(el)
	}

	if willHaveEventLog {
		// Order: open event log first (above) so by the time we suppress
		// inner, the event log path is already accepting records.
		l.writer.SetSuppressInner(true)
	} else {
		// Order: unsuppress first so in-flight Logs that were in event_log
		// mode and now race past SetSuppressInner deliver to stderr; then
		// close the event log (which drains any in-flight event-log
		// dispatches before releasing the OS handle).
		l.writer.SetSuppressInner(false)
		if l.windowsEventLogHandler.IsOpen() {
			_ = l.windowsEventLogHandler.Close()
		}
	}
	return nil
}

// flushBuffer drains and replays any logs that were buffered before
// Update finished resolving the log format. It must be called AFTER the
// new destination has been applied (so replayed records go through the
// right handler) and AFTER l.deferredSlog.buildHandlers has run (so
// child handlers point at the new l.handler).
//
// Holds bufferMut for the entire replay so concurrent Log() calls block
// until the buffer is drained, preserving the order guarantee that
// buffered logs appear before newly-arriving ones.
func (l *Logger) flushBuffer() {
	l.bufferMut.Lock()
	defer l.bufferMut.Unlock()
	l.hasLogFormat = true
	buffer := l.buffer
	l.buffer = nil

	for _, item := range buffer {
		if len(item.kvps) > 0 {
			slogadapter.GoKit(l.handler).Log(item.kvps...)
		} else if item.handler.Enabled(context.Background(), item.record.Level) {
			_ = item.handler.Handle(context.Background(), item.record)
		}
	}
}

func (l *Logger) SetTemporaryWriter(w io.Writer) {
	l.writer.SetTemporaryWriter(w)
}

func (l *Logger) RemoveTemporaryWriter() {
	l.writer.RemoveTemporaryWriter()
}

// Log implements log.Logger.
func (l *Logger) Log(kvps ...any) error {
	// Buffer logs before confirming log format is configured in `logging` block.
	l.bufferMut.RLock()
	if !l.hasLogFormat {
		l.bufferMut.RUnlock()
		l.bufferMut.Lock()
		// Check hasLogFormat again; could have changed since the unlock.
		if !l.hasLogFormat {
			l.buffer = append(l.buffer, &bufferedItem{kvps: kvps})
			l.bufferMut.Unlock()
			return nil
		}
		l.bufferMut.Unlock()
	} else {
		l.bufferMut.RUnlock()
	}

	// NOTE(rfratto): slogadapter is a temporary shim while log/slog is still
	// being adopted throughout the codebase.
	return slogadapter.GoKit(l.handler).Log(kvps...)
}

func (l *Logger) addRecord(r slog.Record, df *deferredSlogHandler) {
	l.bufferMut.Lock()
	defer l.bufferMut.Unlock()

	l.buffer = append(l.buffer, &bufferedItem{
		record:  r,
		handler: df,
	})
}

type lokiWriter struct {
	f []loki.LogsReceiver
}

func (fw *lokiWriter) Write(p []byte) (int, error) {
	for _, receiver := range fw.f {
		// We may have been given a nil value in rare circumstances due to
		// misconfiguration or a component which generates exports after
		// construction.
		//
		// Ignore nil values so we don't panic.
		if receiver == nil {
			continue
		}

		select {
		case receiver.Chan() <- loki.Entry{
			Labels: model.LabelSet{"component": "alloy"},
			Entry: push.Entry{
				Timestamp: time.Now(),
				Line:      string(p),
			},
		}:
		default:
			return 0, fmt.Errorf("lokiWriter failed to forward entry, channel was blocked")
		}
	}
	return len(p), nil
}

type formatVar struct {
	mut sync.RWMutex
	f   Format
}

func (f *formatVar) Format() Format {
	f.mut.RLock()
	defer f.mut.RUnlock()
	return f.f
}

func (f *formatVar) Set(format Format) {
	f.mut.Lock()
	defer f.mut.Unlock()
	f.f = format
}

type writerVar struct {
	mut sync.RWMutex

	lokiWriter    *lokiWriter
	innerWriter   io.Writer
	tmpWriter     io.Writer
	suppressInner bool // when true, Write skips innerWriter
}

func (w *writerVar) SetTemporaryWriter(writer io.Writer) {
	w.mut.Lock()
	defer w.mut.Unlock()
	w.tmpWriter = writer
}

func (w *writerVar) RemoveTemporaryWriter() {
	w.mut.Lock()
	defer w.mut.Unlock()
	w.tmpWriter = nil
}

func (w *writerVar) SetLokiWriter(writer *lokiWriter) {
	w.mut.Lock()
	defer w.mut.Unlock()
	w.lokiWriter = writer
}

// SetSuppressInner toggles whether Write delivers bytes to innerWriter.
// Acquires the write lock, so it waits for any in-flight Write calls to
// finish before flipping — guaranteeing in-flight Logs complete against
// the previous state.
func (w *writerVar) SetSuppressInner(b bool) {
	w.mut.Lock()
	defer w.mut.Unlock()
	w.suppressInner = b
}

// HasSink reports whether any active sink will actually consume bytes
// written to this writerVar. Used by bytesHandler.Handle to skip formatting
// entirely when every sink would silently drop the result (event_log
// destination with no write_to and no temporary writer attached).
func (w *writerVar) HasSink() bool {
	w.mut.RLock()
	defer w.mut.RUnlock()
	return (w.innerWriter != nil && !w.suppressInner) ||
		w.lokiWriter != nil ||
		w.tmpWriter != nil
}

func (w *writerVar) Write(p []byte) (int, error) {
	w.mut.RLock()
	defer w.mut.RUnlock()

	if w.innerWriter == nil {
		return 0, fmt.Errorf("no writer available")
	}

	// The following is effectively an io.Multiwriter, but without updating
	// the Multiwriter each time tmpWriter is added or removed.
	if !w.suppressInner {
		if _, err := w.innerWriter.Write(p); err != nil {
			return 0, err
		}
	}

	if w.lokiWriter != nil {
		if _, err := w.lokiWriter.Write(p); err != nil {
			return 0, err
		}
	}

	if w.tmpWriter != nil {
		if _, err := w.tmpWriter.Write(p); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

type bufferedItem struct {
	kvps    []any
	handler *deferredSlogHandler
	record  slog.Record
}
