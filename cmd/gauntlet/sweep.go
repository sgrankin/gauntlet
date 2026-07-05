package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// sweepAndRecreate removes dir and everything in it, then recreates it
// empty. Used for every directory under -state that holds only ephemeral,
// process-lifetime state — trialsDir and hooksDir (main.go, pre-existing)
// and, as of S16 (phase-6 audit synthesis), executor scratch dirs. This is
// only safe unconditionally now that AcquireLock (S2) guarantees no other
// gauntlet daemon can be using dir concurrently: main.go must call this (or
// the equivalent inline sweep it already had for trials/hooks) only after
// AcquireLock has succeeded.
func sweepAndRecreate(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("sweep %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}

// sweepContainerOrphans removes containers left behind by a prior gauntlet
// process that crashed (or was killed) before the container runtime's own
// `--rm` cleanup ran for a check still in flight (S16, folded into S2's
// lock-first fix per lifecycle #5's ruling in the audit synthesis: a
// `gauntlet-*` container kill by name is exactly the kind of cross-daemon
// destruction hazard S2 addresses, so this must only ever be called after
// AcquireLock has succeeded — a live sibling daemon's own in-flight
// containers would otherwise be indistinguishable from orphans).
//
// runtime == "" (no container executor configured) is a no-op. A missing
// runtime binary, or any failure listing/removing containers, is logged to
// log and otherwise ignored — this is best-effort startup housekeeping
// (worst case, a stray container lingers an extra restart), never a reason
// to fail daemon startup.
func sweepContainerOrphans(ctx context.Context, runtime string, log io.Writer) {
	if runtime == "" {
		return
	}
	if _, err := exec.LookPath(runtime); err != nil {
		fmt.Fprintf(log, "gauntlet: sweep: container runtime %q not found, skipping orphan sweep: %v\n", runtime, err)
		return
	}

	out, err := exec.CommandContext(ctx, runtime, "ps", "--filter", "name=gauntlet-", "--format", "{{.Names}}").Output()
	if err != nil {
		fmt.Fprintf(log, "gauntlet: sweep: %s ps: %v\n", runtime, err)
		return
	}

	for _, name := range strings.Fields(string(out)) {
		if err := exec.CommandContext(ctx, runtime, "rm", "-f", name).Run(); err != nil {
			fmt.Fprintf(log, "gauntlet: sweep: %s rm -f %s: %v\n", runtime, name, err)
		}
	}
}
