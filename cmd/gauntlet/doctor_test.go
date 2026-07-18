package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/ghauth/ghauthtest"
	"github.com/sgrankin/gauntlet/internal/history"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// --- git --------------------------------------------------------------------

func TestProbeGit_PassAndFail(t *testing.T) {
	writeFakeGitVersion := func(t *testing.T, version string) {
		t.Helper()
		dir := t.TempDir()
		script := "#!/bin/sh\necho 'git version " + version + "'\n"
		if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
	}

	t.Run("current", func(t *testing.T) {
		writeFakeGitVersion(t, "2.55.0")
		res := probeGit(t.Context())
		if res.status != statusPass {
			t.Fatalf("status = %v, detail = %q, want PASS", res.status, res.detail)
		}
	})

	t.Run("old", func(t *testing.T) {
		writeFakeGitVersion(t, "2.30.0")
		res := probeGit(t.Context())
		if res.status != statusFail {
			t.Fatalf("status = %v, want FAIL", res.status)
		}
		if res.remedy == "" {
			t.Error("FAIL with no remedy")
		}
		if !strings.Contains(res.remedy, "2.38") {
			t.Errorf("remedy = %q, want it to name the required version", res.remedy)
		}
	})
}

// --- state ------------------------------------------------------------------

func TestProbeState_Writable(t *testing.T) {
	dir := t.TempDir()
	if res := probeState(dir); res.status != statusPass {
		t.Fatalf("existing writable dir: status = %v, detail = %q", res.status, res.detail)
	}

	fresh := filepath.Join(dir, "not-yet-created")
	if res := probeState(fresh); res.status != statusPass {
		t.Fatalf("creatable dir: status = %v, detail = %q", res.status, res.detail)
	}
}

func TestProbeState_ReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	res := probeState(dir)
	if res.status != statusFail {
		t.Fatalf("status = %v, detail = %q, want FAIL", res.status, res.detail)
	}
	if res.remedy == "" {
		t.Error("FAIL with no remedy")
	}
}

func TestProbeState_Empty(t *testing.T) {
	res := probeState("")
	if res.status != statusFail {
		t.Fatalf("status = %v, want FAIL for an empty -state", res.status)
	}
}

// --- history ------------------------------------------------------------------

// writeHistoryDB creates a minimal sqlite file at path stamped with the
// given PRAGMA user_version — ReadSchemaVersion only ever reads that
// pragma, so no real schema.sql table needs to exist for these tests.
func writeHistoryDB(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(version)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}

func TestProbeHistory_Absent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	res := probeHistory(path)
	if res.status != statusPass {
		t.Fatalf("status = %v, detail = %q, want PASS (not yet created is fine)", res.status, res.detail)
	}
}

func TestProbeHistory_Current(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	writeHistoryDB(t, path, history.SchemaVersion)
	res := probeHistory(path)
	if res.status != statusPass {
		t.Fatalf("status = %v, detail = %q, want PASS", res.status, res.detail)
	}
}

func TestProbeHistory_Older(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	writeHistoryDB(t, path, history.SchemaVersion-1)
	res := probeHistory(path)
	if res.status != statusPass {
		t.Fatalf("status = %v, detail = %q, want PASS (the daemon migrates in place)", res.status, res.detail)
	}
}

func TestProbeHistory_Newer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	writeHistoryDB(t, path, history.SchemaVersion+1)
	res := probeHistory(path)
	if res.status != statusFail {
		t.Fatalf("status = %v, detail = %q, want FAIL (this binary is older than the writer)", res.status, res.detail)
	}
	if !strings.Contains(res.remedy, "older than the daemon that wrote it") {
		t.Errorf("remedy = %q, want the documented phrasing", res.remedy)
	}
}

// --- auth: key perms ----------------------------------------------------------

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

