package gitx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sgrankin/gauntlet/internal/core"
)

// gitCommand builds (without running) a git invocation against gitDir, for
// the streamed-output cases plain runGit's buffer-everything shape doesn't
// fit (ExportTree's archive pipe is the other one).
func gitCommand(ctx context.Context, gitDir string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + gitDir}, args...)...)
}

// RestoreMtimes rewrites every exported file's (and symlink's) mtime under
// dir — a fresh ExportTree of commit's tree — to the committer time of the
// last commit that changed that path in commit's history, so repeated
// exports of the same commit are byte- AND metadata-identical however far
// apart in wall time they happen (the git-restore-mtime behavior,
// DESIGN.md "deterministic per-path mtimes"). One bounded `git log`
// subprocess, always — never one per file.
//
// Semantics (the documented contract, tested in mtimes_test.go):
//   - committer time, not author time; future-dated timestamps verbatim
//     (deterministic beats plausible — a cache may decline reuse until the
//     clock catches up, which is preferable to nondeterministic clamping);
//   - a path changed relative to ALL of a merge's parents (auto-merge
//     product — including gauntlet's own synthetic trial merge) gets that
//     merge's time; a path matching any parent is inherited and keeps its
//     deeper history-derived time — in particular, a trial merge whose
//     tree is identical to the candidate's (the up-to-date-candidate
//     shape) owns nothing, so re-trials of the same content restamp
//     identically;
//   - a rename is a change at the new path;
//   - symlink mtimes are set WITHOUT following the link (unix.Lutimes) —
//     the target's metadata is never touched;
//   - directories are untouched: git tracks no directory metadata, and no
//     documented cache keys on directory mtimes;
//   - only what the export materialized is stamped: export-ignore'd paths
//     and submodule gitlinks are tracked but not exported, so the pending
//     set comes from walking dir, never from ls-tree;
//   - any failure is returned as-is — callers treat it as an
//     infrastructure error and fail the trial; there is deliberately no
//     silent wall-clock fallback (a tree claiming stable-cache behavior
//     must never quietly not have it).
//
// One deliberate, disclosed approximation (matching the standard
// git-restore-mtime tool): "last commit that changed the path" is the
// newest diff-touching commit in git log's commit-date order across the
// whole DAG. When a merge discarded one side's change to a path, that
// discarded-side commit can be newer than the surviving side's and wins
// the stamp, diverging from `git log -1 -- path` (which follows the
// surviving lineage). The result is still fully deterministic per commit —
// the property caches key on — just not always the content-lineage time;
// doing better requires per-path history simplification, which a single
// no-pathspec walk cannot express.
//
// The walk stops as soon as every exported path is stamped; the worst case
// (a path untouched since the root commit) reads full history once.
func (r *Repo) RestoreMtimes(ctx context.Context, commit, dir string) (core.MtimeStats, error) {
	var stats core.MtimeStats

	// The pending set is what is actually ON DISK, not `ls-tree`: the
	// export (git archive) honors export-ignore attributes and turns
	// submodule gitlinks into bare directories, so the tree listing
	// over-approximates the exported files and stamping from it would
	// fail ENOENT on any export-ignored path. Lstat's mode also
	// distinguishes symlinks (stamped without following) for free.
	symlink := make(map[string]bool)
	pending := make(map[string]bool)
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		pending[rel] = true
		if d.Type()&fs.ModeSymlink != 0 {
			symlink[rel] = true
		}
		return nil
	})
	if walkErr != nil {
		return stats, fmt.Errorf("gitx: restore-mtimes %s: walk export dir: %w", commit, walkErr)
	}
	if len(pending) == 0 {
		return stats, nil
	}

	// One history walk, newest first. --diff-merges=separate (spelled
	// explicitly: `-m` is a shorthand whose meaning host config —
	// log.diffMerges — can silently change) splits every merge into one
	// entry per parent so the intersection rule below can classify
	// auto-merge products. git emits NO entry at all for a parent the
	// merge is tree-identical to, so %P rides along in the header to make
	// the missing entries detectable. --name-status -z gives NUL-safe
	// per-path change lists; %ct is the committer time. The -c overrides
	// pin the two other log.* knobs that can reshape the stream: showRoot
	// off would drop the root's entries (every root-era path then reads
	// as unstamped), and showSignature on pollutes stdout ahead of the
	// first header.
	cmd := gitCommand(ctx, r.dir,
		"-c", "log.showRoot=true", "-c", "log.showSignature=false",
		"log", "-z", "--diff-merges=separate", "--name-status", "--format=%x01%H %P %ct", commit)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return stats, fmt.Errorf("gitx: restore-mtimes %s: %w", commit, err)
	}
	if err := cmd.Start(); err != nil {
		return stats, fmt.Errorf("gitx: restore-mtimes %s: start log: %w", commit, err)
	}
	// Two ways the subprocess ends. Voluntary early termination — every
	// path stamped with history left over — kills it (that IS the
	// optimization). But if the STREAM ended, git's exit status is
	// meaningful and must be checked: a dying subprocess can truncate the
	// output mid-record in ways that still parse, so only exit 0
	// certifies the walk as complete.
	waited := false
	defer func() {
		if waited {
			return
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	stamp := func(path string, t time.Time) error {
		if !pending[path] {
			return nil
		}
		delete(pending, path)
		stats.Paths++
		full := filepath.Join(dir, path)
		if symlink[path] {
			tv := unix.NsecToTimeval(t.UnixNano())
			if err := unix.Lutimes(full, []unix.Timeval{tv, tv}); err != nil {
				return fmt.Errorf("gitx: restore-mtimes: lutimes %s: %w", path, err)
			}
			return nil
		}
		if err := os.Chtimes(full, t, t); err != nil {
			return fmt.Errorf("gitx: restore-mtimes: chtimes %s: %w", path, err)
		}
		return nil
	}

	walker := newLogWalker(stdout)
	var (
		curHash    string
		curTime    time.Time
		curParents int
		curEntries int            // per-parent entries seen for curHash
		curTouched map[string]int // path -> how many of curHash's entries list it
	)
	flush := func() error {
		if curHash == "" {
			return nil
		}
		// The intersection rule: a merge only OWNS a path it changed
		// relative to EVERY parent — an auto-merge product (or the
		// synthetic trial merge's genuinely merged content). A path
		// matching any parent was inherited; leave it pending for the
		// history behind that parent. git emits no entry at all for a
		// parent the merge is tree-identical to, so fewer entries than
		// parents means some parent's whole diff was empty — nothing
		// changed relative to that parent, the merge owns nothing.
		// Ordinary commits (one parent, one entry) own everything they
		// list; a root commit (zero parents, one entry) owns its
		// additions.
		expected := curParents
		if expected == 0 {
			expected = 1
		}
		if curEntries != expected {
			return nil
		}
		for path, n := range curTouched {
			if n == curEntries {
				if err := stamp(path, curTime); err != nil {
					return err
				}
			}
		}
		return nil
	}
	sawEOF := false
	for len(pending) > 0 {
		rec, ok, err := walker.next()
		if err != nil {
			return stats, fmt.Errorf("gitx: restore-mtimes %s: walk: %w", commit, err)
		}
		if !ok {
			sawEOF = true
			break
		}
		stats.Commits++
		if rec.hash != curHash {
			if err := flush(); err != nil {
				return stats, err
			}
			curHash, curTime, curParents = rec.hash, rec.time, rec.parents
			curEntries, curTouched = 0, make(map[string]int)
		}
		curEntries++
		for _, p := range rec.paths {
			curTouched[p]++
		}
	}
	if err := flush(); err != nil {
		return stats, err
	}
	if sawEOF {
		if err := cmd.Wait(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if len(msg) > 2048 {
				msg = msg[:2048] + "..."
			}
			if msg != "" {
				return stats, fmt.Errorf("gitx: restore-mtimes %s: log: %w: %s", commit, err, msg)
			}
			return stats, fmt.Errorf("gitx: restore-mtimes %s: log: %w", commit, err)
		}
		waited = true
	}

	if len(pending) > 0 {
		// History ran out with paths unstamped: a file on disk that no
		// consumed commit ever touched. Impossible for a faithful export
		// of a well-formed commit (the root's additions cover
		// everything), so fail rather than leave silently wall-clocked
		// files in a tree that claims deterministic metadata.
		var first string
		for path := range pending {
			first = path
			break
		}
		return stats, fmt.Errorf("gitx: restore-mtimes %s: history exhausted with %d path(s) unstamped (first: %q)", commit, len(pending), first)
	}
	return stats, nil
}

// logRecord is one `git log --diff-merges=separate` entry: a commit
// (possibly one of several entries for the same merge — one per parent
// with a non-empty diff), its parent count, its committer time, and the
// paths this entry's diff touched.
type logRecord struct {
	hash    string
	parents int
	time    time.Time
	paths   []string
}

// logWalker incrementally parses `git log -z --diff-merges=separate
// --name-status --format=%x01%H %P %ct` output. With -z the stream is a
// sequence of NUL-terminated tokens, and NUL is the ONLY byte that can
// never appear in a path — 0x01 can, so the \x01 header sentinel is
// trustworthy only at token start, a position a path can never occupy.
// Token shape:
//
//	\x01<hash> [<parent>...] <ct>          (header; %P is empty for a root)
//	\n<status>  <path>                     (first entry; the \n comes from
//	                                        the format's trailing newline)
//	<status>  <path>                       (subsequent entries)
//	<status>  <old>  <new>                 (renames/copies carry two paths —
//	                                        the SECOND is the post-change
//	                                        name, the one in the tree)
//
// Position fully disambiguates: after a status the next token is a path,
// whatever bytes it holds; in status-or-header position a token is a
// header iff it starts with \x01, and anything that is neither a header
// nor a well-formed status is a desync — fail loudly.
type logWalker struct {
	s      *bufio.Scanner
	peeked *string
}

func newLogWalker(r io.Reader) *logWalker {
	s := bufio.NewScanner(r)
	// One token is one path (or one header) — never a whole commit's
	// change list — so the cap bounds a single filename, generously.
	s.Buffer(make([]byte, 64*1024), 16*1024*1024)
	s.Split(scanNulTokens)
	return &logWalker{s: s}
}

// scanNulTokens splits on NUL terminators. A trailing unterminated token
// at EOF is returned as-is: completeness is certified by git's exit
// status (RestoreMtimes checks Wait when the stream ends), not by framing.
func scanNulTokens(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func (w *logWalker) token() (string, bool, error) {
	if w.peeked != nil {
		t := *w.peeked
		w.peeked = nil
		return t, true, nil
	}
	if !w.s.Scan() {
		if err := w.s.Err(); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
	return w.s.Text(), true, nil
}

func (w *logWalker) unread(t string) { w.peeked = &t }

// validStatus reports whether tok has the shape of a --name-status status
// column: one uppercase letter plus an optional similarity score.
func validStatus(tok string) bool {
	if tok == "" || tok[0] < 'A' || tok[0] > 'Z' {
		return false
	}
	for i := 1; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

func (w *logWalker) next() (logRecord, bool, error) {
	tok, ok, err := w.token()
	if err != nil || !ok {
		return logRecord{}, false, err
	}
	if !strings.HasPrefix(tok, "\x01") {
		return logRecord{}, false, fmt.Errorf("expected a record header, got %q", tok)
	}
	fields := strings.Fields(tok[1:])
	if len(fields) < 2 {
		return logRecord{}, false, fmt.Errorf("unparseable log header %q", tok)
	}
	ct, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	if err != nil {
		return logRecord{}, false, fmt.Errorf("unparseable committer time in header %q: %w", tok, err)
	}
	rec := logRecord{hash: fields[0], parents: len(fields) - 2, time: time.Unix(ct, 0)}

	for {
		tok, ok, err := w.token()
		if err != nil {
			return logRecord{}, false, err
		}
		if !ok {
			return rec, true, nil
		}
		if strings.HasPrefix(tok, "\x01") {
			w.unread(tok)
			return rec, true, nil
		}
		status := strings.TrimPrefix(tok, "\n")
		if !validStatus(status) {
			return logRecord{}, false, fmt.Errorf("expected a status token, got %q", tok)
		}
		path, ok, err := w.token()
		if err != nil {
			return logRecord{}, false, err
		}
		if !ok {
			return logRecord{}, false, fmt.Errorf("truncated entry after status %q", status)
		}
		if status[0] == 'R' || status[0] == 'C' {
			// Rename/copy: <old> then <new> — the new path is the change
			// ("a rename is a change at the new path").
			path, ok, err = w.token()
			if err != nil {
				return logRecord{}, false, err
			}
			if !ok {
				return logRecord{}, false, fmt.Errorf("truncated rename entry after status %q", status)
			}
		}
		rec.paths = append(rec.paths, path)
	}
}
