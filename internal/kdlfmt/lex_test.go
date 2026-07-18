package kdlfmt

import (
	"strings"
	"testing"
)

// TestNormalize is the lexer/normalizer's own table-driven suite: each case
// feeds normalize() (the pure whitespace pass, no kdl.Parse gate) directly,
// so malformed inputs exercise the mini-lexer's own error paths rather than
// Format's outer kdl.Parse guard — see TestFormatRefusesInvalidKDL for that
// guard separately. Inputs here are deliberately NOT always syntactically
// complete KDL: normalize only needs to track braces/strings/comments, not
// validate a document, so a few cases below exist purely to exercise that
// tracking (e.g. the multi-close-on-one-line dedent case).
func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		// errContains, if set, means normalize must fail and the error
		// must mention this substring (typically "line N: ...").
		errContains string
	}{
		{
			name: "reindent and trailing whitespace",
			in:   "a {\n  b 1   \n}\n",
			want: "a {\n    b 1\n}\n",
		},
		{
			name: "nested depth, one close per line",
			in:   "a {\nb {\nc 1\n}\n}\n",
			want: "a {\n    b {\n        c 1\n    }\n}\n",
		},
		{
			// The formatter never splits a line (the package doc's safety
			// property), so two closes sharing one input line dedent that
			// SAME line by two levels, at whatever depth remains after
			// both — it doesn't become two lines.
			name: "two leading closes sharing one line dedent by two, stay one line",
			in:   "a {\nb {\nc {\nd 1\n} }\nz 1\n}\n",
			want: "a {\n    b {\n        c {\n            d 1\n    } }\n    z 1\n}\n",
		},
		{
			name: "adjacent closes with no space, stay one line",
			in:   "a {\nb {\nc {\nd 1\n}}\nz 1\n}\n",
			want: "a {\n    b {\n        c {\n            d 1\n    }}\n    z 1\n}\n",
		},
		{
			name: "blank run mid-file collapses to one",
			in:   "a 1\n\n\n\nb 2\n",
			want: "a 1\n\nb 2\n",
		},
		{
			name: "leading blank lines dropped",
			in:   "\n\n\na 1\n",
			want: "a 1\n",
		},
		{
			name: "trailing blank lines dropped",
			in:   "a 1\n\n\n\n",
			want: "a 1\n",
		},
		{
			name: "single blank line mid-file untouched",
			in:   "a 1\n\nb 2\n",
			want: "a 1\n\nb 2\n",
		},
		{
			name: "final newline added when missing",
			in:   "a 1",
			want: "a 1\n",
		},
		{
			name: "single final newline when already present",
			in:   "a 1\n",
			want: "a 1\n",
		},
		{
			name: "empty file stays empty",
			in:   "",
			want: "",
		},
		{
			name: "all-blank file collapses to empty",
			in:   "\n\n\n",
			want: "",
		},
		{
			name: "line comment braces are not structural",
			in:   "a { // comment with { and } inside\n    b 1\n}\n",
			want: "a { // comment with { and } inside\n    b 1\n}\n",
		},
		{
			name: "block comment nested and spanning lines, interior untouched",
			in: "a {\n" +
				"/* outer\n" +
				"  extra   spaces kept  \n" +
				"still outer */\n" +
				"b 1\n" +
				"}\n",
			want: "a {\n" +
				"    /* outer\n" +
				"  extra   spaces kept  \n" +
				"still outer */\n" +
				"    b 1\n" +
				"}\n",
		},
		{
			name: "quoted string with escaped quote and braces is not structural",
			in:   "a {\n  b \"it's a \\\"trap\\\" { not really }\"\n}\n",
			want: "a {\n    b \"it's a \\\"trap\\\" { not really }\"\n}\n",
		},
		{
			name: "raw string braces are not structural",
			in:   "a {\n  command r#\"{ not { a } brace \"quote\" }\"#   \n}\n",
			want: "a {\n    command r#\"{ not { a } brace \"quote\" }\"#\n}\n",
		},
		{
			name: "raw string with zero hashes",
			in:   `command r"{ brace }"` + "\n",
			want: `command r"{ brace }"` + "\n",
		},
		{
			name: "multi-line raw string interior kept byte-identical",
			in: "a {\n" +
				"    command r#\"multi\n" +
				"line   raw\n" +
				"string\"#\n" +
				"}\n",
			want: "a {\n" +
				"    command r#\"multi\n" +
				"line   raw\n" +
				"string\"#\n" +
				"}\n",
		},
		{
			name: "bareword starting with r is not a raw string",
			in:   "a {\n  run 1\n}\n",
			want: "a {\n    run 1\n}\n",
		},
		{
			// A '"' followed by too few '#' to match the delimiter is raw-
			// string CONTENT, not a close — the seek must reset and resume
			// scanning normally (mirrors kdl-go's own raw-string reader).
			name: "false close attempt inside a raw string is content, not a close",
			in:   "a {\n    command r##\"text \"# still inside\"##\n}\n",
			want: "a {\n    command r##\"text \"# still inside\"##\n}\n",
		},
		{
			// The closing quote+hashes themselves may straddle a line
			// break: line3 begins inside the still-open raw string (state
			// carried from line2's end), so it's a passthrough line even
			// though the string closes two characters into it.
			name: "raw string closing delimiter split across a line boundary",
			in: "a {\n" +
				"    command r##\"line one ends with quote\"\n" +
				"##\n" +
				"}\n",
			want: "a {\n" +
				"    command r##\"line one ends with quote\"\n" +
				"##\n" +
				"}\n",
		},
		{
			name: "slashdash node braces still count for depth",
			in:   "/-a {\n  b 1\n}\n",
			want: "/-a {\n    b 1\n}\n",
		},
		{
			name: "line continuation passes the next line through untouched",
			in:   "a 1 \\\n    weird   spacing kept\nb 2\n",
			want: "a 1 \\\n    weird   spacing kept\nb 2\n",
		},
		{
			name: "line continuation preserves wrong indentation on the continued line",
			in:   "a 1 \\\nnot reindented at all\nb 2\n",
			want: "a 1 \\\nnot reindented at all\nb 2\n",
		},
		{
			name:        "unterminated quoted string at EOL",
			in:          "a \"no closing quote\nb 1\n",
			errContains: "line 1: unterminated string",
		},
		{
			name:        "unterminated block comment at EOF",
			in:          "a {\n/* never closed\nb 1\n}\n",
			errContains: "line 2: unterminated block comment",
		},
		{
			name:        "unterminated raw string at EOF",
			in:          "a r#\"never closed\n",
			errContains: "line 1: unterminated raw string",
		},
		{
			name:        "unmatched closing brace goes negative",
			in:          "a 1\n}\n",
			errContains: "line 2: unmatched '}'",
		},
		{
			name:        "unmatched closing brace on the very first line",
			in:          "}\n",
			errContains: "line 1: unmatched '}'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalize([]byte(tt.in))
			if tt.errContains != "" {
				if err == nil {
					t.Fatalf("normalize(%q) = %q, nil; want error containing %q", tt.in, got, tt.errContains)
				}
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("normalize(%q) error = %q; want it to contain %q", tt.in, err, tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize(%q) unexpected error: %v", tt.in, err)
			}
			if string(got) != tt.want {
				t.Fatalf("normalize(%q) =\n%q\nwant\n%q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNormalizeIdempotent checks fmt(fmt(x)) == fmt(x) directly at the
// lexer level for every non-error case above — the formatted form of a
// formatted form must be a no-op, since a second pass sees only 4-space
// indentation and no trailing whitespace to begin with. (Format-level
// idempotency, through the kdl.Parse guards, is covered separately in
// kdlfmt_test.go and corpus_test.go.)
func TestNormalizeIdempotent(t *testing.T) {
	cases := []string{
		"a {\n  b 1   \n}\n",
		"a {\nb {\nc 1\n}\n}\n",
		"a 1\n\n\n\nb 2\n",
		"a { // comment with { and } inside\n    b 1\n}\n",
		"a {\n/* outer\n  extra   spaces kept  \nstill outer */\nb 1\n}\n",
		"a {\n  command r#\"{ not { a } brace \"quote\" }\"#   \n}\n",
		"/-a {\n  b 1\n}\n",
		"a 1 \\\n    weird   spacing kept\nb 2\n",
	}
	for _, in := range cases {
		once, err := normalize([]byte(in))
		if err != nil {
			t.Fatalf("normalize(%q): %v", in, err)
		}
		twice, err := normalize(once)
		if err != nil {
			t.Fatalf("normalize(normalize(%q)): %v", in, err)
		}
		if string(once) != string(twice) {
			t.Fatalf("not idempotent for %q:\nfirst  = %q\nsecond = %q", in, once, twice)
		}
	}
}