func writeKeyFile(t *testing.T, key *rsa.PrivateKey, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.pem")
	der := x509.MarshalPKCS1PrivateKey(key)
	buf := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, buf, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProbeAuthKeyPerms(t *testing.T) {
	key := testRSAKey(t)

	t.Run("owner-only", func(t *testing.T) {
		path := writeKeyFile(t, key, 0o600)
		cfg := &config.Daemon{GitHub: config.GitHub{Auth: &config.GitHubAuth{PrivateKeyFile: path}}}
		res := probeAuthKeyPerms(cfg)
		if res.status != statusPass {
			t.Fatalf("status = %v, detail = %q, want PASS", res.status, res.detail)
		}
	})

	t.Run("group-readable", func(t *testing.T) {
		path := writeKeyFile(t, key, 0o644)
		cfg := &config.Daemon{GitHub: config.GitHub{Auth: &config.GitHubAuth{PrivateKeyFile: path}}}
		res := probeAuthKeyPerms(cfg)
		if res.status != statusFail {
			t.Fatalf("status = %v, detail = %q, want FAIL", res.status, res.detail)
		}
		if !strings.Contains(res.remedy, "chmod") {
			t.Errorf("remedy = %q, want a chmod remedy", res.remedy)
		}
	})
}

// --- auth: TMPDIR exec ----------------------------------------------------------

func TestDoProbeTMPDirExec(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		res := doProbeTMPDirExec(t.Context(), "#!/bin/sh\n")
		if res.status != statusPass {
			t.Fatalf("status = %v, detail = %q, want PASS", res.status, res.detail)
		}
	})

	t.Run("simulated failure", func(t *testing.T) {
		// A shebang naming an interpreter that doesn't exist fails exec the
		// same way a real noexec-mounted TMPDIR would (exec.Run returning
		// an error), deterministically and without needing real mount
		// privileges in the test sandbox.
		res := doProbeTMPDirExec(t.Context(), "#!/nonexistent-interpreter-gauntlet-doctor-test\n")
		if res.status != statusFail {
			t.Fatalf("status = %v, detail = %q, want FAIL", res.status, res.detail)
		}
		if !strings.Contains(res.remedy, "noexec") {
			t.Errorf("remedy = %q, want it to mention noexec", res.remedy)
		}
	})
}

// --- auth: token mint -----------------------------------------------------------

func TestProbeAuthMint_Success(t *testing.T) {
	key := testRSAKey(t)
	keyPath := writeKeyFile(t, key, 0o600)
	issuer := ghauthtest.New(t, 12345, 67890, key)

	cfg := &config.Daemon{
		GitHub: config.GitHub{
			APIURL: issuer.URL(),
			Auth: &config.GitHubAuth{
				Mode: "app", AppID: 12345, InstallationID: 67890,
				PrivateKeyFile: keyPath,
			},
		},
	}

	app, appErr := buildAppTokens(cfg)
	res := probeAuthMint(t.Context(), app, appErr)
	if res.status != statusPass {
		t.Fatalf("status = %v, detail = %q, want PASS", res.status, res.detail)
	}
	if issuer.Mints() != 1 {
		t.Errorf("issuer.Mints() = %d, want 1", issuer.Mints())
	}
	// The minted secret must never appear in the probe's own output.
	if strings.Contains(res.detail+res.remedy, "ghs_FAKEMINT") {
		t.Errorf("probe output leaked the minted token: detail=%q remedy=%q", res.detail, res.remedy)
	}
}

func TestProbeAuthMint_Failure(t *testing.T) {
	key := testRSAKey(t)
	keyPath := writeKeyFile(t, key, 0o600)
	issuer := ghauthtest.New(t, 12345, 67890, key)
	issuer.SetFail(true)

	cfg := &config.Daemon{
		GitHub: config.GitHub{
			APIURL: issuer.URL(),
			Auth: &config.GitHubAuth{
				Mode: "app", AppID: 12345, InstallationID: 67890,
				PrivateKeyFile: keyPath,
			},
		},
	}

	app, appErr := buildAppTokens(cfg)
	res := probeAuthMint(t.Context(), app, appErr)
	if res.status != statusFail {
		t.Fatalf("status = %v, detail = %q, want FAIL", res.status, res.detail)
	}
	// The failure must name the step ("mint") so it's identifiable in
	// output alongside auth-key-perms/auth-tmpdir-exec.
	if !strings.HasPrefix(res.detail, "mint:") {
		t.Errorf("detail = %q, want it to name the mint step", res.detail)
	}
	if strings.Contains(res.detail+res.remedy, "ghs_FAKEMINT") {
		t.Errorf("probe output leaked a token on failure: detail=%q remedy=%q", res.detail, res.remedy)
	}
}

