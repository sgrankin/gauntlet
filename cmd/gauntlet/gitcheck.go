// The daemon's entire trial-merge mechanism rests on `git merge-tree
// --write-tree`, which only exists from git 2.38 onward. Older/missing git
// doesn't fail cleanly at first use — it fails confusingly, deep inside
// gitx, well after the daemon has already started logging as if everything
// were fine. Probing once at startup turns that into one loud, named error
// before anything else runs.
package main

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// minGitMajor and minGitMinor are the minimum supported git version
// (README: "Requires git 2.38 or newer (`git merge-tree --write-tree`)").
const (
	minGitMajor = 2
	minGitMinor = 38
)

// checkGitVersion probes `git --version` and fails loudly if git is missing,
// its version output is unparseable, or the version is below 2.38.
func checkGitVersion() error {
	out, err := exec.Command("git", "--version").Output()
	if err != nil {
		return fmt.Errorf("git --version failed (is git installed and on $PATH?): %w; gauntlet requires git %d.%d or newer (git merge-tree --write-tree)", err, minGitMajor, minGitMinor)
	}
	major, minor, ok := parseGitVersion(string(out))
	if !ok {
		return fmt.Errorf("git --version produced unparseable output %q; gauntlet requires git %d.%d or newer (git merge-tree --write-tree)", strings.TrimSpace(string(out)), minGitMajor, minGitMinor)
	}
	if major < minGitMajor || (major == minGitMajor && minor < minGitMinor) {
		return fmt.Errorf("git %d.%d found, but gauntlet requires git %d.%d or newer (git merge-tree --write-tree)", major, minor, minGitMajor, minGitMinor)
	}
	return nil
}

// gitVersionRE matches the first "<major>.<minor>" in `git --version`'s
// output, e.g. "git version 2.55.0" or "git version 2.39.2 (Apple
// Git-143)".
var gitVersionRE = regexp.MustCompile(`(\d+)\.(\d+)`)

func parseGitVersion(s string) (major, minor int, ok bool) {
	m := gitVersionRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	major, errM := strconv.Atoi(m[1])
	minor, errN := strconv.Atoi(m[2])
	if errM != nil || errN != nil {
		return 0, 0, false
	}
	return major, minor, true
}
