package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// lockFileName is the fixed name of the advisory lock file every gauntlet
// daemon takes on its own -state directory.
const lockFileName = "gauntlet.lock"

// Lock is an exclusive, process-lifetime advisory flock on
// <stateDir>/gauntlet.lock. Startup previously assumed — but never
// enforced — that at most one gauntlet daemon ever runs against a given
// -state directory; a second process starting against the same state (an
// operator error, or an overlapping systemd restart) would otherwise
// unconditionally sweep (os.RemoveAll) the first, still-running daemon's
// in-flight trial/hook/scratch exports out from under it. AcquireLock turns
// that assumption into an enforced precondition: a second process fails
// fast, before it ever reaches a sweep, with a clear refusal.
//
// golang.org/x/sys/unix.Flock (not the stdlib syscall package) is used
// here purely because x/sys is already an indirect dependency of this
// module (go.mod) — no new dependency is added by importing its unix
// subpackage directly. unix.Flock is implemented for both darwin and linux,
// the two platforms this daemon ships on.
type Lock struct {
	f *os.File
}

// AcquireLock opens (creating if necessary) <stateDir>/gauntlet.lock and
// takes an exclusive, non-blocking flock on it. The returned Lock's file
// descriptor must be held open for the entire life of the process — only
// Close (or process exit, which closes every open fd) releases it, at
// which point a second daemon's own AcquireLock against the same stateDir
// can succeed.
//
// On EWOULDBLOCK (another live process already holds the lock), the
// returned error names the exact condition, so an operator restarting
// gauntlet against a still-running instance gets a loud, specific refusal
// instead of a sweep silently racing the live daemon.
func AcquireLock(stateDir string) (*Lock, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("lock: create state dir %s: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, lockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("lock: another gauntlet daemon holds %s; refusing to start", stateDir)
		}
		return nil, fmt.Errorf("lock: flock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Close releases the lock and closes its backing file descriptor, freeing
// stateDir for another daemon's AcquireLock to succeed. Safe (and intended)
// to be deferred for the process's full lifetime.
func (l *Lock) Close() error {
	// Explicit unlock before close keeps the release ordering unambiguous;
	// closing the last fd referring to this open file description would
	// release the flock on its own on darwin/linux, but there's no reason
	// to rely on that implicitly.
	_ = unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	return l.f.Close()
}
