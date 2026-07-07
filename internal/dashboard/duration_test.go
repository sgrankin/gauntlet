package dashboard

// Whitebox test for formatDuration (server.go): unlike dashboard_test.go
// (package dashboard_test), this file can call the unexported function
// directly, same convention as svg_test.go for buildDepthSVG.

import (
	"testing"
	"time"
)

// TestFormatDuration exercises every bucket formatDuration switches on,
// with particular attention to the two boundaries the friendly-duration pass
// added (90s and 1h): below 90s the pre-existing native Duration.String()
// rendering is unchanged, at/above 90s it's "Xm Ys", and at/above 1h it's
// "Xh Ym".
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{500 * time.Millisecond, "500ms"},
		{5 * time.Second, "5s"},
		{65 * time.Second, "1m5s"},                   // < 90s: unchanged native rendering
		{89 * time.Second, "1m29s"},                  // still just under the 90s boundary
		{90 * time.Second, "1m 30s"},                 // exactly the 90s boundary: new format kicks in
		{(5*60 + 27) * time.Second, "5m 27s"},        // brief's own example
		{59*time.Minute + 59*time.Second, "59m 59s"}, // just under 1h
		{time.Hour, "1h 0m"},                         // exactly the 1h boundary
		{time.Hour + 30*time.Minute, "1h 30m"},
		{-5 * time.Second, "0s"}, // negative clamps to 0, same as before
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
