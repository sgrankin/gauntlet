package dashboard

// Whitebox test for formatBytes (server.go): same convention as
// duration_test.go for formatDuration — this file is package dashboard so it
// can call the unexported function directly.

import "testing"

// TestFormatBytes exercises every unit bucket formatBytes switches on, plus
// the non-positive fallback: issue #14's PeakRSS is only ever formatted once
// a caller has already confirmed it's measured (> 0), but formatBytes must
// still degrade sanely rather than panic or print something misleading if
// that changes.
func TestFormatBytes(t *testing.T) {
	cases := []struct {
		b    int64
		want string
	}{
		{-1, "-"},
		{0, "-"},
		{1, "1 B"},
		{999, "999 B"},
		{1000, "1.0 KB"},
		{34_100_000, "34.1 MB"}, // the brief's own example
		{1_500_000, "1.5 MB"},
		{1_000_000_000, "1.0 GB"},
		{1_234_567_890, "1.2 GB"},
		{1_000_000_000_000, "1.0 TB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.b); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.b, got, c.want)
		}
	}
}
