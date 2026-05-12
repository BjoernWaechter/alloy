package logging

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// spyHandler is a controllable slog.Handler used to verify fanoutHandler
// dispatches calls correctly. WithAttrs/WithGroup return new instances that
// record what they received, so tests can inspect propagation.
type spyHandler struct {
	enabled      bool
	handleErr    error
	handled      []slog.Record
	appliedAttrs []slog.Attr
	appliedGroup string
}

func (s *spyHandler) Enabled(_ context.Context, _ slog.Level) bool { return s.enabled }

func (s *spyHandler) Handle(_ context.Context, r slog.Record) error {
	s.handled = append(s.handled, r)
	return s.handleErr
}

func (s *spyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &spyHandler{
		enabled:      s.enabled,
		handleErr:    s.handleErr,
		appliedAttrs: attrs,
	}
}

func (s *spyHandler) WithGroup(name string) slog.Handler {
	return &spyHandler{
		enabled:      s.enabled,
		handleErr:    s.handleErr,
		appliedGroup: name,
	}
}

func TestFanoutHandler_Enabled(t *testing.T) {
	tests := []struct {
		name               string
		aEnabled, bEnabled bool
		want               bool
	}{
		{"both enabled", true, true, true},
		{"only a enabled", true, false, true},
		{"only b enabled", false, true, true},
		{"neither enabled", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fanoutHandler{
				a: &spyHandler{enabled: tc.aEnabled},
				b: &spyHandler{enabled: tc.bEnabled},
			}
			require.Equal(t, tc.want, f.Enabled(context.Background(), slog.LevelInfo))
		})
	}
}

func TestFanoutHandler_Handle(t *testing.T) {
	errA := errors.New("a failed")
	errB := errors.New("b failed")

	tests := []struct {
		name               string
		aEnabled, bEnabled bool
		aErr, bErr         error
		wantHandledA       bool
		wantHandledB       bool
		// wantErrs is the set of underlying errors the returned error must
		// wrap (verified via errors.Is). Empty means "no error expected".
		wantErrs []error
	}{
		{
			name:         "both enabled, no errors",
			aEnabled:     true,
			bEnabled:     true,
			wantHandledA: true,
			wantHandledB: true,
		},
		{
			name:         "only a enabled",
			aEnabled:     true,
			bEnabled:     false,
			wantHandledA: true,
			wantHandledB: false,
		},
		{
			name:         "only b enabled",
			aEnabled:     false,
			bEnabled:     true,
			wantHandledA: false,
			wantHandledB: true,
		},
		{
			name:         "neither enabled",
			aEnabled:     false,
			bEnabled:     false,
			wantHandledA: false,
			wantHandledB: false,
		},
		{
			name:         "both enabled, a errors",
			aEnabled:     true,
			bEnabled:     true,
			aErr:         errA,
			wantHandledA: true,
			wantHandledB: true,
			wantErrs:     []error{errA},
		},
		{
			name:         "both enabled, b errors",
			aEnabled:     true,
			bEnabled:     true,
			bErr:         errB,
			wantHandledA: true,
			wantHandledB: true,
			wantErrs:     []error{errB},
		},
		{
			name:         "both enabled, both error (joined)",
			aEnabled:     true,
			bEnabled:     true,
			aErr:         errA,
			bErr:         errB,
			wantHandledA: true,
			wantHandledB: true,
			wantErrs:     []error{errA, errB},
		},
		{
			// Confirms a disabled child is skipped entirely: its handleErr
			// is not surfaced and its Handle is not called.
			name:         "disabled child's error is not surfaced",
			aEnabled:     true,
			bEnabled:     false,
			aErr:         errA,
			bErr:         errB,
			wantHandledA: true,
			wantHandledB: false,
			wantErrs:     []error{errA},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sa := &spyHandler{enabled: tc.aEnabled, handleErr: tc.aErr}
			sb := &spyHandler{enabled: tc.bEnabled, handleErr: tc.bErr}
			f := fanoutHandler{a: sa, b: sb}

			r := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
			err := f.Handle(context.Background(), r)

			if len(tc.wantErrs) == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				for _, want := range tc.wantErrs {
					require.ErrorIs(t, err, want)
				}
			}

			if tc.wantHandledA {
				require.Len(t, sa.handled, 1)
			} else {
				require.Empty(t, sa.handled)
			}
			if tc.wantHandledB {
				require.Len(t, sb.handled, 1)
			} else {
				require.Empty(t, sb.handled)
			}

			// A disabled child whose handleErr is set must not have its
			// error surfaced.
			if !tc.bEnabled && tc.bErr != nil {
				require.NotErrorIs(t, err, tc.bErr)
			}
			if !tc.aEnabled && tc.aErr != nil {
				require.NotErrorIs(t, err, tc.aErr)
			}
		})
	}
}

func TestFanoutHandler_WithAttrs_PropagatesToBothChildren(t *testing.T) {
	sa := &spyHandler{}
	sb := &spyHandler{}
	f := fanoutHandler{a: sa, b: sb}

	attrs := []slog.Attr{slog.String("k", "v")}
	newF, ok := f.WithAttrs(attrs).(fanoutHandler)
	require.True(t, ok, "WithAttrs should return a fanoutHandler")

	require.Equal(t, attrs, newF.a.(*spyHandler).appliedAttrs)
	require.Equal(t, attrs, newF.b.(*spyHandler).appliedAttrs)

	// Original children should be untouched.
	require.Nil(t, sa.appliedAttrs)
	require.Nil(t, sb.appliedAttrs)
}

func TestFanoutHandler_WithGroup_PropagatesToBothChildren(t *testing.T) {
	sa := &spyHandler{}
	sb := &spyHandler{}
	f := fanoutHandler{a: sa, b: sb}

	newF, ok := f.WithGroup("g").(fanoutHandler)
	require.True(t, ok, "WithGroup should return a fanoutHandler")

	require.Equal(t, "g", newF.a.(*spyHandler).appliedGroup)
	require.Equal(t, "g", newF.b.(*spyHandler).appliedGroup)

	// Original children should be untouched.
	require.Empty(t, sa.appliedGroup)
	require.Empty(t, sb.appliedGroup)
}
