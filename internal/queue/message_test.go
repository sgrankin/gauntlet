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
			got, err := buildMergeMessage(tc.tmpl, tc.fields)
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
	if _, err := buildMergeMessage("{{.Nope", messageFields{}); err == nil {
		t.Fatal("buildMergeMessage: want error for an unparseable template")
	}
}
