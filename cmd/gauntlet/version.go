// Version stamping (production packaging; see docs/deploy.md). `version`
// is baked in at build time via `-ldflags "-X main.version=..."` (see the
// Makefile's `build` target, which derives it from `git describe --always
// --dirty`); a plain `go build`/`go run` leaves it at "devel".
package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version is overridden at link time; see the package doc above.
var version = "devel"

// versionString renders the full `gauntlet -version` output: gauntlet's own
// version, the toolchain that built it, GOOS/GOARCH, and — when built with
// `go build` from a VCS checkout (as opposed to `go install` of a tagged
// module, which strips this) — the VCS revision debug.ReadBuildInfo
// recovers from the binary's embedded build info. The ldflags-stamped
// version and the VCS revision answer different questions (what was
// released vs. exactly what tree produced this binary) and can disagree
// (e.g. a binary built from a dirty tree); print both when both are known.
func versionString() string {
	s := fmt.Sprintf("gauntlet %s\n%s %s/%s", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return s
	}
	var revision string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return s
	}
	if modified {
		revision += "-dirty"
	}
	return fmt.Sprintf("%s\nvcs %s", s, revision)
}
