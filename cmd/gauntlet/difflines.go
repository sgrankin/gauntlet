package main

import (
	"fmt"
	"strings"
)

// unifiedDiffContext is the number of unchanged lines of context shown
// around each change in `gauntlet fmt -d` output — the conventional `diff
// -u` default.
const unifiedDiffContext = 3

// opKind tags one line of a diffLines edit script.
type opKind byte

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind opKind
	line string
}

// unifiedDiff renders a minimal unified diff between before and after —
// `gauntlet fmt -d`'s only job. This is a plain O(n*m) LCS line diff, not a
// new dependency: gauntlet's config files are small (tens to low hundreds
// of lines), so the quadratic cost never matters in practice, and kdlfmt's
// own safety property (never reorders/merges/splits a line — see
// internal/kdlfmt's package doc) means real diffs here are always small
// and local regardless.
func unifiedDiff(path string, before, after []byte) []byte {
	a := splitLinesKeepingNone(before)
	b := splitLinesKeepingNone(after)
	ops := diffLines(a, b)
	hunks := buildHunks(ops, unifiedDiffContext)

	var buf strings.Builder
	fmt.Fprintf(&buf, "--- %s.orig\n", path)
	fmt.Fprintf(&buf, "+++ %s\n", path)
	for _, h := range hunks {
		h.writeTo(&buf)
	}
	return []byte(buf.String())
}

// splitLinesKeepingNone splits data on "\n" into lines with no trailing
// newline markers, dropping the single spurious empty element a trailing
// "\n" produces (matching the same "not a real line" reasoning as
// internal/kdlfmt's normalize doc).
func splitLinesKeepingNone(data []byte) []string {
	s := string(data)
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// diffLines computes an edit script turning a into b via a classic
// dynamic-programming LCS (longest common subsequence) over whole lines,
// backtracked into equal/delete/insert ops in forward order.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	// dp[i][j] = LCS length of a[:i] and b[:j].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	var rev []diffOp
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1]:
			rev = append(rev, diffOp{opEqual, a[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			rev = append(rev, diffOp{opInsert, b[j-1]})
			j--
		default:
			rev = append(rev, diffOp{opDelete, a[i-1]})
			i--
		}
	}

	ops := make([]diffOp, len(rev))
	for k, op := range rev {
		ops[len(rev)-1-k] = op
	}
	return ops
}

// hunk is one unified-diff hunk: a contiguous slice of ops (which may
// include leading/trailing equal-line context) plus the 1-based starting
// line numbers in the old and new files.
type hunk struct {
	oldStart, newStart int
	ops                []diffOp
}

// buildHunks groups a full edit script into unified-diff hunks, each
// carrying up to `context` unchanged lines before and after its changes;
// two change regions closer than 2*context apart are merged into one hunk
// (mirrors `diff -u`'s own hunk-merging behavior, avoiding a run of tiny
// adjacent hunks).
func buildHunks(ops []diffOp, context int) []hunk {
	// changed[k] marks ops[k] as non-equal; used to find change regions.
	var regions [][2]int // [start,end) indices into ops, non-equal runs
	i := 0
	for i < len(ops) {
		if ops[i].kind == opEqual {
			i++
			continue
		}
		start := i
		for i < len(ops) && ops[i].kind != opEqual {
			i++
		}
		regions = append(regions, [2]int{start, i})
	}
	if len(regions) == 0 {
		return nil
	}

	// Expand each region by `context` on both sides (clamped to ops
	// bounds), then merge regions whose expanded windows overlap or touch.
	type window struct{ start, end int }
	windows := make([]window, len(regions))
	for k, r := range regions {
		s := r[0] - context
		if s < 0 {
			s = 0
		}
		e := r[1] + context
		if e > len(ops) {
			e = len(ops)
		}
		windows[k] = window{s, e}
	}
	merged := []window{windows[0]}
	for _, w := range windows[1:] {
		last := &merged[len(merged)-1]
		if w.start <= last.end {
			if w.end > last.end {
				last.end = w.end
			}
			continue
		}
		merged = append(merged, w)
	}

	// oldLine/newLine track the 1-based line number each ops[k] starts at,
	// computed once by walking the full script.
	oldLine := make([]int, len(ops)+1)
	newLine := make([]int, len(ops)+1)
	oldLine[0], newLine[0] = 1, 1
	for k, op := range ops {
		oldLine[k+1], newLine[k+1] = oldLine[k], newLine[k]
		switch op.kind {
		case opEqual:
			oldLine[k+1]++
			newLine[k+1]++
		case opDelete:
			oldLine[k+1]++
		case opInsert:
			newLine[k+1]++
		}
	}

	hunks := make([]hunk, len(merged))
	for k, w := range merged {
		hunks[k] = hunk{
			oldStart: oldLine[w.start],
			newStart: newLine[w.start],
			ops:      ops[w.start:w.end],
		}
	}
	return hunks
}

// writeTo renders one hunk in `diff -u` form: an "@@ -oldStart,oldCount
// +newStart,newCount @@" header followed by its lines (" " context, "-"
// deleted, "+" inserted).
func (h hunk) writeTo(buf *strings.Builder) {
	oldCount, newCount := 0, 0
	for _, op := range h.ops {
		switch op.kind {
		case opEqual:
			oldCount++
			newCount++
		case opDelete:
			oldCount++
		case opInsert:
			newCount++
		}
	}
	fmt.Fprintf(buf, "@@ -%d,%d +%d,%d @@\n", h.oldStart, oldCount, h.newStart, newCount)
	for _, op := range h.ops {
		switch op.kind {
		case opEqual:
			fmt.Fprintf(buf, " %s\n", op.line)
		case opDelete:
			fmt.Fprintf(buf, "-%s\n", op.line)
		case opInsert:
			fmt.Fprintf(buf, "+%s\n", op.line)
		}
	}
}
