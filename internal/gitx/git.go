// Package gitx implements core.GitRepo by shelling out to the git CLI
// against a local bare repository. It is gauntlet's entire VCS surface:
// plumbing only, no working copy, no checkout. This is the only package
// that runs git.
package gitx

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
)

// Repo implements core.GitRepo against a local bare repository whose
// "origin" remote is the configured remote URL.
type Repo struct {
	dir string // bare repo path (--git-dir)

	// tokens/authHost, when set (WithTokenSource), authenticate every
	// remote operation via an ephemeral askpass helper — see auth.go.
	tokens   TokenSource
	authHost string
}

var _ core.GitRepo = (*Repo)(nil)

// New opens the bare repository at dir, initializing it (git init --bare)
// if dir does not already exist. Either way, origin is set (or updated) to
// remoteURL and configured with a fetch refspec that maps the remote's
// refs/heads/* into this repo's refs/remotes/origin/* — the fixed point
// Fetch and ListRefs rely on. It does not fetch; callers should Fetch
// before relying on any ref state.
func New(ctx context.Context, dir, remoteURL string, opts ...Option) (*Repo, error) {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("gitx: stat %s: %w", dir, err)
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return nil, fmt.Errorf("gitx: mkdir %s: %w", filepath.Dir(dir), err)
		}
		if _, err := runGit(ctx, "", nil, "init", "--bare", "-q", dir); err != nil {
			return nil, fmt.Errorf("gitx: init %s: %w", dir, err)
		}
	}
	r := &Repo{dir: dir}
	for _, opt := range opts {
		opt(r)
	}
	if _, err := r.run(ctx, "remote", "get-url", "origin"); err != nil {
		if _, err := r.run(ctx, "remote", "add", "origin", remoteURL); err != nil {
			return nil, fmt.Errorf("gitx: add origin: %w", err)
		}
	} else {
		if _, err := r.run(ctx, "remote", "set-url", "origin", remoteURL); err != nil {
			return nil, fmt.Errorf("gitx: set origin url: %w", err)
		}
	}
	if _, err := r.run(ctx, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return nil, fmt.Errorf("gitx: configure fetch refspec: %w", err)
	}
	return r, nil
}

// Fetch updates refs/remotes/origin/* from the remote, pruning any that
// vanished there. This is the tick's snapshot of ground truth.
func (r *Repo) Fetch(ctx context.Context) error {
	if _, err := r.runRemote(ctx, "fetch", "--prune", "origin"); err != nil {
		return fmt.Errorf("gitx: fetch: %w", err)
	}
	return nil
}

// ListRefs returns every remote ref (as it is named on the remote, e.g.
// "refs/heads/main") mapped to its OID, as of the most recent Fetch.
func (r *Repo) ListRefs(ctx context.Context) (map[string]string, error) {
	out, err := r.run(ctx, "for-each-ref", "--format=%(objectname) %(refname)", "refs/remotes/origin")
	if err != nil {
		return nil, fmt.Errorf("gitx: list-refs: %w", err)
	}
	const prefix = "refs/remotes/origin/"
	refs := make(map[string]string)
	for _, ln := range splitLines(out) {
		oid, name, ok := strings.Cut(ln, " ")
		if !ok {
			continue
		}
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok || rest == "HEAD" {
			continue // skip the remote-tracking symbolic HEAD; not a real ref
		}
		refs["refs/heads/"+rest] = oid
	}
	return refs, nil
}

// MergeTree trial-merges candidate onto base without touching any working
// copy or branch.
func (r *Repo) MergeTree(ctx context.Context, base, candidate string) (core.TrialMerge, error) {
	out, err := r.run(ctx, "merge-tree", "--write-tree", base, candidate)
	lines := splitLines(out)
	if err == nil {
		if len(lines) == 0 {
			return core.TrialMerge{}, fmt.Errorf("gitx: merge-tree %s %s: empty output", base, candidate)
		}
		return core.TrialMerge{Clean: true, TreeOID: lines[0]}, nil
	}
	// Exit 1 covers both a real conflict and "not something we can merge"
	// (e.g. a bogus object name) with empty stdout in the latter case, so
	// the exit code alone doesn't distinguish them: a real conflict always
	// has at least the tree-OID line.
	var ge *gitError
	if errors.As(err, &ge) && ge.exitCode() == 1 && len(lines) > 0 {
		// Conflict: line 0 is still a tree OID (with conflict markers), then
		// stage lines "<mode> <oid> <stage>\t<path>" until a blank line,
		// then informational messages. Collect distinct conflicted paths.
		return core.TrialMerge{Clean: false, Conflicts: parseConflictPaths(lines[1:])}, nil
	}
	return core.TrialMerge{}, fmt.Errorf("gitx: merge-tree %s %s: %w", base, candidate, err)
}

