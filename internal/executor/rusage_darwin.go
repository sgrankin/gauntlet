//go:build darwin

package executor

import "syscall"

// maxrssBytes normalizes syscall.Rusage.Maxrss to bytes. On macOS/Darwin,
// getrusage(2) already reports ru_maxrss in BYTES (the BSD convention) — no
// conversion needed, unlike Linux's KiB. See rusage_linux.go.
func maxrssBytes(ru *syscall.Rusage) int64 {
	return ru.Maxrss
}
