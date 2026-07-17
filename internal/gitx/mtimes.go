package gitx

import (
	"bufio"
	"context"
	"fmt"
	"io"
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

// RestoreMtimes rewrites every tracked file's (and symlink's) mtime under
// dir — a fresh ExportTree of commit's tree — to the committer time of the
// last commit that changed that path in commit's history, so repeated
// exports of the same commit are byte- AND metadata-identical however far
// apart in wall time they happen (the git-restore-mtime behavior,
// DESIGN.md "deterministic per-path mtimes"). Two subprocesses, always —
// one `ls-tree`, one bounded `log` walk — never one per file.
//
// Semantics (the documented contract, tested in mtimes_test.go):
//   - committer time, not author time; future-dated timestamps verbatim
//     (deterministic beats plausible — a cache may decline reuse until the
//     clock catches up, which is preferable to nondeterministic clamping);
//   - a path changed relative to ALL of a merge's parents (auto-merge
//     product — including gauntlet's own synthetic trial merge) gets that
//     merge's time; a path matching any parent is inherited and keeps its
//     deeper history-derived time;
//   - a rename is a change at the new path;
//   - symlink mtimes are set WITHOUT following the link (unix.Lutimes) —
//     the target's metadata is never touched;
//   - directories are untouched: git tracks no directory metadata, and no
//     documented cache keys on directory mtimes;
//   - any failure is returned as-is — callers treat it as an
//     infrastructure error and fail the trial; there is deliberately no
//     silent wall-clock fallback (a tree claiming stable-cache behavior
//     must never quietly not have it).
//
// The walk stops as soon as every tracked path is stamped; the worst case
// (a path untouched since the root commit) reads full history once. Paths
// present in the tree but absent from consumed history (possible only if
// the walk errors mid-stream) fail the pass rather than stay wall-clocked.
func (r *Repo) RestoreMtimes(ctx context.Context, commit, dir string) (core.MtimeStats, error) {
	var stats core.MtimeStats

	// Tracked paths (files and symlinks) with modes, NUL-delimited so any
	// filename is safe.
	out, err := r.run(ctx, "ls-tree", "-r", "-z", commit)
	if err != nil {
		return stats, fmt.Errorf("gitx: restore-mtimes %s: ls-tree: %w", commit, err)
	}
	symlink := make(map[string]bool)
	pending := make(map[string]bool)
	for _, ent := range strings.Split(strings.TrimRight(out, "\x00"), "\x00") {
		if ent == "" {
			continue
		}
		// "<mode> <type> <oid>\t<path>"
		meta, path, ok := strings.Cut(ent, "\t")
		if !ok {
			return stats, fmt.Errorf("gitx: restore-mtimes %s: unparseable ls-tree entry %q", commit, ent)
		}
		pending[path] = true
		if strings.HasPrefix(meta, "120000") {
			symlink[path] = true
		}
	}
	if len(pending) == 0 {
		return stats, nil
	}

	// One history walk, newest first. -m splits every merge into one
	// entry per parent so the intersection rule below can classify
	// auto-merge products; --name-status -z gives NUL-safe per-path
	// change lists; %ct is the committer time. The record separator \x01
	// can't appear in git output otherwise.
	cmd := gitCommand(ctx, r.dir, "log", "-z", "-m", "--name-status", "--format=%x01%H %ct", commit)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return stats, fmt.Errorf("gitx: restore-mtimes %s: %w", commit, err)
	}
	if err := cmd.Start(); err != nil {
		return stats, fmt.Errorf("gitx: restore-mtimes %s: start log: %w", commit, err)
	}
	// The walk stops early once every path is stamped; killing the log
	// subprocess then is the point (worst case it read all of history).
	defer func() {
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
		curEntries int            // -m entries seen for curHash
		curTouched map[string]int // path -> how many of curHash's entries list it
	)
	flush := func() error {
		if curHash == "" {
			return nil
		}
		for path, n := range curTouched {
			// The intersection rule: a merge (curEntries > 1) only OWNS a
			// path it changed relative to EVERY parent — an auto-merge
			// product (or the synthetic trial merge's genuinely merged
			// content). A path matching any parent was inherited; leave
			// it pending for the history behind that parent. Ordinary
			// commits (curEntries == 1) own everything they list.
			if n == curEntries {
				if err := stamp(path, curTime); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for len(pending) > 0 {
		rec, ok, err := walker.next()
		if err != nil {
			return stats, fmt.Errorf("gitx: restore-mtimes %s: walk: %w", commit, err)
		}
		if !ok {
			break
		}
		stats.Commits++
		if rec.hash != curHash {
			if err := flush(); err != nil {
				return stats, err
			}
			curHash, curTime = rec.hash, rec.time
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

	if len(pending) > 0 {
		// History ran out with paths unstamped. This should be impossible
		// for a well-formed commit (the root commit's additions cover
		// everything), so it means the walk was cut short — fail rather
		// than leave silently wall-clocked files in a tree that claims
		// deterministic metadata.
		for path := range pending {
			return stats, fmt.Errorf("gitx: restore-mtimes %s: history exhausted with %d path(s) unstamped (first: %q)", commit, len(pending), path)
		}
	}
	return stats, nil
}

// logRecord is one `git log -m` entry: a commit (possibly one of several
// entries for the same merge), its committer time, and the paths its diff
// touched.
type logRecord struct {
	hash  string
	time  time.Time
	paths []string
}

// logWalker incrementally parses `git log -z -m --name-status
// --format=%x01%H %ct` output: records separated by \x01, each "H ct\n"
// header followed by NUL-delimited "<status>\0<path>[\0<path2>]" pairs
// (renames/copies carry two paths — the SECOND is the post-change name,
// the one that exists in the exported tree).
type logWalker struct {
	s *bufio.Scanner
}

func newLogWalker(r io.Reader) *logWalker {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 16*1024*1024)
	s.Split(scanRecords)
	return &logWalker{s: s}
}

// scanRecords splits on \x01 record separators.
func scanRecords(data []byte, atEOF bool) (advance int, token []byte, err error) {
	// Skip a leading separator.
	start := 0
	for start < len(data) && data[start] == 0x01 {
		start++
	}
	if i := bytesIndexByte(data[start:], 0x01); i >= 0 {
		return start + i + 1, data[start : start+i], nil
	}
	if atEOF {
		if start == len(data) {
			return start, nil, nil
		}
		return len(data), data[start:], nil
	}
	return start, nil, nil
}

func bytesIndexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func (w *logWalker) next() (logRecord, bool, error) {
	if !w.s.Scan() {
		if err := w.s.Err(); err != nil {
			return logRecord{}, false, err
		}
		return logRecord{}, false, nil
	}
	raw := w.s.Text()
	header, body, _ := strings.Cut(raw, "\n")
	// -z terminates the format line with a NUL before the newline.
	header = strings.Trim(header, "\x00 ")
	hash, ctStr, ok := strings.Cut(header, " ")
	if !ok {
		return logRecord{}, false, fmt.Errorf("unparseable log header %q", header)
	}
	ct, err := strconv.ParseInt(ctStr, 10, 64)
	if err != nil {
		return logRecord{}, false, fmt.Errorf("unparseable committer time %q: %w", ctStr, err)
	}
	rec := logRecord{hash: hash, time: time.Unix(ct, 0)}

	fields := strings.Split(strings.Trim(body, "\x00"), "\x00")
	for i := 0; i < len(fields); {
		status := fields[i]
		if status == "" {
			i++
			continue
		}
		switch status[0] {
		case 'R', 'C':
			// Rename/copy: <status>\0<old>\0<new> — the new path is the
			// change ("a rename is a change at the new path").
			if i+2 >= len(fields) {
				return logRecord{}, false, fmt.Errorf("truncated rename entry after %q", status)
			}
			rec.paths = append(rec.paths, fields[i+2])
			i += 3
		default:
			if i+1 >= len(fields) {
				return logRecord{}, false, fmt.Errorf("truncated entry after status %q", status)
			}
			rec.paths = append(rec.paths, fields[i+1])
			i += 2
		}
	}
	return rec, true, nil
}
