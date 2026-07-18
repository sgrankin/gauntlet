package main

import (
	"strings"
	"testing"
)

// TestUnifiedDiff exercises diffLines/buildHunks directly with cases
// fmt_test.go's -d tests don't isolate on their own: pure insertion, pure
// deletion, and two separate change regions far enough apart to produce
// two distinct hunks.
func TestUnifiedDiff(t *testing.T) {
	tests := []struct {
		name         string
		before       string
		after        string
		wantContains []string
		hunkCount    int
	}{
		{
			name:         "pure insertion",
			before:       "a\nb\n",
			after:        "a\nx\nb\n",
			wantContains: []string{"+x"},
			hunkCount:    1,
		},
		{
			name:         "pure deletion",
			before:       "a\nx\nb\n",
			after:        "a\nb\n",
			wantContains: []string{"-x"},
			hunkCount:    1,
		},
		{
			name:         "modification",
			before:       "a\nb\nc\n",
			after:        "a\nB\nc\n",
			wantContains: []string{"-b", "+B"},
			hunkCount:    1,
		},
		{
			name: "two far-apart changes produce two hunks",
			before: "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n" +
				"11\n12\n13\n14\n15\n16\n17\n18\n19\n20\n",
			after: "1\n2\nCHANGED\n4\n5\n6\n7\n8\n9\n10\n" +
				"11\n12\n13\n14\n15\n16\n17\n18\nCHANGED2\n20\n",
			wantContains: []string{"-3", "+CHANGED", "-19", "+CHANGED2"},
			hunkCount:    2,
		},
		{
			name:         "no change produces no hunks",
			before:       "a\nb\n",
			after:        "a\nb\n",
			wantContains: nil,
			hunkCount:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := diffLines(splitLinesKeepingNone([]byte(tt.before)), splitLinesKeepingNone([]byte(tt.after)))
			hunks := buildHunks(ops, unifiedDiffContext)
			if len(hunks) != tt.hunkCount {
				t.Fatalf("got %d hunks, want %d", len(hunks), tt.hunkCount)
			}

			out := string(unifiedDiff("f.kdl", []byte(tt.before), []byte(tt.after)))
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("diff output missing %q:\n%s", want, out)
				}
			}
			if tt.hunkCount == 0 && strings.Contains(out, "@@") {
				t.Errorf("expected no hunks but found one:\n%s", out)
			}
		})
	}
}

// TestUnifiedDiffHeaders checks the file-header lines (--- a.orig / +++ a).
func TestUnifiedDiffHeaders(t *testing.T) {
	out := string(unifiedDiff("path/to/f.kdl", []byte("a\n"), []byte("b\n")))
	if !strings.HasPrefix(out, "--- path/to/f.kdl.orig\n+++ path/to/f.kdl\n") {
		t.Fatalf("unexpected header:\n%s", out)
	}
}
