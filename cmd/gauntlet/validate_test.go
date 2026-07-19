package main

// Tests for gauntlet validate: static validation only (no daemon, no
// network) — LoadDaemon/ParseChecks parity, cross-check gating, and the
// side-effect-free contract. Matches land_test.go/status_test.go's "no
// exec, no wall-clock sleeps" style: everything here is file I/O against
// t.TempDir() fixtures.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
)

// validDaemonKDL is the minimal daemon config LoadDaemon accepts (mirrors
// internal/config/config_test.go's TestLoadDaemon_Defaults fixture).
const validDaemonKDL = `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
`

// invalidDaemonKDL is missing committer.email — LoadDaemon's validate()
// rejects it with "committer: email must not be empty".
const invalidDaemonKDL = `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
}
target "main" branch="main"
`

const validChecksKDL = `
check "test" {
    command "go" "test" "./..."
}
`

// cycleChecksKDL declares an after-cycle between "a" and "b".
const cycleChecksKDL = `
check "a" {
    command "true"
    after "b"
}
check "b" {
    command "true"
    after "a"
}
`

// unknownAfterChecksKDL names an after prerequisite that doesn't exist.
const unknownAfterChecksKDL = `
check "a" {
    command "true"
    after "ghost"
}
`

// duplicateWorkspaceChecksKDL declares "workspace" twice, which
// rejectDuplicateSingletons (internal/config/checks.go) rejects even though
// kdl-go's own Unmarshal would silently take the last one.
const duplicateWorkspaceChecksKDL = `
workspace "isolated"
workspace "shared"
check "test" {
    command "true"
}
`

// writeFile writes contents to name under dir and returns the full path.
func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestRunValidate_NoFlagsIsUsageError(t *testing.T) {
	if err := runValidate(nil); err == nil {
		t.Fatal("runValidate(nil) = nil error, want a usage error naming -config/-checks")
	} else if !strings.Contains(err.Error(), "-config") || !strings.Contains(err.Error(), "-checks") {
		t.Errorf("runValidate(nil) error = %q, want it to name both -config and -checks", err.Error())
	}
}

func TestRunValidate_ConfigMatchesLoadDaemon(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid", func(t *testing.T) {
		path := writeFile(t, dir, "valid.kdl", validDaemonKDL)
		var out bytes.Buffer
		if err := runValidateTo(&out, []string{"-config", path}); err != nil {
			t.Fatalf("runValidateTo: %v", err)
		}
		if want := path + ": ok\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		path := writeFile(t, dir, "invalid.kdl", invalidDaemonKDL)

		_, loadErr := config.LoadDaemon(path)
		if loadErr == nil {
			t.Fatal("LoadDaemon accepted a config missing committer.email; fixture is wrong")
		}

		var out bytes.Buffer
		err := runValidateTo(&out, []string{"-config", path})
		if err == nil {
			t.Fatal("runValidateTo accepted an invalid config")
		}
		// Exact parity: the same file must produce the exact same error
		// LoadDaemon itself returns — validate must never re-implement
		// the check.
		if err.Error() != loadErr.Error() {
			t.Errorf("runValidateTo error = %q, want exactly LoadDaemon's %q", err.Error(), loadErr.Error())
		}
		if out.Len() != 0 {
			t.Errorf("stdout = %q, want nothing printed on failure", out.String())
		}
	})
}

func TestRunValidate_ChecksMatchesParseChecks(t *testing.T) {
	dir := t.TempDir()

	t.Run("valid", func(t *testing.T) {
		path := writeFile(t, dir, "valid.gauntlet.kdl", validChecksKDL)
		var out bytes.Buffer
		if err := runValidateTo(&out, []string{"-checks", path}); err != nil {
			t.Fatalf("runValidateTo: %v", err)
		}
		if want := path + ": ok\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
	})

	cases := []struct {
		name string
		kdl  string
	}{
		{"cycle", cycleChecksKDL},
		{"unknown after", unknownAfterChecksKDL},
		{"duplicate singleton", duplicateWorkspaceChecksKDL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeFile(t, dir, c.name+".gauntlet.kdl", c.kdl)

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, parseErr := config.ParseChecks(data)
			if parseErr == nil {
				t.Fatalf("ParseChecks accepted %q; fixture is wrong", c.name)
			}

			var out bytes.Buffer
			gotErr := runValidateTo(&out, []string{"-checks", path})
			if gotErr == nil {
				t.Fatal("runValidateTo accepted an invalid check spec")
			}
			if gotErr.Error() != parseErr.Error() {
				t.Errorf("runValidateTo error = %q, want exactly ParseChecks's %q", gotErr.Error(), parseErr.Error())
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want nothing printed on failure", out.String())
			}
		})
	}
}

