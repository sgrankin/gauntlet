package main

import (
	"context"
	"encoding/json"
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

// containerNamePrefix is the prefix sweepContainerOrphans matches container
// names against, mirroring internal/executor/container.go's containerName
// (which mints names in exactly this shape): "gauntlet-<token>-" when token
// is non-empty, or the host-global "gauntlet-" when it's empty (test
// compatibility / no-token daemons).
func containerNamePrefix(token string) string {
	if token == "" {
		return "gauntlet-"
	}
	return "gauntlet-" + token + "-"
}

// sweepContainerOrphans removes containers left behind by a prior gauntlet
// process that crashed (or was killed) before the container runtime's own
// `--rm` cleanup ran for a check still in flight (S16, folded into S2's
// lock-first fix per lifecycle #5's ruling in the audit synthesis).
//
// token scopes the sweep to containers this daemon itself could plausibly
// have created (B1, phase-6 B-track review). AcquireLock (S2) only takes an
// advisory lock on THIS process's -state directory — it says nothing about
// the container-name namespace, which is host-global: two gauntlet daemons
// pointed at different -state dirs each acquire their own lock and both
// start successfully, but without this scoping, daemon B's startup sweep
// would `ps`/`ls`-match and `rm -f` daemon A's live, in-flight check
// containers, since both mint names under the same bare "gauntlet-" prefix.
// Filtering on "gauntlet-<token>-" (token is a hash of this daemon's own
// absolute -state path, minted in main.go and threaded into both this sweep
// and executor.Params.Token, which names this daemon's own containers) is
// what actually closes that gap: the flock guards the state dir, the token
// guards the container namespace — two independent scopes, both required.
// Empty token preserves the pre-B1 host-global "gauntlet-" match exactly.
//
// runtime == "" (no container executor configured) is a no-op. A missing
// runtime binary, or any failure listing/removing containers, is logged to
// log and otherwise ignored — this is best-effort startup housekeeping
// (worst case, a stray container lingers an extra restart), never a reason
// to fail daemon startup.
func sweepContainerOrphans(ctx context.Context, runtime, token string, log io.Writer) {
	if runtime == "" {
		return
	}
	if _, err := exec.LookPath(runtime); err != nil {
		fmt.Fprintf(log, "gauntlet: sweep: container runtime %q not found, skipping orphan sweep: %v\n", runtime, err)
		return
	}

	names, err := listContainerNames(ctx, runtime)
	if err != nil {
		fmt.Fprintf(log, "gauntlet: sweep: %s: %v\n", runtime, err)
		return
	}

	prefix := containerNamePrefix(token)
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if err := exec.CommandContext(ctx, runtime, "rm", "-f", name).Run(); err != nil {
			fmt.Fprintf(log, "gauntlet: sweep: %s rm -f %s: %v\n", runtime, name, err)
		}
	}
}

// listContainerNames returns every container name runtime currently knows
// about, running or stopped (hence "all") — sweepContainerOrphans
// prefix-filters the result client-side in Go.
//
// This deliberately never asks the runtime itself to filter by name: docker
// and podman support `ps --filter name=...`, but Apple's `container` CLI has
// no `ps` subcommand at all (its listing command is `list`/`ls`) — live
// finding, phase-6 B-track review: `container ps --filter name=... --format
// {{.Names}}` (the invocation this code used before) exits 64 ("Plugin
// 'container-ps' not found"), which silently degraded the whole sweep to a
// no-op via the log-and-continue error path below. Doing one plain listing
// call per runtime and filtering by prefix in Go afterward, instead of
// leaning on a per-runtime filter flag, is the one shape verified to work
// identically across all three runtimes.
func listContainerNames(ctx context.Context, runtime string) ([]string, error) {
	if runtime == "container" {
		out, err := exec.CommandContext(ctx, runtime, "ls", "-a", "--format", "json").Output()
		if err != nil {
			return nil, fmt.Errorf("ls: %w", err)
		}
		return parseContainerLSNames(out)
	}
	// docker/podman: plain `ps -a --format {{.Names}}`, one name per line.
	out, err := exec.CommandContext(ctx, runtime, "ps", "-a", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// parseContainerLSNames extracts container names from `container ls --format
// json`'s output: an array of objects each carrying the --name value in a
// top-level "id" field. Live-verified against `container` CLI 1.0.0 (phase-6
// B-track review): `container run --name foo ... && container ls -a --format
// json` emits `[{"id":"foo","configuration":{"id":"foo",...},...}]` — "id" at
// the top level is what this parses; every other field is ignored.
func parseContainerLSNames(out []byte) ([]string, error) {
	var entries []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("parse ls --format json: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.ID)
	}
	return names, nil
}
