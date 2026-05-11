package logging

import (
	"context"
	"errors"
	"log/slog"
)

// fanoutHandler dispatches each slog operation to two child handlers. Used
// when the Windows Event Log destination is active alongside the regular
// text/byte handler so that write_to still receives records.
type fanoutHandler struct{ a, b slog.Handler }

func (f fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return f.a.Enabled(ctx, l) || f.b.Enabled(ctx, l)
}

func (f fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errA, errB error
	if f.a.Enabled(ctx, r.Level) {
		errA = f.a.Handle(ctx, r)
	}
	if f.b.Enabled(ctx, r.Level) {
		errB = f.b.Handle(ctx, r)
	}
	return errors.Join(errA, errB)
}

func (f fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return fanoutHandler{a: f.a.WithAttrs(attrs), b: f.b.WithAttrs(attrs)}
}

func (f fanoutHandler) WithGroup(name string) slog.Handler {
	return fanoutHandler{a: f.a.WithGroup(name), b: f.b.WithGroup(name)}
}
