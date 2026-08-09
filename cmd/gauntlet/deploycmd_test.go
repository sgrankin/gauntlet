package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/deploy"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

// `gauntlet deploy`/`gauntlet promote` are git porcelain, so they are tested
// against real git: a testutil.Remote bare repo standing in for the remote,
// and a real clone as the process's working directory (t.Chdir), exactly the
// two things the commands assume exist. There is no fake git layer here for
// the same reason the queue's harness runs against real bare repos — the
// behaviors under test (ls-remote resolution, --force-with-lease semantics,
// "the object must be local to push it") are git's, not ours.

// clone makes a working clone of r and returns its path.
func clone(t *testing.T, r *testutil.Remote) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "-q", r.Dir, dir).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v: %s", err, out)
	}
	return dir
}

func TestDeploy_CreatesDesiredRef(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	t.Chdir(clone(t, r))

	var out bytes.Buffer
	if err := deployEnv(&out, deployOptions{env: "prod", rev: "main", remote: "origin"}); err != nil {
		t.Fatalf("deployEnv: %v", err)
	}

	want := r.Ref("refs/heads/main")
	if got := r.Ref(deploy.DesiredRef("prod")); got != want {
		t.Fatalf("%s = %q, want %q", deploy.DesiredRef("prod"), got, want)
	}
	// A first deploy says so — the leased old value was "no ref at all",
	// which is exactly what distinguishes it from an advance.
	if !strings.Contains(out.String(), "(new)") || !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want it to mention (new) and %s", out.String(), want)
	}
}

// A branch -rev resolves against the REMOTE's tip, not the local clone's
// idea of it: the clone here is deliberately stale (cloned before the second
// seed) and never fetched by the test, so a resolution that read local refs
// would deploy the wrong revision — and the push would fail outright without
// the object-fetch step, since the stale clone doesn't have the new commit.
func TestDeploy_RevResolvesRemoteTip(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	stale := r.Ref("refs/heads/main")
	dir := clone(t, r)
	r.Seed("main", map[string]string{"f.txt": "2\n"})
	tip := r.Ref("refs/heads/main")
	if stale == tip {
		t.Fatal("test setup: the second seed did not move main")
	}
	t.Chdir(dir)

	if err := deployEnv(&bytes.Buffer{}, deployOptions{env: "dev", rev: "main", remote: "origin"}); err != nil {
		t.Fatalf("deployEnv: %v", err)
	}
	if got := r.Ref(deploy.DesiredRef("dev")); got != tip {
		t.Fatalf("%s = %q, want the remote tip %q (stale local tip is %q)", deploy.DesiredRef("dev"), got, tip, stale)
	}
}

// A full SHA is taken verbatim, including one no branch points at.
func TestDeploy_RevFullSHA(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	first := r.Ref("refs/heads/main")
	r.Seed("main", map[string]string{"f.txt": "2\n"})
	t.Chdir(clone(t, r))

	if err := deployEnv(&bytes.Buffer{}, deployOptions{env: "prod", rev: first, remote: "origin"}); err != nil {
		t.Fatalf("deployEnv: %v", err)
	}
	if got := r.Ref(deploy.DesiredRef("prod")); got != first {
		t.Fatalf("%s = %q, want %q", deploy.DesiredRef("prod"), got, first)
	}
}

// An annotated tag deploys the commit it points at, not the tag object: a
// deploy ref may only ever hold a revision.
func TestDeploy_RevAnnotatedTagPeels(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	commit := r.Ref("refs/heads/main")
	dir := clone(t, r)
	gitIn(t, dir, "config", "user.email", "a@example.com")
	gitIn(t, dir, "config", "user.name", "A")
	gitIn(t, dir, "tag", "-a", "-m", "release", "v1", commit)
	gitIn(t, dir, "push", "-q", "origin", "v1")
	t.Chdir(dir)

	if err := deployEnv(&bytes.Buffer{}, deployOptions{env: "prod", rev: "v1", remote: "origin"}); err != nil {
		t.Fatalf("deployEnv: %v", err)
	}
	if got := r.Ref(deploy.DesiredRef("prod")); got != commit {
		t.Fatalf("%s = %q, want the peeled commit %q", deploy.DesiredRef("prod"), got, commit)
	}
}

