package dashboard

// Whitebox tests for buildDepthSVG (server.go): unlike dashboard_test.go
// (package dashboard_test), this file can call the unexported function
// directly with hand-built history.DepthPoint values and a fixed since/now,
// so the expected pixel coordinates are fully deterministic — no dependency
// on wall-clock time the way a round-trip through the HTTP handler would
// have (handleChecks anchors the chart's right edge on time.Now()).

import (
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/history"
)

func TestBuildDepthSVG_EmptySeries(t *testing.T) {
	now := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	got := buildDepthSVG(nil, now.Add(-24*time.Hour), now)
	if got != "" {
		t.Errorf("buildDepthSVG(nil, ...) = %q, want empty", got)
	}
}

// TestBuildDepthSVG_GapCarriesForward feeds two samples 48h apart (since's
// heartbeat is 10m, a real series would never actually gap this wide, but
// the renderer must not assume that — it should carry the earlier value
// forward across whatever gap it's given, never interpolate toward the next
// value or drop the gap as missing data).
//
// since == the first sample and now == 96h after it are chosen so every
// coordinate is an exact, hand-computable number (chart is 700x120 with
// padding 32/8/8/20 -> a 660x92 plot area; the 48h sample sits at exactly
// half the 96h span -> x = 32 + 0.5*660 = 362.0), so the expected polyline
// string can be asserted verbatim rather than approximately.
func TestBuildDepthSVG_GapCarriesForward(t *testing.T) {
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := since.Add(96 * time.Hour)
	points := []history.DepthPoint{
		{At: since, Waiting: 3, InFlight: 1, Parked: 0},
		{At: since.Add(48 * time.Hour), Waiting: 0, InFlight: 0, Parked: 2},
	}

	svg := string(buildDepthSVG(points, since, now))

	if !strings.Contains(svg, `class="depth-chart"`) {
		t.Fatalf("missing chart root:\n%s", svg)
	}

	// maxV across all three series is 3 (Waiting's first sample) -> y(3) =
	// 8 + 92 - 92 = 8.0 (top), y(0) = 8 + 92 - 0 = 100.0 (bottom, on axis).
	// x(since) = 32.0, x(since+48h) = 362.0 (half the 660-wide plot), x(now)
	// = 692.0 (right edge).
	//
	// The critical assertion is the 32.0,8.0 -> 362.0,8.0 segment: the
	// value (3) held flat across the entire 48h gap, only dropping at the
	// second sample's own timestamp — never interpolated, never zeroed
	// early.
	const wantWaiting = `points="32.0,8.0 362.0,8.0 362.0,100.0 692.0,100.0" class="depth-waiting"`
	if !strings.Contains(svg, wantWaiting) {
		t.Errorf("depth-waiting polyline = missing expected carry-forward points\nwant substring: %s\ngot svg:\n%s", wantWaiting, svg)
	}

	if !strings.Contains(svg, `class="depth-inflight"`) {
		t.Errorf("missing depth-inflight polyline:\n%s", svg)
	}
	if !strings.Contains(svg, `class="depth-parked"`) {
		t.Errorf("missing depth-parked polyline:\n%s", svg)
	}
}

func TestBuildDepthSVG_AxisLabels(t *testing.T) {
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	now := since.Add(24 * time.Hour)
	points := []history.DepthPoint{
		{At: since, Waiting: 5, InFlight: 0, Parked: 0},
	}

	svg := string(buildDepthSVG(points, since, now))
	if !strings.Contains(svg, ">5<") {
		t.Errorf("expected y-axis max label \"5\":\n%s", svg)
	}
	if !strings.Contains(svg, ">0<") {
		t.Errorf("expected y-axis zero label:\n%s", svg)
	}
	if !strings.Contains(svg, "07-01") {
		t.Errorf("expected x-axis start-date label:\n%s", svg)
	}
}
