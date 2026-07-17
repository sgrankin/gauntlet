package gitx_test

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sgrankin/gauntlet/internal/gitx"
)

// fakeTokens hands out a scripted token sequence: Token returns the
// current one; Invalidate advances to the next when the rejected token is
// the current one (mirroring ghauth.App's guarded invalidation).
type fakeTokens struct {
	mu          sync.Mutex
	seq         []string
	i           int
	mints       int
	invalidated []string
}

func (f *fakeTokens) Token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mints++
	return f.seq[f.i], nil
}

func (f *fakeTokens) Invalidate(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated = append(f.invalidated, tok)
	if f.seq[f.i] == tok && f.i+1 < len(f.seq) {
		f.i++
	}
}

func (f *fakeTokens) stats() (mints int, invalidated []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mints, append([]string(nil), f.invalidated...)
}

// newUpstream builds a bare upstream repo with one commit on main and
// push enabled over http-backend.
func newUpstream(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(root, "init", "--bare", "-q", "-b", "main", upstream)
	run(root, "-C", upstream, "config", "http.receivepack", "true")
	work := filepath.Join(root, "work")
	run(root, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "-q", "-m", "seed")
	run(work, "push", "-q", upstream, "main")
	return upstream
}

// newAuthedRemote serves upstream over real smart HTTP (git http-backend
// as CGI), accepting exactly Basic x-access-token:<want()>; everything
// else is 401, or `code` when non-zero.
func newAuthedRemote(t *testing.T, upstream string, code int, want func() string) *httptest.Server {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git not found: %v", err)
	}
	backend := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Dir(upstream),
			"GIT_HTTP_EXPORT_ALL=1",
		},
		InheritEnv: []string{"PATH"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || pass != want() {
			if code != 0 {
				http.Error(w, "forbidden", code)
				return
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="gauntlet-test"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAuth_TokenSourceAuthenticatesFetchAndPush is the happy path
// end-to-end: a TokenSource-configured Repo fetches and CAS-pushes
// against a remote demanding Basic auth, with the token absent from the
// persistent git config and no askpass helper left behind.
func TestAuth_TokenSourceAuthenticatesFetchAndPush(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	ft := &fakeTokens{seq: []string{"ghs_FAKEA"}}
	srv := newAuthedRemote(t, upstream, 0, func() string { return "ghs_FAKEA" })

	// Point os.TempDir at a private dir so leftover askpass helpers are
	// detectable as "this dir is not empty afterwards".
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	local := filepath.Join(t.TempDir(), "local.git")
	remoteURL := srv.URL + "/upstream.git"
	repo, err := gitx.New(ctx, local, remoteURL, gitx.WithTokenSource(ft, strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	refs, err := repo.ListRefs(ctx)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	main := refs["refs/heads/main"]
	if main == "" {
		t.Fatalf("refs/heads/main missing after authenticated fetch: %v", refs)
	}

	if err := repo.CASUpdate(ctx, "refs/heads/feature", "", main); err != nil {
		t.Fatalf("CASUpdate: %v", err)
	}
	out, err := exec.Command("git", "-C", upstream, "rev-parse", "refs/heads/feature").Output()
	if err != nil || strings.TrimSpace(string(out)) != main {
		t.Fatalf("upstream feature ref = %q (%v), want %q", strings.TrimSpace(string(out)), err, main)
	}

	mints, invalidated := ft.stats()
	if len(invalidated) != 0 {
		t.Errorf("invalidated = %v, want none on the happy path", invalidated)
	}
	if mints == 0 {
		t.Error("token source never consulted")
	}

	// The token must not be persisted anywhere in the local repo...
	cfg, err := os.ReadFile(filepath.Join(local, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "ghs_FAKEA") {
		t.Errorf("token leaked into git config:\n%s", cfg)
	}
	if !strings.Contains(string(cfg), remoteURL) {
		t.Errorf("remote URL missing from config (rewritten?):\n%s", cfg)
	}
	// ...and every ephemeral askpass helper must be gone.
	ents, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "gauntlet-askpass-") {
			t.Errorf("askpass helper left behind: %s", e.Name())
		}
	}
}

// TestAuth_ExpiredTokenRetriesExactlyOnce: the remote rejects the first
// token (as GitHub does once an installation token expires); the Repo
// must invalidate it and retry the operation once with a fresh mint.
func TestAuth_ExpiredTokenRetriesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	ft := &fakeTokens{seq: []string{"ghs_FAKEOLD", "ghs_FAKENEW"}}
	srv := newAuthedRemote(t, upstream, 0, func() string { return "ghs_FAKENEW" })

	local := filepath.Join(t.TempDir(), "local.git")
	repo, err := gitx.New(ctx, local, srv.URL+"/upstream.git", gitx.WithTokenSource(ft, strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := repo.Fetch(ctx); err != nil {
		t.Fatalf("Fetch after expired-token retry: %v", err)
	}

	mints, invalidated := ft.stats()
	if len(invalidated) != 1 || invalidated[0] != "ghs_FAKEOLD" {
		t.Errorf("invalidated = %v, want exactly [ghs_FAKEOLD]", invalidated)
	}
	if mints != 2 {
		t.Errorf("mints = %d, want 2 (one per attempt)", mints)
	}
}

// TestAuth_PermissionDenialIsNotRetried: a 403 means the credential is
// valid but under-privileged — minting a fresh token cannot fix it, so
// there must be no invalidate-and-retry loop hiding the real error.
func TestAuth_PermissionDenialIsNotRetried(t *testing.T) {
	ctx := context.Background()
	upstream := newUpstream(t)
	ft := &fakeTokens{seq: []string{"ghs_FAKEA"}}
	srv := newAuthedRemote(t, upstream, http.StatusForbidden, func() string { return "never-matches" })

	local := filepath.Join(t.TempDir(), "local.git")
	repo, err := gitx.New(ctx, local, srv.URL+"/upstream.git", gitx.WithTokenSource(ft, strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = repo.Fetch(ctx)
	if err == nil {
		t.Fatal("Fetch = nil error against a 403 remote, want failure")
	}
	// The surfaced error carries git's stderr; the token must not be in
	// it (it never appears in the URL or argv, only the subprocess env).
	if strings.Contains(err.Error(), "ghs_FAKEA") {
		t.Errorf("token leaked into the fetch error: %v", err)
	}
	_, invalidated := ft.stats()
	if len(invalidated) != 0 {
		t.Errorf("invalidated = %v on a 403, want none (permission is not expiry)", invalidated)
	}
}
