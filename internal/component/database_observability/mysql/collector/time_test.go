package collector

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_CalculateWallTime(t *testing.T) {
	t.Run("calculates the timestamp at which an event happened", func(t *testing.T) {
		serverStartTime := 2.0
		timer := secondsToPicoseconds(2)
		lastUptime := 0.0

		result := calculateWallTime(serverStartTime, timer, timer, lastUptime)
		require.Equalf(t, 4000.0, result, "got %f, want 4000", result)
	})

	t.Run("calculates the timestamp, taking into account the overflows", func(t *testing.T) {
		serverStartTime := 3.0
		timer := secondsToPicoseconds(2)
		lastUptime := picosecondsToSeconds(math.MaxUint64) + 1

		result := calculateWallTime(serverStartTime, timer, timer, lastUptime)
		require.Equalf(t, 18446749073.709553, result, "got %f, want 18446749073.709553", result)
	})

	t.Run("calculates another timestamp when timer approaches overflow", func(t *testing.T) {
		serverStartTime := 3.0
		timer := float64(math.MaxUint64 - 5)
		lastUptime := picosecondsToSeconds(math.MaxUint64) + 1

		result := calculateWallTime(serverStartTime, timer, timer, lastUptime)
		require.Equalf(t, 3.6893491147419106e+10, result, "got %f, want 3.6893491147419106e+10", result)
	})

	t.Run("treats timer > anchor as previous overflow window (e.g. TIMER_START spanning overflow)", func(t *testing.T) {
		serverStartTime := 3.0
		// TIMER_END just past overflow (small remainder), TIMER_START still near max-uint64
		// from the previous window; uptime reflects one overflow has occurred.
		timerStart := float64(math.MaxUint64 - 5)
		timerEnd := secondsToPicoseconds(2)
		lastUptime := picosecondsToSeconds(math.MaxUint64) + picosecondsToSeconds(timerEnd)

		// With the wraparound check, calculateWallTime should subtract one overflow window
		// from the count, yielding a timestamp ~213 days (one window) earlier than what the
		// pre-fix call would have produced.
		actualWithAnchor := calculateWallTime(serverStartTime, timerStart, timerEnd, lastUptime)
		actualWithoutAnchor := calculateWallTime(serverStartTime, timerStart, timerStart, lastUptime)
		// The anchored result should be exactly one overflow window earlier than the
		// unanchored (pre-fix) result.
		expectedDelta := secondsToMilliseconds(picosecondsOverflowInSeconds)
		require.InDeltaf(t, expectedDelta, actualWithoutAnchor-actualWithAnchor, 1e-3,
			"anchored call should be one overflow window earlier; got delta %f, want %f",
			actualWithoutAnchor-actualWithAnchor, expectedDelta)
	})
}

func Test_CalculateNumberOfOverflows(t *testing.T) {
	testCases := map[string]struct {
		expected uint64
		uptime   float64
	}{
		"0 overflows": {0, 5},
		"1 overflow":  {1, picosecondsToSeconds(math.MaxUint64) + 5},
		"2 overflows": {2, picosecondsToSeconds(math.MaxUint64)*2 + 5},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			require.EqualValues(t, tc.expected, calculateNumberOfOverflows(tc.uptime))
		})
	}
}
