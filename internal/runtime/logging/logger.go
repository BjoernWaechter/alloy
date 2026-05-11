package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
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
	// paths. It is l.bytesHandler by default and a
	// fanoutHandler{windowsEventLogHandler, bytesHandler} when the Windows
	// Event Log destination is active. Stored atomically so Log/Enabled can
	// read it without a lock while Update reassigns it.
	//
	// Trade-off: a Log goroutine can load the old handler microseconds before
	// Update swaps it in, so during a reload there is a brief window where a
	// record may dispatch through the previous configuration — e.g. an event
	// log → stderr reload could duplicate a record (old fanoutHandler still
	// writes to event log while the new stderr path also writes), and an
	// event log reload could send a record to a just-closed event log handle
	// (which returns an error from el.Info but does not crash). Accepted
	// because (1) config reloads are rare, (2) the worst case is a couple of
	// duplicated or dropped records per reload, and (3) the alternative —
	// holding bufferMut for the entire dispatch — would let a slow sink
	// (e.g. a blocked Loki receiver) stall Update indefinitely, which is a
	// worse failure mode.
	handler atomic.Pointer[handlerHolder]
}

// handlerHolder wraps a slog.Handler so it can be stored in an
// atomic.Pointer (which requires a concrete pointer type).
type handlerHolder struct{ h slog.Handler }

var _ EnabledAware = (*Logger)(nil)

// Enabled implements EnabledAware interface.
func (l *Logger) Enabled(ctx context.Context, level slog.Level) bool {
	return l.handler.Load().h.Enabled(ctx, level)
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
		writer  writerVar
	)
	l := &Logger{
		inner: w,

		buffer:       []*bufferedItem{},
		hasLogFormat: false,

		level:  &leveler,
		format: &format,
		writer: &writer,
		bytesHandler: &bytesHandler{
			w:         &writer,
			leveler:   &leveler,
			formatter: &format,
			replacer:  replace,
		},
		eventLogOpener: eventlog.GetEventLogOpener(),
	}
	l.handler.Store(&handlerHolder{h: l.bytesHandler})
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

	// Close any existing Windows Event Log handler; we'll open a fresh one
	// below if the destination still calls for it.
	if l.windowsEventLogHandler != nil {
		_ = l.windowsEventLogHandler.Close()
		l.windowsEventLogHandler = nil
	}
	// Configure the destination. The bytes handler always writes through
	// l.writer, which fans bytes to innerWriter (below) and, when set,
	// lokiWriter (write_to) — so write_to receives logs regardless of
	// destination.
	if o.EffectiveDestination() == LogDestinationWindowsEventLog {
		el, err := l.eventLogOpener("Alloy")
		if err != nil {
			return fmt.Errorf("failed to open Windows Event Log: %w", err)
		}
		l.windowsEventLogHandler = newWindowsEventLogHandler(el, l.level, replace)
		if l.windowsEventLogHandler == nil {
			return fmt.Errorf("failed to create Windows Event Log handler: %w", err)
		}
		// Suppress innerWriter so the bytes handler only feeds write_to; the
		// event log itself is delivered via windowsEventLogHandler.
		l.writer.SetInnerWriter(io.Discard)
		l.handler.Store(&handlerHolder{h: fanoutHandler{a: l.windowsEventLogHandler, b: l.bytesHandler}})
	} else {
		l.writer.SetInnerWriter(l.inner)
		l.handler.Store(&handlerHolder{h: l.bytesHandler})
	}
	if len(o.WriteTo) > 0 {
		l.writer.SetLokiWriter(&lokiWriter{o.WriteTo})
	}
	l.bufferMut.Unlock()

	// Build deferred handlers outside bufferMut to avoid a deadlock: concurrent
	// Handle() calls hold a child handler's RLock while waiting for bufferMut
	// (via addRecord), while Update holding bufferMut and waiting for the child's
	// write lock in buildHandlers creates a cycle.
	if l.deferredSlog != nil {
		l.deferredSlog.buildHandlers(nil)
	}

	// Flip hasLogFormat and drain/replay while holding bufferMut so new Log()
	// calls block on RLock until replay finishes — preserving the original
	// guarantee that buffered logs are emitted before newly-arriving ones.
	l.bufferMut.Lock()
	defer l.bufferMut.Unlock()
	l.hasLogFormat = true
	buffer := l.buffer
	l.buffer = nil

	// Replay buffered logs. The bufferedItem's handler (for slog records) was
	// rebuilt above via deferredSlog.buildHandlers and now points at l.handler,
	// which fans out to the event log + bytes handler when both are needed.
	h := l.handler.Load().h
	for _, bufferedLogChunk := range buffer {
		if len(bufferedLogChunk.kvps) > 0 {
			slogadapter.GoKit(h).Log(bufferedLogChunk.kvps...)
		} else if bufferedLogChunk.handler.Enabled(context.Background(), bufferedLogChunk.record.Level) {
			_ = bufferedLogChunk.handler.Handle(context.Background(), bufferedLogChunk.record)
		}
	}

	return nil
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
	return slogadapter.GoKit(l.handler.Load().h).Log(kvps...)
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

	lokiWriter  *lokiWriter
	innerWriter io.Writer
	tmpWriter   io.Writer
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

func (w *writerVar) SetInnerWriter(writer io.Writer) {
	w.mut.Lock()
	defer w.mut.Unlock()
	w.innerWriter = writer
}

func (w *writerVar) SetLokiWriter(writer *lokiWriter) {
	w.mut.Lock()
	defer w.mut.Unlock()
	w.lokiWriter = writer
}

func (w *writerVar) Write(p []byte) (int, error) {
	w.mut.RLock()
	defer w.mut.RUnlock()

	if w.innerWriter == nil {
		return 0, fmt.Errorf("no writer available")
	}

	// The following is effectively an io.Multiwriter, but without updating
	// the Multiwriter each time tmpWriter is added or removed.
	if _, err := w.innerWriter.Write(p); err != nil {
		return 0, err
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