func TestProbeAuthTokenEnv(t *testing.T) {
	cfg := &config.Daemon{GitHub: config.GitHub{TokenEnv: "GAUNTLET_DOCTOR_TEST_TOKEN"}}

	t.Run("unset", func(t *testing.T) {
		t.Setenv("GAUNTLET_DOCTOR_TEST_TOKEN", "")
		os.Unsetenv("GAUNTLET_DOCTOR_TEST_TOKEN")
		res := probeAuthTokenEnv(cfg)
		if res.status != statusFail {
			t.Fatalf("status = %v, want FAIL", res.status)
		}
	})

	t.Run("set", func(t *testing.T) {
		t.Setenv("GAUNTLET_DOCTOR_TEST_TOKEN", "ghp_fake")
		res := probeAuthTokenEnv(cfg)
		if res.status != statusPass {
			t.Fatalf("status = %v, want PASS", res.status)
		}
	})
}

// --- remote -----------------------------------------------------------------

func TestProbeRemote_Reachable(t *testing.T) {
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f": "1"})

	cfg := &config.Daemon{Remote: remote.Dir}
	res := probeRemote(t.Context(), cfg, nil, nil)
	if res.status != statusPass {
		t.Fatalf("status = %v, detail = %q, want PASS", res.status, res.detail)
	}
}

func TestProbeRemote_Unreachable(t *testing.T) {
	cfg := &config.Daemon{Remote: filepath.Join(t.TempDir(), "does-not-exist.git")}
	res := probeRemote(t.Context(), cfg, nil, nil)
	if res.status != statusFail {
		t.Fatalf("status = %v, detail = %q, want FAIL", res.status, res.detail)
	}
	if res.remedy == "" {
		t.Error("FAIL with no remedy")
	}
}

// TestProbeRemote_Timeout proves the remote probe never hangs: a listener
// that accepts a connection and then stalls forever (never speaks the git
// protocol) must still return within the injected timeout, not the real
// ~10s default — no wall-clock sleep, a short ctx deadline instead.
func TestProbeRemote_Timeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and stall: never write a byte back, never close.
			t.Cleanup(func() { conn.Close() })
		}
	}()

	cfg := &config.Daemon{Remote: "git://" + ln.Addr().String() + "/repo.git"}

	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	res := probeRemote(ctx, cfg, nil, nil)
	elapsed := time.Since(start)

	if res.status != statusFail {
		t.Fatalf("status = %v, detail = %q, want FAIL", res.status, res.detail)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("probeRemote took %s, want it bounded by the injected ~500ms timeout (no hang)", elapsed)
	}
}

// --- executors ----------------------------------------------------------------

func TestContainerProfilesAndRuntimeUsages(t *testing.T) {
	cfg := &config.Daemon{
		Executor: config.Executor{Kind: "container", Runtime: "docker", Image: "img-default"},
		Profiles: []config.Executor{
			{Name: "ci", Kind: "container", Runtime: "docker", Image: "img-ci"},
			{Name: "gpu", Kind: "container", Runtime: "podman", Image: "img-gpu"},
			{Name: "local-profile", Kind: "local"},
		},
	}
	profiles := containerProfiles(cfg)
	if len(profiles) != 3 {
		t.Fatalf("containerProfiles = %+v, want 3 (default, ci, gpu — local-profile excluded)", profiles)
	}
	if profiles[0].label != "default" || profiles[1].label != "ci" || profiles[2].label != "gpu" {
		t.Fatalf("containerProfiles order/labels = %+v", profiles)
	}

	usages := runtimeUsages(profiles)
	if len(usages) != 2 {
		t.Fatalf("runtimeUsages = %+v, want 2 distinct runtimes", usages)
	}
	if usages[0].runtime != "docker" || len(usages[0].profiles) != 2 {
		t.Fatalf("docker usage = %+v, want profiles [default ci]", usages[0])
	}
	if usages[1].runtime != "podman" || len(usages[1].profiles) != 1 {
		t.Fatalf("podman usage = %+v, want profiles [gpu]", usages[1])
	}
}

