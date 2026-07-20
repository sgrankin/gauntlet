//go:build linux

package executor

import "syscall"

// maxrssBytes normalizes syscall.Rusage.Maxrss to bytes. On Linux,
// getrusage(2) reports ru_maxrss in KIBIBYTES — multiply by 1024. See
// rusage_darwin.go for the opposite convention on macOS.
func maxrssBytes(ru *syscall.Rusage) int64 {
	return ru.Maxrss * 1024
}
