package main

import (
	"runtime"
	"strings"
	"testing"
)

// Subprocess-free: exercises versionString() directly rather than shelling
// out to a built binary (per the project testing style,
// docs/plans/phase23.md §5 — no exec in unit tests). `make build &&
// ./gauntlet -version` is the end-to-end check, done by hand, not here.

func TestVersionStringDefault(t *testing.T) {
	got := versionString()

	if !strings.Contains(got, "gauntlet "+version) {
		t.Errorf("versionString() = %q, want it to contain %q", got, "gauntlet "+version)
	}
	if !strings.Contains(got, runtime.Version()) {
		t.Errorf("versionString() = %q, want it to contain toolchain version %q", got, runtime.Version())
	}
	if !strings.Contains(got, runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("versionString() = %q, want it to contain %q", got, runtime.GOOS+"/"+runtime.GOARCH)
	}
}

func TestVersionStringOverride(t *testing.T) {
	old := version
	defer func() { version = old }()

	version = "v1.2.3"
	got := versionString()
	if !strings.HasPrefix(got, "gauntlet v1.2.3\n") {
		t.Errorf("versionString() = %q, want it to start with %q", got, "gauntlet v1.2.3\n")
	}
}

// vcs.revision presence/absence depends on how `go test` itself was invoked
// (module mode vs. a VCS checkout), so this only asserts the function
// doesn't panic and produces well-formed output either way — the "vcs "
// line's exact content isn't asserted, just that when debug.ReadBuildInfo
// succeeds the base fields are still present.
func TestVersionStringWellFormed(t *testing.T) {
	got := versionString()
	lines := strings.Split(got, "\n")
	if len(lines) < 2 {
		t.Fatalf("versionString() = %q, want at least 2 lines", got)
	}
	if lines[0] != "gauntlet "+version {
		t.Errorf("versionString() first line = %q, want %q", lines[0], "gauntlet "+version)
	}
}
