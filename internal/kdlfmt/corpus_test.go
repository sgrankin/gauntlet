package kdlfmt

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
)

// repoRoot is two levels up from this package, same convention as
// internal/config/config_test.go's exampleDaemonPath/exampleChecksPath.
const repoRoot = "../.."

// kdlFixture is one document this test formats and checks: either a real
// .kdl file in the repo, or one ```kdl fence extracted from a docs/*.md
// file (name records both the source file and, for a fence, its ordinal
// position in that file, so a failure names something findable).
type kdlFixture struct {
	name string
	data []byte
}

// corpusFixtures gathers every KDL fixture the package doc's
// semantics-preserving property is checked against: the repo's own example
// configs (gauntlet.kdl, .gauntlet.kdl) and every ```kdl fence in
// docs/*.md. It deliberately does NOT reach into internal/queue/testdata's
// txtar scenario files — those embed trivial, repetitive check specs as
// scenario INPUT, not documentation of the config language, and are
// exercised by the queue's own scenario tests instead.
func corpusFixtures(t *testing.T) []kdlFixture {
	t.Helper()
	var fixtures []kdlFixture

	for _, rel := range []string{"gauntlet.kdl", ".gauntlet.kdl"} {
		path := filepath.Join(repoRoot, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		fixtures = append(fixtures, kdlFixture{name: rel, data: data})
	}

	docsDir := filepath.Join(repoRoot, "docs")
	err := filepath.WalkDir(docsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		for i, fence := range extractKDLFences(t, path) {
			fixtures = append(fixtures, kdlFixture{
				name: fmt.Sprintf("%s#kdl-fence-%d", rel, i+1),
				data: fence,
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", docsDir, err)
	}
	// The repo has 2 root .kdl files and ~20 docs fences today; a floor
	// well above the .kdl count alone means a broken fence extractor
	// cannot silently pass on just the root configs.
	if len(fixtures) < 15 {
		t.Fatalf("corpusFixtures found only %d fixtures — extraction likely broken", len(fixtures))
	}
	return fixtures
}

// extractKDLFences pulls every ```kdl ... ``` fenced block out of a
// markdown file's raw text, in order, as raw bytes (list indentation and
// all — KDL's tokenizer doesn't care about absolute indentation, only
// brace structure, so re-parsing after Format doesn't depend on stripping
// it).
func extractKDLFences(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var fences [][]byte
	var cur *bytes.Buffer
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		// Prefix match, not equality: an info string may carry attributes
		// after the language ("```kdl title=...") and those fences are
		// still KDL corpus material — silently skipping them would shrink
		// the corpus without any test noticing.
		case cur == nil && (strings.TrimSpace(line) == "```kdl" ||
			strings.HasPrefix(strings.TrimSpace(line), "```kdl ")):
			cur = &bytes.Buffer{}
		case cur != nil && strings.TrimSpace(line) == "```":
			fences = append(fences, cur.Bytes())
			cur = nil
		case cur != nil:
			cur.WriteString(line)
			cur.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	if cur != nil {
		t.Fatalf("%s: unterminated ```kdl fence", path)
	}
	return fences
}

// TestCorpusFormatSucceeds requires every extracted fixture to be valid KDL
// that Format accepts — docs/*.md kdl fences and the repo's own example
// configs are all meant to be genuine, parseable KDL (unlike this file's
// own handwritten TestFormatRefusesInvalidKDL cases), so a fixture Format
// refuses is either a broken doc example or a kdlfmt bug, worth failing
// loudly rather than skipping.
func TestCorpusFormatSucceeds(t *testing.T) {
	for _, fx := range corpusFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			if _, err := Format(fx.data); err != nil {
				t.Fatalf("Format(%s): %v", fx.name, err)
			}
		})
	}
}

// TestCorpusIdempotent extends TestFormatIdempotent's property — fmt(fmt(x))
// == fmt(x) — to every real fixture, not just the handwritten cases.
func TestCorpusIdempotent(t *testing.T) {
	for _, fx := range corpusFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			once, err := Format(fx.data)
			if err != nil {
				t.Fatalf("Format(%s): %v", fx.name, err)
			}
			twice, err := Format(once)
			if err != nil {
				t.Fatalf("Format(Format(%s)): %v", fx.name, err)
			}
			if !bytes.Equal(once, twice) {
				t.Fatalf("%s: not idempotent:\nfirst  = %s\nsecond = %s", fx.name, once, twice)
			}
		})
	}
}