func TestRunValidate_BothFilesSuccessReportsBothOK(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeFile(t, dir, "gauntlet.kdl", validDaemonKDL)
	checksPath := writeFile(t, dir, ".gauntlet.kdl", validChecksKDL)

	var out bytes.Buffer
	if err := runValidateTo(&out, []string{"-config", cfgPath, "-checks", checksPath}); err != nil {
		t.Fatalf("runValidateTo: %v", err)
	}
	want := cfgPath + ": ok\n" + checksPath + ": ok\n"
	if out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// TestRunValidate_CrossCheck covers the three cross-file rejections
// (unknown profile, image on a non-container profile, services required but
// not configured) and proves each offending spec passes in -checks-alone
// mode — cross-file properties genuinely require -config to be caught.
func TestRunValidate_CrossCheck(t *testing.T) {
	const daemonNoProfilesNoServices = `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "local"
`
	const checksUnknownProfile = `
check "test" {
    command "true"
    executor "ci"
}
`
	const checksImageOnLocalProfile = `
image "app" {
    command "true"
}
check "test" {
    command "true"
    image "app"
}
`
	const checksNeedsService = `
service "db" {
    image "postgres:16"
    port 5432
}
check "test" {
    command "true"
    needs "db"
}
`
	const checksNoReceipt = `
check "test" {
    command "true"
}
`
	const checksWithReceipt = `
check "test" {
    command "true"
}
receipt "deployment" {
    command "./ci/write-candidate-receipt"
}
`
	const daemonWithReceiptNotes = `
remote "https://github.com/acme/widgets.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "local"
github "acme/widgets" {
    receipt-notes {}
}
`

	cases := []struct {
		name       string
		daemonKDL  string // defaults to daemonNoProfilesNoServices when empty
		checksKDL  string
		wantErrSub string
	}{
		{
			name:       "unknown executor profile",
			checksKDL:  checksUnknownProfile,
			wantErrSub: `check "test" selects unknown executor profile "ci"`,
		},
		{
			name:       "image on non-container profile",
			checksKDL:  checksImageOnLocalProfile,
			wantErrSub: `check "test" runs candidate-built image "app" but its executor profile is not a container profile`,
		},
		{
			name:       "services required but not configured",
			checksKDL:  checksNeedsService,
			wantErrSub: "check spec declares services but this daemon has no services block",
		},
		{
			// issue #13: receipt-notes configured, spec declares no receipt.
			name:       "receipt-notes policy requires a receipt but spec declares none",
			daemonKDL:  daemonWithReceiptNotes,
			checksKDL:  checksNoReceipt,
			wantErrSub: "this daemon requires a receipt (receipt-notes is configured) but the check spec declares none",
		},
		{
			// issue #13: no receipt-notes policy, spec declares one anyway.
			name:       "spec declares a receipt but daemon has no receipt-notes policy",
			checksKDL:  checksWithReceipt,
			wantErrSub: `check spec declares receipt "deployment" but this daemon has no receipt-notes policy`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			daemonKDL := c.daemonKDL
			if daemonKDL == "" {
				daemonKDL = daemonNoProfilesNoServices
			}
			dir := t.TempDir()
			cfgPath := writeFile(t, dir, "gauntlet.kdl", daemonKDL)
			checksPath := writeFile(t, dir, ".gauntlet.kdl", c.checksKDL)

			// The same spec, alone, only checks intrinsic validity — the
			// cross-file property is invisible without -config. This also
			// proves repo-mode-alone accepts a spec that declares (or
			// omits) a receipt regardless of any daemon's policy: receipt
			// SYNTAX is self-checkable, receipt POLICY is not (docs/checks.md's
			// "Self-checking your spec" cross-file list).
			var aloneOut bytes.Buffer
			if err := runValidateTo(&aloneOut, []string{"-checks", checksPath}); err != nil {
				t.Fatalf("runValidateTo(-checks alone) = %v, want success (cross-file issues aren't checked without -config)", err)
			}

			var out bytes.Buffer
			err := runValidateTo(&out, []string{"-config", cfgPath, "-checks", checksPath})
			if err == nil {
				t.Fatal("runValidateTo(-config, -checks) accepted a spec that should fail cross-check")
			}
			if !strings.Contains(err.Error(), c.wantErrSub) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), c.wantErrSub)
			}
		})
	}
}

// TestRunValidate_CrossCheck_ReceiptPolicyAccepted is the positive
// counterpart to TestRunValidate_CrossCheck's two new receipt-policy
// cases: a spec WITH a receipt against a daemon WITH receipt-notes, and a
// spec with none against a daemon with none, both cross-check clean.
func TestRunValidate_CrossCheck_ReceiptPolicyAccepted(t *testing.T) {
	const daemonNoProfilesNoServices = `
remote "https://example.com/repo.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "local"
`
	const daemonWithReceiptNotes = `
remote "https://github.com/acme/widgets.git"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
executor "local"
github "acme/widgets" {
    receipt-notes {}
}
`
	const checksNoReceipt = `
check "test" {
    command "true"
}
`
	const checksWithReceipt = `
check "test" {
    command "true"
}
receipt "deployment" {
    command "./ci/write-candidate-receipt"
}
`

	cases := []struct {
		name      string
		daemonKDL string
		checksKDL string
	}{
		{"policy enabled, spec declares one", daemonWithReceiptNotes, checksWithReceipt},
		{"policy disabled, spec declares none", daemonNoProfilesNoServices, checksNoReceipt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			cfgPath := writeFile(t, dir, "gauntlet.kdl", c.daemonKDL)
			checksPath := writeFile(t, dir, ".gauntlet.kdl", c.checksKDL)

			var out bytes.Buffer
			if err := runValidateTo(&out, []string{"-config", cfgPath, "-checks", checksPath}); err != nil {
				t.Fatalf("runValidateTo(-config, -checks) = %v, want acceptance", err)
			}
		})
	}
}

func TestRunValidate_CreatesNoFiles(t *testing.T) {
	fixtureDir := t.TempDir()
	cfgPath := writeFile(t, fixtureDir, "gauntlet.kdl", validDaemonKDL)
	checksPath := writeFile(t, fixtureDir, ".gauntlet.kdl", validChecksKDL)

	cwd := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(orig); err != nil {
			t.Fatal(err)
		}
	}()

	var out bytes.Buffer
	if err := runValidateTo(&out, []string{"-config", cfgPath, "-checks", checksPath}); err != nil {
		t.Fatalf("runValidateTo: %v", err)
	}

	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("cwd has %d entries after runValidateTo, want 0 (no side-effect files): %v", len(entries), entries)
	}
}