func parseConflictPaths(lines []string) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, ln := range lines {
		if ln == "" {
			break
		}
		_, path, ok := strings.Cut(ln, "\t")
		if !ok || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}

// CommitTree creates a commit object from tree and parents. This is the
// only object gauntlet ever creates (Invariant 6). message is passed via
// stdin so multi-paragraph trailers survive intact.
//
// who's identity is forced via identityEnv, not just the "-c
// user.name=.../-c user.email=..." arguments below: git's own precedence
// puts GIT_AUTHOR_*/GIT_COMMITTER_* environment ahead of -c config, so a
// daemon process that inherits those variables from its own ambient
// environment would otherwise get the ENV identity on the commit, not who
// — empirically confirmed (TestCommitTreeIdentityImmuneToAmbientEnv;
// before this fix, the equivalent case was the -u-suppressed failure in
// TestCommitTreeTwoParentsWithTrailers). The -c flags stay for a reader
// checking `git log --format=%an` against a config-only mental model;
// identityEnv is what actually decides it.
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, message string, who core.Identity) (string, error) {
	args := []string{
		"-c", "user.name=" + who.Name,
		"-c", "user.email=" + who.Email,
		"commit-tree", "--no-gpg-sign", tree,
	}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	out, err := runGitEnv(ctx, r.dir, strings.NewReader(message), identityEnv(who), args...)
	if err != nil {
		return "", fmt.Errorf("gitx: commit-tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// identityEnv returns a subprocess environment that forces who as the
// GIT_AUTHOR_NAME/GIT_AUTHOR_EMAIL/GIT_COMMITTER_NAME/GIT_COMMITTER_EMAIL
// identity for an object-creating git command, regardless of what the
// daemon process's own ambient environment carries. Built as
// os.Environ() with those four appended (never prepended): os/exec keeps
// the LAST occurrence of a duplicate key, so any ambient
// GIT_AUTHOR_*/GIT_COMMITTER_* the process inherited is shadowed, not
// merged with — the same append-last pattern auth.go's runAuthed already
// uses for GIT_ASKPASS/GIT_TERMINAL_PROMPT/LC_ALL. This is the ONLY
// reliable way to pin identity: git's own precedence puts these four
// environment variables ahead of -c user.name/-c user.email, so passing
// -c alone (CommitTree and AddNote's previous behavior) is silently
// overridden whenever the ambient environment happens to set them —
// empirically confirmed by TestCommitTreeIdentityImmuneToAmbientEnv and
// TestAddNoteIdentityImmuneToAmbientEnv.
func identityEnv(who core.Identity) []string {
	return append(os.Environ(),
		"GIT_AUTHOR_NAME="+who.Name,
		"GIT_AUTHOR_EMAIL="+who.Email,
		"GIT_COMMITTER_NAME="+who.Name,
		"GIT_COMMITTER_EMAIL="+who.Email,
	)
}

// ReadFileFromTree reads path out of tree (or any tree-ish, e.g. a commit)
// without a checkout.
func (r *Repo) ReadFileFromTree(ctx context.Context, tree, path string) ([]byte, error) {
	out, err := r.run(ctx, "cat-file", "-p", tree+":"+path)
	if err != nil {
		return nil, fmt.Errorf("gitx: read %s from %s: %w", path, tree, err)
	}
	return []byte(out), nil
}

// CommitInfo is one commit's subject and body, as returned by Log.
type CommitInfo struct {
	Subject string
	Body    string
}

// Log returns, oldest-first, the subject and body of every commit reachable
// from tip but not from base (base..tip) — the commits a candidate branch
// actually introduces onto the target. internal/summarize uses this to
// build the prompt for an optional Claude-written merge-commit body;
// nothing else in gauntlet inspects commit bodies.
//
// The format string delimits fields with ASCII unit/record separators
// (0x1F/0x1E) rather than blank lines, since a commit body can itself
// contain blank lines and would otherwise be indistinguishable from the
// boundary between commits.
func (r *Repo) Log(ctx context.Context, base, tip string) ([]CommitInfo, error) {
	out, err := r.run(ctx, "log", "--reverse", "--format=%s\x1f%b\x1e", base+".."+tip)
	if err != nil {
		return nil, fmt.Errorf("gitx: log %s..%s: %w", base, tip, err)
	}
	var commits []CommitInfo
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimPrefix(rec, "\n") // git terminates each %...\x1e record with its own newline
		if rec == "" {
			continue
		}
		subject, body, _ := strings.Cut(rec, "\x1f")
		commits = append(commits, CommitInfo{Subject: subject, Body: strings.TrimRight(body, "\n")})
	}
	return commits, nil
}

// DiffStat returns git's diffstat summary for base..tip (the per-file
// change lines plus the "N files changed, ..." total) verbatim, for use as
// prompt context by internal/summarize.
func (r *Repo) DiffStat(ctx context.Context, base, tip string) (string, error) {
	out, err := r.run(ctx, "diff", "--stat", base+".."+tip)
	if err != nil {
		return "", fmt.Errorf("gitx: diff --stat %s..%s: %w", base, tip, err)
	}
	return strings.TrimRight(out, "\n"), nil
}

// IsAncestor reports whether maybeAncestor is an ancestor of ref.
func (r *Repo) IsAncestor(ctx context.Context, maybeAncestor, ref string) (bool, error) {
	_, err := r.run(ctx, "merge-base", "--is-ancestor", maybeAncestor, ref)
	if err == nil {
		return true, nil
	}
	var ge *gitError
	if errors.As(err, &ge) && ge.exitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("gitx: is-ancestor %s %s: %w", maybeAncestor, ref, err)
}

// maxLandingMergeSearch bounds how many first-parent merge commits
// FindLandingMerge walks back through branchTip before giving up. Landings
// being recovered from a crash are recent by construction (Invariant 4 fires
// on the daemon's very next tick after the crash), so a search this deep
// only fails to terminate promptly for a candidate that was never landed at
// all — which is exactly the "" ("unknown") case FindLandingMerge already
// returns for that scenario.
const maxLandingMergeSearch = 1000

// FindLandingMerge identifies the merge commit that landed candidateSHA onto
// branchTip's history. See core.GitRepo.FindLandingMerge for the contract.
//
// Plumbing: `git rev-list --first-parent --merges --parents` lists, newest
// first, exactly the merge commits along branchTip's first-parent chain
// (gauntlet never touches any other lineage), each line already carrying its
// parent OIDs — no separate rev-parse per candidate merge is needed. Each
// merge's second parent is compared against candidateSHA by exact equality,
// not ancestry: buildChainLinkPrecomputed (reconcile.go) always calls
// CommitTree with the candidate's own raw SHA as the second parent, and
// Invariant 6 (candidate commits are never rewritten) guarantees that SHA
// never changes underneath it, so equality alone always finds a landed
// candidate's own merge. Ancestry would be the WRONG generalization here:
// candidateSHA is trivially an ancestor of any later candidate whose author
// rebased onto main after candidateSHA's own landing (an ordinary
// occurrence), which would make the walk match that unrelated later merge
// instead of candidateSHA's actual one — equality is what Invariant 6
// promises, and it is both necessary and sufficient.
func (r *Repo) FindLandingMerge(ctx context.Context, branchTip, candidateSHA string) (string, error) {
	return r.findLandingMerge(ctx, branchTip, candidateSHA, maxLandingMergeSearch)
}

func (r *Repo) findLandingMerge(ctx context.Context, branchTip, candidateSHA string, maxCount int) (string, error) {
	out, err := r.run(ctx, "rev-list", "--first-parent", "--merges", "--parents",
		fmt.Sprintf("--max-count=%d", maxCount), branchTip)
	if err != nil {
		return "", fmt.Errorf("gitx: find-landing-merge %s: %w", branchTip, err)
	}
	for _, ln := range splitLines(out) {
		fields := strings.Fields(ln)
		if len(fields) < 3 {
			continue // defensive: --merges guarantees >=2 parents, so >=3 fields
		}
		if mergeOID, parent2 := fields[0], fields[2]; parent2 == candidateSHA {
			return mergeOID, nil
		}
	}
	return "", nil
}

// ExportTree materializes tree's contents into dir (created if necessary)
// via git archive, extracted with the standard library's tar reader to
// avoid BSD/GNU tar drift.
func (r *Repo) ExportTree(ctx context.Context, tree, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("gitx: export-tree %s: %w", tree, err)
	}
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+r.dir, "archive", "--format=tar", tree)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("gitx: export-tree %s: %w", tree, err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("gitx: export-tree %s: %w", tree, err)
	}
	extractErr := extractTar(stdout, dir)
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("gitx: export-tree %s: %w: %s", tree, waitErr, stderr.String())
	}
	if extractErr != nil {
		return fmt.Errorf("gitx: export-tree %s: extract: %w", tree, extractErr)
	}
	return nil
}