// A name matching both a branch and a tag is ambiguous, and ambiguity is an
// error rather than a guess.
func TestDeploy_RevAmbiguous(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	commit := r.Ref("refs/heads/main")
	dir := clone(t, r)
	gitIn(t, dir, "branch", "rel", commit)
	gitIn(t, dir, "tag", "rel", commit)
	gitIn(t, dir, "push", "-q", "origin", "refs/heads/rel:refs/heads/rel", "refs/tags/rel:refs/tags/rel")
	t.Chdir(dir)

	err := deployEnv(&bytes.Buffer{}, deployOptions{env: "prod", rev: "rel", remote: "origin"})
	if err == nil {
		t.Fatal("deployEnv succeeded on an ambiguous -rev, want an error")
	}
	if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "refs/tags/rel") {
		t.Errorf("error = %v, want it to name the ambiguity and the matching refs", err)
	}
	if got := r.Ref(deploy.DesiredRef("prod")); got != "" {
		t.Errorf("%s = %q, want nothing pushed", deploy.DesiredRef("prod"), got)
	}
}

func TestDeploy_RevNoMatch(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	t.Chdir(clone(t, r))

	err := deployEnv(&bytes.Buffer{}, deployOptions{env: "prod", rev: "nope", remote: "origin"})
	if err == nil || !strings.Contains(err.Error(), "no branch or tag") {
		t.Fatalf("error = %v, want a clear no-match error", err)
	}
}

// -from-env reads the SOURCE environment's observed ref — which a normal
// clone never fetches, and whose commit this clone therefore doesn't have
// until the command fetches it.
func TestDeploy_FromEnvResolvesObservedRef(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	dir := clone(t, r)
	r.Seed("main", map[string]string{"f.txt": "2\n"})
	deployed := r.Ref("refs/heads/main")
	r.SetRef(deploy.ObservedRef("dev"), deployed)
	t.Chdir(dir)

	var out bytes.Buffer
	if err := deployEnv(&out, deployOptions{env: "prod", fromEnv: "dev", remote: "origin"}); err != nil {
		t.Fatalf("deployEnv: %v", err)
	}
	if got := r.Ref(deploy.DesiredRef("prod")); got != deployed {
		t.Fatalf("%s = %q, want dev's observed %q", deploy.DesiredRef("prod"), got, deployed)
	}
	if !strings.Contains(out.String(), deploy.ObservedRef("dev")) {
		t.Errorf("output = %q, want it to name the source ref it promoted from", out.String())
	}
}

// Promoting from an environment that has never finished a deploy is a clear
// error naming the missing observed ref, not a mysterious push failure.
func TestDeploy_FromEnvNeverDeployed(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	t.Chdir(clone(t, r))

	err := deployEnv(&bytes.Buffer{}, deployOptions{env: "prod", fromEnv: "dev", remote: "origin"})
	if err == nil {
		t.Fatal("deployEnv succeeded promoting from a never-deployed environment, want an error")
	}
	if !strings.Contains(err.Error(), deploy.ObservedRef("dev")) || !strings.Contains(err.Error(), "never finished a deploy") {
		t.Errorf("error = %v, want it to name %s and say the environment never finished a deploy", err, deploy.ObservedRef("dev"))
	}
	if got := r.Ref(deploy.DesiredRef("prod")); got != "" {
		t.Errorf("%s = %q, want nothing pushed", deploy.DesiredRef("prod"), got)
	}
}

// Re-pushing the value the ref already holds is reported as the no-op it is,
// rather than as a successful deploy that silently did nothing (the daemon
// is level-triggered: desired == observed means no graph runs).
func TestDeploy_AlreadyAtRevision(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	t.Chdir(clone(t, r))

	if err := deployEnv(&bytes.Buffer{}, deployOptions{env: "prod", rev: "main", remote: "origin"}); err != nil {
		t.Fatalf("first deployEnv: %v", err)
	}
	var out bytes.Buffer
	if err := deployEnv(&out, deployOptions{env: "prod", rev: "main", remote: "origin"}); err != nil {
		t.Fatalf("second deployEnv: %v", err)
	}
	if !strings.Contains(out.String(), "already at") {
		t.Errorf("output = %q, want it to report the ref was already there", out.String())
	}
}

