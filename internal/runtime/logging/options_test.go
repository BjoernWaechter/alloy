package logging

import (
	"testing"

	"github.com/grafana/alloy/syntax"
	"github.com/stretchr/testify/require"
)

// TestOptions_EndToEnd drives the real syntax decoder to verify that:
//   - when `destination` is omitted, it defaults to stderr;
//   - explicit values are decoded correctly;
//   - an unrecognized destination is rejected at decode time.
func TestOptions_EndToEnd(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		want    LogDestination
		wantErr bool
	}{
		{
			name:   "destination omitted defaults to stderr",
			config: `level = "info"`,
			want:   LogDestinationStderr,
		},
		{
			name:   "explicit stderr",
			config: `destination = "stderr"`,
			want:   LogDestinationStderr,
		},
		{
			name:   "explicit none",
			config: `destination = "none"`,
			want:   LogDestinationNone,
		},
		{
			name:    "unrecognized destination is rejected",
			config:  `destination = "not_a_destination"`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var o Options
			err := syntax.Unmarshal([]byte(tc.config), &o)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, o.Destination)
		})
	}
}