func TestContainerProfiles_None(t *testing.T) {
	cfg := &config.Daemon{Executor: config.Executor{Kind: "local"}}
	if profiles := containerProfiles(cfg); len(profiles) != 0 {
		t.Fatalf("containerProfiles = %+v, want none for an all-local config", profiles)
	}
	env := &doctorEnv{cfg: cfg, timeout: time.Second}
	for _, p := range buildProbes(env) {
		if strings.HasPrefix(p.name, "executor-") {
			t.Errorf("buildProbes included %q with no container profiles configured", p.name)
		}
	}
}

func TestProbeExecutorRuntime(t *testing.T) {
	t.Run("absent binary names the profiles", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir()) // nothing resolves
		u := runtimeUsage{runtime: "docker", profiles: []string{"default", "ci"}}
		res := probeExecutorRuntime(t.Context(), u)
		if res.status != statusFail {
			t.Fatalf("status = %v, detail = %q, want FAIL", res.status, res.detail)
		}
		if !strings.Contains(res.detail, "default") || !strings.Contains(res.detail, "ci") {
			t.Errorf("detail = %q, want it to name both profiles", res.detail)
		}
	})

	t.Run("present and reachable", func(t *testing.T) {
		dir := t.TempDir()
		script := "#!/bin/sh\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		u := runtimeUsage{runtime: "docker", profiles: []string{"default"}}
		res := probeExecutorRuntime(t.Context(), u)
		if res.status != statusPass {
			t.Fatalf("status = %v, detail = %q, want PASS", res.status, res.detail)
		}
	})
}

func TestProbeImagePresent(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(t.TempDir(), "invocations.log")
		script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		res := probeImagePresent(t.Context(), "docker", "example.com/img:tag")
		if res.status != statusPass {
			t.Fatalf("status = %v, detail = %q, want PASS", res.status, res.detail)
		}
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "pull") {
			t.Errorf("probeImagePresent must never pull; invocations = %q", data)
		}
	})

	t.Run("absent warns, never fails, never pulls", func(t *testing.T) {
		dir := t.TempDir()
		logPath := filepath.Join(t.TempDir(), "invocations.log")
		script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\nexit 1\n"
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		res := probeImagePresent(t.Context(), "docker", "example.com/missing:tag")
		if res.status != statusWarn {
			t.Fatalf("status = %v, detail = %q, want WARN (never FAIL)", res.status, res.detail)
		}
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "pull") {
			t.Errorf("probeImagePresent must never pull; invocations = %q", data)
		}
	})
}

// --- endpoint -----------------------------------------------------------------

func TestProbeEndpoint_FreeAndOccupied(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	t.Run("occupied warns", func(t *testing.T) {
		cfg := &config.Daemon{Dashboard: config.Dashboard{Bind: addr}}
		res := probeEndpoint(cfg)
		if res.status != statusWarn {
			t.Fatalf("status = %v, detail = %q, want WARN", res.status, res.detail)
		}
	})

	ln.Close()

	t.Run("free passes", func(t *testing.T) {
		cfg := &config.Daemon{Dashboard: config.Dashboard{Bind: addr}}
		res := probeEndpoint(cfg)
		if res.status != statusPass {
			t.Fatalf("status = %v, detail = %q, want PASS", res.status, res.detail)
		}
	})
}

// --- redaction ----------------------------------------------------------------

func TestRedactCreds(t *testing.T) {
	in := "https://x-access-token:ghp_SUPERSECRET@github.com/acme/widgets.git"
	out := redactCreds(in)
	if strings.Contains(out, "ghp_SUPERSECRET") {
		t.Fatalf("redactCreds(%q) = %q, still leaks the credential", in, out)
	}
	if !strings.Contains(out, "github.com/acme/widgets.git") {
		t.Fatalf("redactCreds(%q) = %q, should keep the host/path", in, out)
	}
}

// --- runDoctorTo: end-to-end wiring, probe independence, exit codes -----------

