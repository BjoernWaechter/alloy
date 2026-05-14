package logging

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"github.com/grafana/alloy/internal/runtime/logging/eventlog"
	"github.com/grafana/alloy/internal/runtime/logging/eventlog/testutil"
	"github.com/stretchr/testify/require"
)

// TestLogger_EventLog_LevelReloadTakesEffect verifies that changing the
// log level on an event_log → event_log reload actually propagates to the
// event log handler, even though we deliberately don't reconstruct it.
func TestLogger_EventLog_LevelReloadTakesEffect(t *testing.T) {
	mock := &testutil.MockEventLog{}
	var inner bytes.Buffer
	l, err := NewDeferred(&inner)
	require.NoError(t, err)
	l.eventLogOpener = func(_ string) (eventlog.EventLog, error) {
		return mock, nil
	}

	// Start at Info level on event_log destination.
	require.NoError(t, l.Update(Options{
		Level:       LevelInfo,
		Format:      FormatLogfmt,
		Destination: LogDestinationWindowsEventLog,
	}))

	h := l.Slog().Handler()
	ctx := t.Context()

	// Debug record should be filtered at Info level.
	require.False(t, h.Enabled(ctx, slog.LevelDebug),
		"handler should report debug as disabled at Info level")
	require.NoError(t, h.Handle(ctx,
		slog.NewRecord(time.Now(), slog.LevelDebug, "debug-before", 0)))
	require.Empty(t, mock.Infos, "debug record at Info level should be filtered")

	// Reload at Debug level, same destination — no reopen.
	require.NoError(t, l.Update(Options{
		Level:       LevelDebug,
		Format:      FormatLogfmt,
		Destination: LogDestinationWindowsEventLog,
	}))

	// Debug record should now pass through.
	require.True(t, h.Enabled(ctx, slog.LevelDebug),
		"handler should report debug as enabled at Debug level")
	require.NoError(t, h.Handle(ctx,
		slog.NewRecord(time.Now(), slog.LevelDebug, "debug-after", 0)))
	require.Len(t, mock.Infos, 1, "debug record at Debug level should reach event log")
	require.Contains(t, mock.Infos[0], "debug-after")
}

// TestLogger_EventLog_BytesHandlerSkipsFormatWhenNoSink verifies that when
// the destination is windows_event_log and neither write_to nor a temporary
// writer is attached, the bytes handler short-circuits without invoking the
// underlying slog text/JSON formatter (no record is built; ReplaceAttr is
// not called). When a temporary writer is later attached, the bytes handler
// resumes formatting and the temp writer receives the record.
func TestLogger_EventLog_BytesHandlerSkipsFormatWhenNoSink(t *testing.T) {
	mock := &testutil.MockEventLog{}
	var inner bytes.Buffer
	l, err := NewDeferred(&inner)
	require.NoError(t, err)
	l.eventLogOpener = func(_ string) (eventlog.EventLog, error) {
		return mock, nil
	}

	// Detect any formatter invocation by counting ReplaceAttr calls.
	replacerCalls := 0
	l.bytesHandler.replacer = func(groups []string, a slog.Attr) slog.Attr {
		replacerCalls++
		return replace(groups, a)
	}

	require.NoError(t, l.Update(Options{
		Level:       LevelInfo,
		Format:      FormatLogfmt,
		Destination: LogDestinationWindowsEventLog,
	}))

	// No write_to, no tmpWriter — writerVar should report no sink.
	require.False(t, l.writer.HasSink(), "writerVar should have no active sink")

	require.NoError(t, l.Log("msg", "skip-formatting"))
	require.Len(t, mock.Infos, 1, "event log still receives the record")
	require.Zero(t, replacerCalls, "bytes handler must not format when no sink is listening")

	// Attach a temp writer (mimics /-/support). The bytes handler must
	// resume formatting from the next call onward.
	var tmp bytes.Buffer
	l.SetTemporaryWriter(&tmp)
	require.True(t, l.writer.HasSink())

	require.NoError(t, l.Log("msg", "with-temp"))
	require.Len(t, mock.Infos, 2, "event log still receives the record")
	require.Contains(t, tmp.String(), "with-temp", "temp writer captures the formatted record")
	require.Greater(t, replacerCalls, 0, "bytes handler should format once a sink is attached")
}

// TestLogger_EventLog_TransitionToStderrClosesHandle verifies that
// genuinely leaving the windows_event_log destination DOES close the
// handle and subsequent logs no longer reach it. The bytes path is now
// the stderr writer, so the record reaches stderr instead.
func TestLogger_EventLog_TransitionToStderrClosesHandle(t *testing.T) {
	mock := &testutil.MockEventLog{}
	var inner bytes.Buffer
	l, err := NewDeferred(&inner)
	require.NoError(t, err)
	l.eventLogOpener = func(_ string) (eventlog.EventLog, error) {
		return mock, nil
	}

	require.NoError(t, l.Update(Options{
		Level:       LevelInfo,
		Format:      FormatLogfmt,
		Destination: LogDestinationWindowsEventLog,
	}))

	require.NoError(t, l.Update(Options{
		Level:       LevelInfo,
		Format:      FormatLogfmt,
		Destination: LogDestinationStderr,
	}))
	require.False(t, l.windowsEventLogHandler.IsOpen(),
		"event_log → stderr should close the event log handle")

	mock.Reset()
	require.NoError(t, l.Log("msg", "stderr-only"))
	require.Empty(t, mock.Infos, "no further records should reach the event log after transition")
	require.Contains(t, inner.String(), "stderr-only",
		"bytes path should now reach the stderr writer")
}