// TestCorpusLinePreserving is the line-preservation property from the
// package doc: output line count == input line count minus removed blank
// lines, and every non-blank input line survives (whitespace-normalized)
// in order. Verified against an independent reference implementation of
// JUST the blank-collapsing rule (collapseBlankLinesRef below), not against
// normalize's own collapsing code — so this doesn't just check normalize
// against itself.
//
// This assumes no fixture has a blank line living INSIDE a multi-line
// block comment or raw string (none in this corpus do): such a line is a
// passthrough line — see the package doc — and is never dropped/collapsed
// regardless of blankness, which collapseBlankLinesRef doesn't model. The
// stronger, fixture-specific claim (every comment byte, blank or not,
// survives untouched) is checked directly by
// TestFormatCommentAndSlashdashRetention instead.
func TestCorpusLinePreserving(t *testing.T) {
	for _, fx := range corpusFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			out, err := Format(fx.data)
			if err != nil {
				t.Fatalf("Format(%s): %v", fx.name, err)
			}

			inLines := splitDroppingFinalNewlineArtifact(fx.data)
			outLines := splitDroppingFinalNewlineArtifact(out)
			want := collapseBlankLinesRef(inLines)

			if len(want) != len(outLines) {
				t.Fatalf("%s: line count = %d, want %d (input %d lines minus collapsed blanks)\ngot:\n%s\nwant:\n%s",
					fx.name, len(outLines), len(want), len(inLines), out, strings.Join(want, "\n"))
			}
			for i := range want {
				if strings.TrimSpace(want[i]) != strings.TrimSpace(outLines[i]) {
					t.Fatalf("%s: line %d = %q, want %q (whitespace-normalized)", fx.name, i+1, outLines[i], want[i])
				}
			}
		})
	}
}

// splitDroppingFinalNewlineArtifact splits data on "\n"; a trailing "\n" in
// data produces a spurious empty final element from bytes.Split (the empty
// tail after the last real line, not a blank line the file actually has),
// which this drops so line counts reflect real lines only.
func splitDroppingFinalNewlineArtifact(data []byte) []string {
	parts := strings.Split(string(data), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// collapseBlankLinesRef is an independent reference implementation of
// transform 3 (collapse 2+ blank lines to 1; drop leading/trailing blank
// runs), used ONLY to compute the expected line count/content for
// TestCorpusLinePreserving — deliberately not shared code with
// normalize's own blank-collapsing pass in kdlfmt.go.
func collapseBlankLinesRef(lines []string) []string {
	var out []string
	pending := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			pending++
			continue
		}
		if len(out) > 0 && pending > 0 {
			out = append(out, "")
		}
		pending = 0
		out = append(out, l)
	}
	return out
}

// TestCorpusSemanticsPreserving is the strong safety property: for every
// fixture that config.LoadDaemon or config.ParseChecks accepts as-is
// (whichever one takes it — a fixture may be a full daemon config, a check
// spec, or, for many docs/*.md fragments, neither, in which case it's
// skipped, per the package doc's contract), the SAME parser accepts the
// Format-ed bytes and produces a deeply-equal struct. Comments/slashdash
// are already proven byte-identical by construction (the line-based
// safety property); this is the complementary proof that reindenting never
// changes what the document MEANS to gauntlet's own parsers.
func TestCorpusSemanticsPreserving(t *testing.T) {
	tmp := t.TempDir()
	tested := 0
	for _, fx := range corpusFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			out, err := Format(fx.data)
			if err != nil {
				t.Fatalf("Format(%s): %v", fx.name, err)
			}

			origPath := filepath.Join(tmp, "orig.kdl")
			if err := os.WriteFile(origPath, fx.data, 0o644); err != nil {
				t.Fatal(err)
			}
			fmtPath := filepath.Join(tmp, "fmt.kdl")
			if err := os.WriteFile(fmtPath, out, 0o644); err != nil {
				t.Fatal(err)
			}

			if origDaemon, err := config.LoadDaemon(origPath); err == nil {
				fmtDaemon, err := config.LoadDaemon(fmtPath)
				if err != nil {
					t.Fatalf("LoadDaemon accepted the original but rejected the formatted output: %v", err)
				}
				if !reflect.DeepEqual(origDaemon, fmtDaemon) {
					t.Fatalf("LoadDaemon result changed by formatting:\nbefore: %+v\nafter:  %+v", origDaemon, fmtDaemon)
				}
				tested++
				return
			}

			if origChecks, err := config.ParseChecks(fx.data); err == nil {
				fmtChecks, err := config.ParseChecks(out)
				if err != nil {
					t.Fatalf("ParseChecks accepted the original but rejected the formatted output: %v", err)
				}
				if !reflect.DeepEqual(origChecks, fmtChecks) {
					t.Fatalf("ParseChecks result changed by formatting:\nbefore: %+v\nafter:  %+v", origChecks, fmtChecks)
				}
				tested++
				return
			}

			t.Skipf("%s is neither a valid daemon config nor a valid check spec on its own; skipped per package doc's contract", fx.name)
		})
	}
	if tested == 0 {
		t.Fatal("no fixture was accepted by LoadDaemon or ParseChecks — semantics-preserving property was never actually exercised")
	}
}
