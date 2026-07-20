//go:build linux

package executor

import (
	"syscall"
	"testing"
)

// TestMaxrssBytes_Linux_MultipliesByKiB pins Linux's getrusage(2) convention:
// ru_maxrss is reported in KIBIBYTES, so maxrssBytes must multiply by 1024
// to land on core.CheckResult.PeakRSS's documented byte unit. A known input
// run through the real helper, not a hand-derived formula duplicated here,
// so a future accidental change to the multiplier fails this test directly.
func TestMaxrssBytes_Linux_MultipliesByKiB(t *testing.T) {
	tests := []struct {
		maxrssKiB int64
		wantBytes int64
	}{
		{0, 0},
		{1, 1024},
		{1024, 1024 * 1024},   // 1 MiB in, 1 MiB out
		{34620, 34620 * 1024}, // observed value for a real 32MB-buffer test case
	}
	for _, tt := range tests {
		ru := &syscall.Rusage{Maxrss: tt.maxrssKiB}
		got := maxrssBytes(ru)
		if got != tt.wantBytes {
			t.Errorf("maxrssBytes(Maxrss=%d KiB) = %d, want %d bytes", tt.maxrssKiB, got, tt.wantBytes)
		}
	}
}