// pinRefPrefix is the local ref namespace that anchors in-flight synthetic
// merge commits against garbage collection. The queue owns no branch, so a
// trial chain's commits are otherwise unreferenced loose objects — safe
// from `git gc` only by the gc.pruneExpire grace period, which an
// operator's `--prune=now` (or a long-enough run) defeats. One ref per
// active run, named by the pinned OID itself, keeps the whole chain
// reachable (a commit reaches its parents) for as long as checks and hooks
// may resolve it through GAUNTLET_GIT_DIR.
//
// The namespace is deliberately outside refs/heads/ and refs/remotes/:
// Fetch's refspec and --prune only touch refs/remotes/origin/*, ListRefs
// only reads them, and CASUpdate only pushes refs the caller names — so
// pins are invisible to the queue's view of ground truth and are never
// pushed anywhere.
const pinRefPrefix = "refs/gauntlet/pin/"

// Pin anchors oid against garbage collection by pointing
// refs/gauntlet/pin/<oid> at it. Idempotent: pinning the same OID twice
// resolves to the same ref and value. Pins are local refs, not objects,
// so Invariant 6 (the merge commit is the only object gauntlet creates)
// stands.
func (r *Repo) Pin(ctx context.Context, oid string) error {
	if _, err := r.run(ctx, "update-ref", pinRefPrefix+oid, oid); err != nil {
		return fmt.Errorf("gitx: pin %s: %w", oid, err)
	}
	return nil
}

