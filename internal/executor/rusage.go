package executor

import (
	"os"
	"syscall"
	"time"
)

// captureRusage extracts best-effort peak RSS and CPU time from a finished
// command's process state (LocalExecutor only — see rusage_linux.go/
// rusage_darwin.go for the platform-specific byte normalization this
// depends on). Returns all zeros when ps is nil (the command never
// started — e.g. exec itself failed, so there is nothing to report) or its
// SysUsage isn't a *syscall.Rusage (never true on the linux/darwin targets
// this package builds for — see .goreleaser.yaml; defensive rather than a
// panic if that ever changes).
//
// The rusage the kernel hands back through Wait covers the direct child
// PLUS every descendant it has already reaped via its own wait(2) calls —
// a `sh -c` pipeline's subshells, or a build tool's ordinary worker
// processes, are all folded in so long as each intermediate process waits
// for its own children (every ordinary shell and build tool does). A
// double-forking daemonizer that detaches and outlives the check's direct
// child escapes this: its usage is simply never attributed to the check.
// This is a documented limitation (issue #14), not a bug, and there is no
// polling or /proc-walking here to work around it — that is deliberately
// out of scope (see the container executor's investigation notes in
// container.go for why the same "no polling" line is drawn there too).
//
// Captured identically regardless of outcome — success, failure, or a
// cancellation kill — since ps is non-nil (and its rusage populated)
// whenever the process was actually started and waited on, cancellation
// included: cmd.Cancel's SIGKILL still lets Wait populate ProcessState,
// it just reports a signalled exit.
func captureRusage(ps *os.ProcessState) (peakRSS int64, userCPU, sysCPU time.Duration) {
	if ps == nil {
		return 0, 0, 0
	}
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0, 0, 0
	}
	return maxrssBytes(ru), time.Duration(ru.Utime.Nano()), time.Duration(ru.Stime.Nano())
}