// The lease is what makes two concurrent deployers safe: a push whose leased
// old value no longer matches loses, loudly, and is told to re-run. Staged
// directly against casPushDeployRef because the race it models — the ref
// moving between the ls-remote read and the push — is precisely the window
// the lease closes.
func TestDeploy_LeaseFailureOnConcurrentMove(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	first := r.Ref("refs/heads/main")
	r.Seed("main", map[string]string{"f.txt": "2\n"})
	second := r.Ref("refs/heads/main")
	r.Seed("main", map[string]string{"f.txt": "3\n"})
	third := r.Ref("refs/heads/main")
	t.Chdir(clone(t, r))

	// What this deployer read a moment ago...
	ref := deploy.DesiredRef("prod")
	r.SetRef(ref, first)
	// ...and what somebody else pushed in the meantime.
	r.SetRef(ref, second)

	// This deployer now tries to put its own revision there under the lease
	// it read: a different value, so git genuinely attempts the update and
	// the lease is what stops it.
	err := casPushDeployRef("origin", ref, first, third)
	if err == nil {
		t.Fatal("casPushDeployRef succeeded against a stale lease, want an error")
	}
	if !strings.Contains(err.Error(), "re-run") {
		t.Errorf("error = %v, want it to tell the operator to re-run", err)
	}
	if got := r.Ref(ref); got != second {
		t.Errorf("%s = %q, want the concurrent writer's value %q left intact", ref, got, second)
	}
}

// promote -from dev -to prod is deploy -env prod -from-env dev, end to end.
func TestPromote_EndToEnd(t *testing.T) {
	r := testutil.NewRemote(t)
	r.Seed("main", map[string]string{"f.txt": "1\n"})
	deployed := r.Ref("refs/heads/main")
	r.SetRef(deploy.ObservedRef("dev"), deployed)
	t.Chdir(clone(t, r))

	o, err := parsePromoteFlags([]string{"-from", "dev", "-to", "prod"})
	if err != nil {
		t.Fatalf("parsePromoteFlags: %v", err)
	}
	if o.env != "prod" || o.fromEnv != "dev" || o.remote != "origin" {
		t.Fatalf("parsePromoteFlags = %+v, want env=prod from-env=dev remote=origin", o)
	}
	if err := deployEnv(&bytes.Buffer{}, o); err != nil {
		t.Fatalf("deployEnv: %v", err)
	}
	if got := r.Ref(deploy.DesiredRef("prod")); got != deployed {
		t.Fatalf("%s = %q, want %q", deploy.DesiredRef("prod"), got, deployed)
	}
}

func TestParseDeployFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    deployOptions
		wantErr []string // substrings the error must contain; nil ⇒ no error
	}{
		{
			name: "rev",
			args: []string{"-env", "prod", "-rev", "main"},
			want: deployOptions{env: "prod", rev: "main", remote: "origin"},
		},
		{
			name: "from-env with an explicit remote",
			args: []string{"-env", "prod", "-from-env", "dev", "-remote", "upstream"},
			want: deployOptions{env: "prod", fromEnv: "dev", remote: "upstream"},
		},
		{
			name:    "no env",
			args:    []string{"-rev", "main"},
			wantErr: []string{"-env is required"},
		},
		{
			name:    "both revision forms",
			args:    []string{"-env", "prod", "-rev", "main", "-from-env", "dev"},
			wantErr: []string{"mutually exclusive"},
		},
		{
			// The design doc's `gauntlet deploy -env prod` sketch: no daemon
			// round-trip means no way to read the configured source, so the
			// error has to name both ways to say what to deploy.
			name:    "neither revision form",
			args:    []string{"-env", "prod"},
			wantErr: []string{"-rev", "-from-env", "cannot read the environment's configured source"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseDeployFlags(c.args)
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("parseDeployFlags(%v) = %v, want no error", c.args, err)
				}
				if got != c.want {
					t.Fatalf("parseDeployFlags(%v) = %+v, want %+v", c.args, got, c.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("parseDeployFlags(%v) = %+v, want an error", c.args, got)
			}
			for _, want := range c.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

func TestParsePromoteFlags_Missing(t *testing.T) {
	for _, c := range []struct {
		name, want string
		args       []string
	}{
		{"no from", "-from is required", []string{"-to", "prod"}},
		{"no to", "-to is required", []string{"-from", "dev"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parsePromoteFlags(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("parsePromoteFlags(%v) = %v, want %q", c.args, err, c.want)
			}
		})
	}
}

// gitIn runs git in dir, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git -C %s %s: %v: %s", dir, strings.Join(args, " "), err, out)
	}
}
