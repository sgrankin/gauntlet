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
}

var _ core.GitRepo = (*Repo)(nil)

// New opens the bare repository at dir, initializing it (git init --bare)
// if dir does not already exist. Either way, origin is set (or updated) to
// remoteURL and configured with a fetch refspec that maps the remote's
// refs/heads/* into this repo's refs/remotes/origin/* — the fixed point
// Fetch and ListRefs rely on. It does not fetch; callers should Fetch
// before relying on any ref state.
func New(ctx context.Context, dir, remoteURL string) (*Repo, error) {
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
	if _, err := r.run(ctx, "fetch", "--prune", "origin"); err != nil {
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
func (r *Repo) CommitTree(ctx context.Context, tree string, parents []string, message string, who core.Identity) (string, error) {
	args := []string{
		"-c", "user.name=" + who.Name,
		"-c", "user.email=" + who.Email,
		"commit-tree", "--no-gpg-sign", tree,
	}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	out, err := runGit(ctx, r.dir, strings.NewReader(message), args...)
	if err != nil {
		return "", fmt.Errorf("gitx: commit-tree: %w", err)
	}
	return strings.TrimSpace(out), nil
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
	_, err := r.run(ctx, "push", "origin", refspec, lease)
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
	full := args
	if gitDir != "" {
		full = append([]string{"--git-dir=" + gitDir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Stdin = stdin
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
