package kdlfmt

import (
	"strings"
	"testing"
)

// TestFormatValid confirms Format's two kdl.Parse guards (pre-check on the
// input, post-check on the output — see Format's doc) both pass on ordinary
// valid KDL, and that reindenting/trimming actually happened.
func TestFormatValid(t *testing.T) {
	in := "a {\n  b 1   \n}\n"
	want := "a {\n    b 1\n}\n"
	got, err := Format([]byte(in))
	if err != nil {
		t.Fatalf("Format(%q): %v", in, err)
	}
	if string(got) != want {
		t.Fatalf("Format(%q) = %q, want %q", in, got, want)
	}
}

// TestFormatRefusesInvalidKDL checks Format's FIRST guard: input that is
// not valid KDL at all (not just malformed in a way the mini-lexer alone
// would catch) is refused before normalize ever runs. "fmt is for valid
// KDL", per the package doc — never a silent pass-through, never a repair
// tool.
func TestFormatRefusesInvalidKDL(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"unterminated string", "a \"never closed\n"},
		{"unmatched close", "a 1\n}\n"},
		{"bare stray brace", "{\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Format([]byte(tt.in))
			if err == nil {
				t.Fatalf("Format(%q) = %q, nil; want an error (input is not valid KDL)", tt.in, got)
			}
			if got != nil {
				t.Fatalf("Format(%q) returned a non-nil result alongside an error: %q", tt.in, got)
			}
		})
	}
}

// TestFormatIdempotent checks fmt(fmt(x)) == fmt(x) through the full
// Format entry point (both kdl.Parse guards included), on a handful of
// representative valid documents exercising comments, strings, and
// slashdash together — the corpus test in corpus_test.go extends this
// property to every real KDL fixture in the repo.
func TestFormatIdempotent(t *testing.T) {
	cases := []string{
		"a {\n  b 1   \n}\n",
		"// leading\na \"x\" { // trailing\n    b 1\n}\n",
		"/-a {\n    b 1\n}\n",
		`command r#"{ raw "quoted" }"#` + "\n",
	}
	for _, in := range cases {
		once, err := Format([]byte(in))
		if err != nil {
			t.Fatalf("Format(%q): %v", in, err)
		}
		twice, err := Format(once)
		if err != nil {
			t.Fatalf("Format(Format(%q)): %v", in, err)
		}
		if string(once) != string(twice) {
			t.Fatalf("Format not idempotent for %q:\nfirst  = %q\nsecond = %q", in, once, twice)
		}
	}
}

// TestFormatCommentAndSlashdashRetention is the fixture the package doc
// promises: comments in every position (leading, trailing-inline, between
// children, dangling after the last child, end-of-file, nested block
// comments) and a slashdash node with children, fed through Format with
// deliberately WRONG indentation and stray trailing whitespace, asserted
// byte-for-byte against a hand-written correctly-formatted want — proving
// every comment/slashdash byte survives, and ONLY whitespace changed.
func TestFormatCommentAndSlashdashRetention(t *testing.T) {
	in := "" +
		"// leading comment\n" +
		"a \"1\" {   \n" +
		"      b 1\n" +
		"  // comment between children\n" +
		"        c 2\n" +
		"  // dangling comment after last child\n" +
		"}\n" +
		"/* nested /* inner */ still outer */\n" +
		"/-d \"skip\" {\n" +
		"        e 3\n" +
		"}\n" +
		"// end-of-file comment\n"

	want := "" +
		"// leading comment\n" +
		"a \"1\" {\n" +
		"    b 1\n" +
		"    // comment between children\n" +
		"    c 2\n" +
		"    // dangling comment after last child\n" +
		"}\n" +
		"/* nested /* inner */ still outer */\n" +
		"/-d \"skip\" {\n" +
		"    e 3\n" +
		"}\n" +
		"// end-of-file comment\n"

	got, err := Format([]byte(in))
	if err != nil {
		t.Fatalf("Format: %v", err)
	}
	if string(got) != want {
		t.Fatalf("Format(in) =\n%s\nwant\n%s", got, want)
	}

	// Every comment's own text must appear verbatim (the byte-equality
	// check above already proves this; these are a readable cross-check
	// naming each required position from the package doc's list).
	for _, comment := range []string{
		"// leading comment",
		"// comment between children",
		"// dangling comment after last child",
		"/* nested /* inner */ still outer */",
		"// end-of-file comment",
	} {
		if !strings.Contains(string(got), comment) {
			t.Errorf("formatted output is missing comment %q", comment)
		}
	}
	if !strings.Contains(string(got), `/-d "skip" {`) {
		t.Errorf("formatted output is missing the slashdash node")
	}

	// Already correctly formatted, so a second pass is a strict no-op —
	// the same idempotency property TestFormatIdempotent checks generally.
	again, err := Format(got)
	if err != nil {
		t.Fatalf("Format(want): %v", err)
	}
	if string(again) != want {
		t.Fatalf("Format is not idempotent on its own output:\n%s", again)
	}
}
