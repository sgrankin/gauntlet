//go:build darwin

package executor

import (
	"syscall"
	"testing"
)

// TestMaxrssBytes_Darwin_PassesThroughBytes pins macOS's getrusage(2)
// convention: ru_maxrss is already reported in BYTES (the BSD heritage),
// unlike Linux's KiB, so maxrssBytes must be a pure pass-through — no
// multiplier. A known input run through the real helper, not a
// hand-derived formula duplicated here, so an accidental "helpfully" added
// multiplier on this platform fails this test directly.
func TestMaxrssBytes_Darwin_PassesThroughBytes(t *testing.T) {
	tests := []struct {
		maxrssBytes int64
	}{
		{0},
		{1},
		{1024},
		{35_651_584}, // ~34MB, an observed order-of-magnitude value
	}
	for _, tt := range tests {
		ru := &syscall.Rusage{Maxrss: tt.maxrssBytes}
		got := maxrssBytes(ru)
		if got != tt.maxrssBytes {
			t.Errorf("maxrssBytes(Maxrss=%d bytes) = %d, want %d (pass-through)", tt.maxrssBytes, got, tt.maxrssBytes)
		}
	}
}
