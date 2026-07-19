// `gauntlet validate` is client-side porcelain: static validation of the
// operator config and/or a repo check spec, with no daemon behind it. It
// exists so a config edit (or a repo's own pre-merge check) can be checked
// at typing time, or self-checked in CI, without standing up a daemon.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/queue"
)

// runValidate implements "gauntlet validate", writing to os.Stdout.
// validate.go's body lives in runValidateTo, which takes an io.Writer
// instead, so tests can assert on the "<path>: ok" lines without piping
// os.Stdout — same reasoning as status.go's renderStatus taking an
// io.Writer rather than printing straight to os.Stdout.
func runValidate(args []string) error {
	return runValidateTo(os.Stdout, args)
}

// runValidateTo does the actual work. Daemon-mode validation IS
// config.LoadDaemon, repo-mode validation IS config.ParseChecks — this
// never re-implements either parser, so a config LoadDaemon (or a spec
// ParseChecks) accepts or rejects always matches what this command reports.
// With both flags given, it additionally applies the same spec-vs-daemon
// predicates the queue applies when it loads a check spec against a live
// daemon (crossCheck below), so a spec that would be rejected at run time
// is caught here first.
//
// STRICTLY no side effects: LoadDaemon/ParseChecks only read the given
// file(s) — no network, no state dir, no git subprocess, no key-file open.
// Host/credential probing is deliberately out of scope; that's a separate
// `doctor` command, not this one.
func runValidateTo(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to the daemon config (gauntlet.kdl) to validate")
	checksPath := fs.String("checks", "", "path to a repo check spec (.gauntlet.kdl) to validate")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *configPath == "" && *checksPath == "" {
		return errors.New("nothing to validate: pass -config (daemon config), -checks (repo check spec), or both")
	}

	var cfg *config.Daemon
	if *configPath != "" {
		c, err := config.LoadDaemon(*configPath)
		if err != nil {
			return err
		}
		cfg = c
		fmt.Fprintf(w, "%s: ok\n", *configPath)
	}

	var spec *config.CheckSpec
	if *checksPath != "" {
		data, err := os.ReadFile(*checksPath)
		if err != nil {
			// LoadDaemon prefixes its own errors "config:"; give the
			// checks file the matching context so a two-flag invocation
			// says which input failed to read.
			return fmt.Errorf("checks: %w", err)
		}
		s, err := config.ParseChecks(data)
		if err != nil {
			return err
		}
		spec = s
		fmt.Fprintf(w, "%s: ok\n", *checksPath)
	}

	if cfg != nil && spec != nil {
		if err := crossCheck(cfg, spec); err != nil {
			return err
		}
	}

	return nil
}

// crossCheck applies the spec-vs-daemon gates the queue applies when a
// spec is loaded against a live daemon: queue.SpecRejectReason, THE
// function both of the daemon's run-start paths call, fed the same
// executorPredicates (executor.go) that run() wires into queue.Config —
// so neither the gates nor their wording can drift between this command
// and a rejection at run time.
//
// The hasServices condition len(cfg.Services.Allow) > 0 is equivalent to
// the queue's Services != nil: run() populates queue.Config.Services
// exactly when Allow is non-empty. Likewise cfg.GitHub.ReceiptNotes != nil
// (issue #13) is equivalent to the queue's Config.ReceiptNotes: both mean
// "this daemon has a receipt-notes policy configured".
func crossCheck(cfg *config.Daemon, spec *config.CheckSpec) error {
	known, imageCapable := executorPredicates(cfg)
	if reason := queue.SpecRejectReason(spec, len(cfg.Services.Allow) > 0, known, imageCapable, cfg.GitHub.ReceiptNotes != nil); reason != "" {
		return errors.New(reason)
	}
	return nil
}