// minimalConfigPath writes a daemon config exercising exactly one target and
// (optionally) a dashboard bind, returning its path.
func minimalConfigPath(t *testing.T, remote, dashboardBind string) string {
	t.Helper()
	body := `
remote "` + remote + `"
committer {
    name "Gauntlet"
    email "gauntlet@example.com"
}
target "main" branch="main"
`
	if dashboardBind != "" {
		body += "dashboard \"" + dashboardBind + "\"\n"
	}
	path := filepath.Join(t.TempDir(), "gauntlet.kdl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunDoctorTo_ConfigLoadShortCircuits(t *testing.T) {
	badPath := filepath.Join(t.TempDir(), "bad.kdl")
	if err := os.WriteFile(badPath, []byte("this is not valid kdl {{{"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := runDoctorTo(&buf, []string{"-config", badPath})
	if !errors.Is(err, errDoctorFailed) {
		t.Fatalf("err = %v, want errDoctorFailed", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "FAIL") || !strings.Contains(out, "config") {
		t.Fatalf("output = %q, want it to start with a FAIL config line", out)
	}
	// Config-load failure short-circuits: no OTHER probe's PASS/FAIL/WARN
	// line follows it (the error itself may legitimately span several
	// lines — kdl-go's parse errors quote the offending source snippet, so
	// a bare substring check would false-positive on the parser's own
	// internal state names; match a real probe line's start instead).
	for _, other := range []string{"git", "state", "remote", "endpoint"} {
		re := regexp.MustCompile(`(?m)^(PASS|WARN|FAIL)\s+` + other + `\b`)
		if re.MatchString(out) {
			t.Errorf("output contains a %q probe line; config-load failure must short-circuit everything else:\n%s", other, out)
		}
	}
}

// TestRunDoctorTo_ProbeIndependence proves an early FAIL (bad git) doesn't
// stop later probes (the endpoint check) from running and being reported.
func TestRunDoctorTo_ProbeIndependence(t *testing.T) {
	// Build the fixture (a real remote, a real config) with the REAL git
	// still on $PATH — only then does $PATH get replaced by the fake,
	// too-old one doctor itself will see, so testutil's own git plumbing
	// isn't affected by the fake.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // free again; the endpoint probe should find it free

	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f": "1"})
	cfgPath := minimalConfigPath(t, remote.Dir, addr)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\necho 'git version 2.20.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	var buf bytes.Buffer
	err = runDoctorTo(&buf, []string{"-config", cfgPath, "-state", filepath.Join(t.TempDir(), "state")})
	if !errors.Is(err, errDoctorFailed) {
		t.Fatalf("err = %v, want errDoctorFailed (git probe FAILed)", err)
	}
	out := buf.String()
	if !strings.Contains(out, "FAIL git") {
		t.Errorf("output missing FAIL git line:\n%s", out)
	}
	if !strings.Contains(out, "endpoint") {
		t.Errorf("output missing the endpoint probe entirely — an early FAIL must not stop later probes:\n%s", out)
	}
	if !strings.Contains(out, "PASS remote") && !strings.Contains(out, "FAIL remote") {
		// Whatever its own verdict, the remote probe must still have run.
		t.Errorf("output missing the remote probe entirely:\n%s", out)
	}
}

// TestRunDoctorTo_WarnOnlyExitsZero proves a WARN-only run (endpoint bind
// already in use, everything else clean) still exits 0 — only FAIL fails
// doctor.
func TestRunDoctorTo_WarnOnlyExitsZero(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f": "1"})
	cfgPath := minimalConfigPath(t, remote.Dir, addr)

	var buf bytes.Buffer
	err = runDoctorTo(&buf, []string{"-config", cfgPath, "-state", filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatalf("err = %v, want nil (WARN alone must not fail doctor); output:\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "WARN endpoint") {
		t.Errorf("output missing the expected WARN endpoint line:\n%s", buf.String())
	}
}

func TestRunDoctorTo_AllPass(t *testing.T) {
	remote := testutil.NewRemote(t)
	remote.Seed("main", map[string]string{"f": "1"})
	cfgPath := minimalConfigPath(t, remote.Dir, "")

	var buf bytes.Buffer
	err := runDoctorTo(&buf, []string{"-config", cfgPath, "-state", filepath.Join(t.TempDir(), "state")})
	if err != nil {
		t.Fatalf("err = %v, want nil; output:\n%s", err, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"PASS config", "PASS git", "PASS state", "PASS remote"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunDoctorTo_MissingConfigFlag(t *testing.T) {
	var buf bytes.Buffer
	err := runDoctorTo(&buf, nil)
	if err == nil || errors.Is(err, errDoctorFailed) {
		t.Fatalf("err = %v, want a plain usage error (no -config)", err)
	}
}
