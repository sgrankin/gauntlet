package queue

import (
	"strings"
	"testing"
)

func TestBuildMergeMessage(t *testing.T) {
	cases := []struct {
		name     string
		tmpl     string
		fields   messageFields
		wantSubj string
	}{
		{
			name:     "default with user",
			fields:   messageFields{Topic: "widget", User: "alice", Ref: "refs/heads/for/main/alice/widget", RunID: "run1", Target: "main"},
			wantSubj: "Merge widget (alice)",
		},
		{
			name:     "default degrades to omit an empty user (§9.3)",
			fields:   messageFields{Topic: "widget", User: "", Ref: "refs/heads/for/main/widget", RunID: "run1", Target: "main"},
			wantSubj: "Merge widget",
		},
		{
			name:     "custom template used as-is even with an empty user",
			tmpl:     "Land {{.Topic}} for {{.Target}} <{{.User}}>",
			fields:   messageFields{Topic: "widget", User: "", Ref: "r", RunID: "run1", Target: "main"},
			wantSubj: "Land widget for main <>",
		},
		{
			name:     "custom template with user",
			tmpl:     "Merge {{.Topic}} ({{.User}})",
			fields:   messageFields{Topic: "widget", User: "bob", Ref: "r", RunID: "run1", Target: "main"},
			wantSubj: "Merge widget (bob)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildMergeMessage(tc.tmpl, tc.fields, "")
			if err != nil {
				t.Fatalf("buildMergeMessage: %v", err)
			}
			subject := strings.SplitN(got, "\n", 2)[0]
			if subject != tc.wantSubj {
				t.Errorf("subject = %q, want %q", subject, tc.wantSubj)
			}
			if !strings.Contains(got, "Gauntlet-Ref: "+tc.fields.Ref) {
				t.Errorf("message missing Gauntlet-Ref trailer:\n%s", got)
			}
			if !strings.Contains(got, "Gauntlet-Run: "+tc.fields.RunID) {
				t.Errorf("message missing Gauntlet-Run trailer:\n%s", got)
			}
		})
	}
}

func TestBuildMergeMessage_InvalidTemplate(t *testing.T) {
	if _, err := buildMergeMessage("{{.Nope", messageFields{}, ""); err == nil {
		t.Fatal("buildMergeMessage: want error for an unparseable template")
	}
}

// TestBuildMergeMessage_EmptyBodyMatchesPriorShape locks in that an empty
// body (Config.MergeBody nil, or returning "") produces byte-for-byte:
// subject, blank line, trailers, nothing else in between.
func TestBuildMergeMessage_EmptyBodyMatchesPriorShape(t *testing.T) {
	f := messageFields{Topic: "widget", User: "alice", Ref: "refs/heads/for/main/alice/widget", RunID: "run1", Target: "main"}
	got, err := buildMergeMessage("", f, "")
	if err != nil {
		t.Fatalf("buildMergeMessage: %v", err)
	}
	want := "Merge widget (alice)\n\nGauntlet-Ref: refs/heads/for/main/alice/widget\nGauntlet-Run: run1\n"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

// TestBuildMergeMessage_BodyBetweenSubjectAndTrailers is the contract: a
// non-empty body lands between the subject and the trailers,
// each blank-line separated (standard git message shape), and the
// trailers still parse (still the last two lines, still "Key: value").
func TestBuildMergeMessage_BodyBetweenSubjectAndTrailers(t *testing.T) {
	f := messageFields{Topic: "widget", User: "alice", Ref: "refs/heads/for/main/alice/widget", RunID: "run1", Target: "main"}
	got, err := buildMergeMessage("", f, "  Adds widget rendering support.  \n")
	if err != nil {
		t.Fatalf("buildMergeMessage: %v", err)
	}
	want := "Merge widget (alice)\n\nAdds widget rendering support.\n\nGauntlet-Ref: refs/heads/for/main/alice/widget\nGauntlet-Run: run1\n"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}

	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if lines[len(lines)-2] != "Gauntlet-Ref: refs/heads/for/main/alice/widget" {
		t.Errorf("trailers not at the end: %q", got)
	}
	if lines[len(lines)-1] != "Gauntlet-Run: run1" {
		t.Errorf("trailers not at the end: %q", got)
	}
}

// TestBuildMergeMessage_WhitespaceOnlyBodyOmitted treats a
// whitespace-only body (e.g. a summarizer that degraded to blanks) the
// same as an empty one — trimmed to nothing, so it's dropped rather than
// leaving a stray blank paragraph.
func TestBuildMergeMessage_WhitespaceOnlyBodyOmitted(t *testing.T) {
	f := messageFields{Topic: "widget", Ref: "r", RunID: "run1", Target: "main"}
	got, err := buildMergeMessage("", f, "   \n\n  ")
	if err != nil {
		t.Fatalf("buildMergeMessage: %v", err)
	}
	want := "Merge widget\n\nGauntlet-Ref: r\nGauntlet-Run: run1\n"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}