// Unpin removes oid's pin ref. Unpinning an OID that was never pinned (or
// was already unpinned) is a no-op, not an error — `update-ref -d` without
// an old-value assertion succeeds on a missing ref, which is exactly the
// idempotence terminal paths need (they unpin unconditionally rather than
// tracking whether their run ever reached the pin step).
func (r *Repo) Unpin(ctx context.Context, oid string) error {
	if _, err := r.run(ctx, "update-ref", "-d", pinRefPrefix+oid); err != nil {
		return fmt.Errorf("gitx: unpin %s: %w", oid, err)
	}
	return nil
}

// SweepTrialRefs deletes every REMOTE trial ref under prefix (issue #7),
// returning how many were removed. Startup calls this to clear refs a
// crashed previous process orphaned: the in-memory retention schedule
// (Daemon.trialReap) does not survive a restart, so without this, orphans
// would leak on the remote forever. Each delete is CAS-keyed on the SHA
// just observed (race-safe — a ref changed since the listing is left
// alone); an individual delete failure is returned so the caller can
// decide, but does not undo the ones that succeeded. This assumes one
// daemon owns the remote (gauntlet's "one daemon, N queues" model): with
// multiple daemons sharing it, a boot sweep would race a peer's live refs.
func (r *Repo) SweepTrialRefs(ctx context.Context, prefix string) (int, error) {
	refs, err := r.ListRemoteRefs(ctx, prefix+"/*")
	if err != nil {
		return 0, fmt.Errorf("gitx: sweep trial refs: %w", err)
	}
	// Best-effort, like every other trial-ref disposal path
	// (deleteTrialRefNow, reapTrialRefs): one ref that won't delete — a
	// transient network blip on its push, a server-side ref rule — must
	// not abort the remaining deletes OR block startup. The first hard
	// error is returned for the caller to LOG, but the sweep still
	// finishes, and the caller keeps running (a leftover trial ref anchors
	// only a synthetic merge, never correctness).
	n := 0
	var firstErr error
	for ref, oid := range refs {
		if err := r.CASUpdate(ctx, ref, oid, ""); err != nil {
			if errors.Is(err, core.ErrCASStale) {
				continue // changed since we listed; not ours to remove
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("gitx: sweep trial refs: delete %s: %w", ref, err)
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// SweepPins deletes every pin ref, returning how many were swept. Startup
// calls this before the first reconcile pass: a crash can strand pins, and
// every in-flight run is re-derived from refs and re-run from scratch
// anyway (Invariant 4), so a surviving pin never protects anything the new
// process still needs — the same rationale as sweeping the trials dir.
// Safe only under the daemon's exclusive state lock, like that sweep.
func (r *Repo) SweepPins(ctx context.Context) (int, error) {
	out, err := r.run(ctx, "for-each-ref", "--format=%(refname)", pinRefPrefix)
	if err != nil {
		return 0, fmt.Errorf("gitx: sweep pins: %w", err)
	}
	refs := splitLines(out)
	if len(refs) == 0 {
		return 0, nil
	}
	// One update-ref --stdin transaction instead of a subprocess per ref.
	var b strings.Builder
	for _, ref := range refs {
		fmt.Fprintf(&b, "delete %s\n", ref)
	}
	if _, err := runGit(ctx, r.dir, strings.NewReader(b.String()), "update-ref", "--stdin"); err != nil {
		return 0, fmt.Errorf("gitx: sweep pins: %w", err)
	}
	return len(refs), nil
}

// ListRemoteRefs returns every remote ref matching pattern (a git
// ref-glob, e.g. "refs/gauntlet/trials/*") as name -> OID, straight from
// the remote via ls-remote — NOT the local remote-tracking view, which
// only mirrors refs/heads/* (the fetch refspec). The trial-ref reaper
// (issue #7) needs the authoritative remote state under a custom
// namespace the daemon never fetches, so this is its own round trip.
// Empty result (no match) is not an error.
func (r *Repo) ListRemoteRefs(ctx context.Context, pattern string) (map[string]string, error) {
	out, err := r.runRemote(ctx, "ls-remote", "origin", pattern)
	if err != nil {
		return nil, fmt.Errorf("gitx: ls-remote %s: %w", pattern, err)
	}
	refs := make(map[string]string)
	for _, ln := range splitLines(out) {
		// "<oid>\t<refname>"
		oid, name, ok := strings.Cut(ln, "\t")
		if !ok {
			continue
		}
		refs[name] = oid
	}
	return refs, nil
}

// CASUpdate compare-and-swaps remoteRef from oldOID to newOID (newOID == ""
// deletes). oldOID == "" asserts the ref must not currently exist. Returns
// core.ErrCASStale (wrapped) if the ref's actual value did not match oldOID.
func (r *Repo) CASUpdate(ctx context.Context, remoteRef, oldOID, newOID string) error {
	var refspec string
	if newOID == "" {
		refspec = ":" + remoteRef
	} else {
		refspec = newOID + ":" + remoteRef
	}
	lease := "--force-with-lease=" + remoteRef + ":" + oldOID
	_, err := r.runRemote(ctx, "push", "origin", refspec, lease)
	if err == nil {
		return nil
	}
	if isStaleLease(err) {
		return fmt.Errorf("gitx: cas update %s: %w", remoteRef, core.ErrCASStale)
	}
	return fmt.Errorf("gitx: cas update %s: %w", remoteRef, err)
}

func isStaleLease(err error) bool {
	var ge *gitError
	return errors.As(err, &ge) && strings.Contains(ge.stderr, "stale info")
}

func (r *Repo) run(ctx context.Context, args ...string) (string, error) {
	return runGit(ctx, r.dir, nil, args...)
}

func runGit(ctx context.Context, gitDir string, stdin io.Reader, args ...string) (string, error) {
	return runGitEnv(ctx, gitDir, stdin, nil, args...)
}

// runGitEnv is runGit with an explicit subprocess environment (nil
// inherits the daemon's). The credential-injection path (auth.go) is the
// only caller that passes one: the token rides in the git subprocess's
// env, read back by the askpass helper — never in argv, never in the
// persistent remote URL.
func runGitEnv(ctx context.Context, gitDir string, stdin io.Reader, env []string, args ...string) (string, error) {
	full := args
	if gitDir != "" {
		full = append([]string{"--git-dir=" + gitDir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Stdin = stdin
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &gitError{args: args, stderr: stderr.String(), cause: err}
	}
	return stdout.String(), nil
}

// gitError wraps a failed git invocation, keeping the raw exit error
// (for exit-code branching) and stderr text (for stale-lease detection)
// alongside a readable message.
type gitError struct {
	args   []string
	stderr string
	cause  error
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.cause, strings.TrimSpace(e.stderr))
}

func (e *gitError) Unwrap() error { return e.cause }

func (e *gitError) exitCode() int {
	var ee *exec.ExitError
	if errors.As(e.cause, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func extractTar(src io.Reader, dir string) error {
	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode&0o777))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}
