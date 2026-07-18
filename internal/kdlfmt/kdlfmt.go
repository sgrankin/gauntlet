// Package kdlfmt normalizes whitespace in a KDL document without touching
// its content.
//
// A spike on issue #12 proved kdl-go (github.com/sblinch/kdl-go, gauntlet's
// only KDL dependency) loses/mangles comments on parse->emit: its document
// model isn't a faithful enough round-trip surface to format through. So
// this package deliberately never builds or re-emits a kdl-go document.
// Instead it is a humbler LINE-BASED normalizer whose safety property is
// structural, not semantic: it never drops, merges, splits, or reorders a
// line of the input — it only rewrites each line's leading/trailing
// whitespace, collapses blank-line runs, and normalizes the final newline.
// Comments and `/-` slashdash nodes are therefore trivially safe: a line
// that's entirely comment text is still exactly one line, in exactly the
// same place, before and after.
//
// The complete transform list (nothing else):
//  1. Re-indent each line by brace depth, 4 spaces per level; a line whose
//     first token(s) are closing braces dedents for them (gofmt-style).
//  2. Strip trailing whitespace from each line.
//  3. Collapse runs of 2+ blank lines to exactly 1; drop blank lines at the
//     very start of the file and just before EOF.
//  4. Exactly one final newline.
//
// Depth tracking needs to know when a `{`/`}` is lexically inert — inside a
// line comment, a (possibly nested, possibly multi-line) block comment, a
// quoted string, or a raw string — without parsing the document; see
// scanLine's doc for that mini-lexer's rules and normalize's doc for how
// per-line results are assembled into the final output.
package kdlfmt

import (
	"bytes"
	"fmt"
	"strings"

	kdl "github.com/sblinch/kdl-go"
)

// indentUnit is transform 1's "4 spaces per level" — the only knob this
// package has, deliberately: gofmt-shaped means no style options.
const indentUnit = "    "

// Format normalizes data's whitespace per the package doc and returns the
// result. It never round-trips through kdl-go's document model (see the
// package doc) and it REFUSES rather than guesses: any of
//   - data failing kdl.Parse (fmt is for valid KDL, not a repair tool),
//   - the mini-lexer hitting malformed input (unterminated string/comment,
//     more closing braces than open), or
//   - the belt-and-suspenders check that the FORMATTED output still parses
//
// returns a non-nil error and a nil result. Callers must not write/print a
// nil result — see cmd/gauntlet/fmt.go, which never truncates a target file
// on error.
func Format(data []byte) ([]byte, error) {
	if _, err := kdl.Parse(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("kdlfmt: not valid KDL: %w", err)
	}
	out, err := normalize(data)
	if err != nil {
		return nil, err
	}
	// Defense against a lexer bug: normalize's whitespace-only edits should
	// never be able to invalidate already-valid KDL, but a lexer that
	// mis-tracks string/comment state could still shift a brace's rendered
	// column without changing its structural meaning — this catches that
	// class of bug before it reaches disk instead of trusting the proof by
	// construction.
	if _, err := kdl.Parse(bytes.NewReader(out)); err != nil {
		return nil, fmt.Errorf("kdlfmt: internal error: formatted output is not valid KDL: %w", err)
	}
	return out, nil
}

// renderedLine is one output line plus the two facts the blank-collapsing
// pass in normalize needs about it: whether it was rendered at all (a
// passthrough line is emitted byte-identical to the input, never
// reindented/trimmed, and never treated as droppable/collapsible — see
// normalize's doc) and, for non-passthrough lines, whether it's blank.
type renderedLine struct {
	text        string
	passthrough bool
	blank       bool
}

// normalize is transforms 1-4 (see package doc), split into two passes:
// scanLine renders (or passes through) each line independently, tracking
// lexical state across lines; the second pass in this function collapses
// blank-line runs across the whole rendered sequence.
//
// Blank-line collapsing only ever considers/removes non-passthrough blank
// lines. A blank line that is itself inside a multi-line block comment,
// raw string, or multi-line quoted string is a passthrough line (see
// scanLine) and is therefore NEVER
// dropped or merged into a neighboring collapse run — deleting it would
// violate "a comment's interior is content, not code" for no formatting
// benefit. A passthrough line does, however, still flush (collapse to at
// most one) any blank-line run immediately before it, exactly like any
// other non-blank line would.
func normalize(data []byte) ([]byte, error) {
	// A trailing "\n" in data makes the last split element "" — not a real
	// blank line, just the empty tail after the final newline. Treating it
	// as an ordinary (droppable) blank line is exactly correct: it's
	// always at the very end, so the "drop blank lines just before EOF"
	// rule removes it, and transform 4 supplies the single real trailing
	// newline the input's own last "\n" stood for.
	rawLines := bytes.Split(data, []byte("\n"))

	rendered := make([]renderedLine, 0, len(rawLines))
	var st state
	for idx, raw := range rawLines {
		ln := idx + 1
		passthrough := st.blockDepth > 0 || st.rawActive || st.quotedActive || st.continuation

		depth, next, err := scanLine(raw, ln, st)
		if err != nil {
			return nil, err
		}
		st = next

		if passthrough {
			rendered = append(rendered, renderedLine{text: string(raw), passthrough: true})
			continue
		}
		text := renderLine(raw, depth)
		rendered = append(rendered, renderedLine{text: text, blank: text == ""})
	}

	if st.blockDepth > 0 {
		return nil, fmt.Errorf("kdlfmt: line %d: unterminated block comment", st.blockStart)
	}
	if st.rawActive {
		return nil, fmt.Errorf("kdlfmt: line %d: unterminated raw string", st.rawStart)
	}
	if st.quotedActive {
		return nil, fmt.Errorf("kdlfmt: line %d: unterminated string", st.quotedStart)
	}

	var lines []string
	pendingBlank := 0
	for _, r := range rendered {
		if !r.passthrough && r.blank {
			pendingBlank++
			continue
		}
		// Flush at most one blank line for the run just closed — but only
		// once something real has already been emitted; a leading run (or
		// one that runs all the way to EOF, handled by simply never
		// flushing pendingBlank below) is dropped outright, per transform 3.
		if len(lines) > 0 && pendingBlank > 0 {
			lines = append(lines, "")
		}
		pendingBlank = 0
		lines = append(lines, r.text)
	}

	if len(lines) == 0 {
		return []byte{}, nil
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// renderLine applies transforms 1 and 2 to one non-passthrough line: the
// content (original leading/trailing whitespace stripped) reindented to
// depth levels of indentUnit. A blank line (no content once whitespace is
// stripped) renders as "" — indenting an otherwise-empty line would just be
// more trailing whitespace, which transform 2 forbids.
func renderLine(raw []byte, depth int) string {
	// scanLine refuses (returns an error) before depth can go negative —
	// see its "negative depth" case — so depth here is always >= 0.
	content := strings.Trim(string(raw), " \t\r")
	if content == "" {
		return ""
	}
	return strings.Repeat(indentUnit, depth) + content
}
