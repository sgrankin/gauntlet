package queue

import "testing"

// TestParseCandidateRef is the §9.3 ref-grammar table test: every shape the
// grammar defines (solo, user+topic, user+topic-with-slashes) plus the
// malformed shapes that must be rejected.
func TestParseCandidateRef(t *testing.T) {
	cases := []struct {
		ref                             string
		wantTarget, wantUser, wantTopic string
		wantOK                          bool
	}{
		{ref: "refs/heads/for/main/alice/widget", wantTarget: "main", wantUser: "alice", wantTopic: "widget", wantOK: true},
		{ref: "refs/heads/for/main/alice/feat/foo", wantTarget: "main", wantUser: "alice", wantTopic: "feat/foo", wantOK: true},
		{ref: "refs/heads/for/main/widget", wantTarget: "main", wantUser: "", wantTopic: "widget", wantOK: true}, // solo, no user segment
		{ref: "refs/heads/for/release-v2/bob/deep/nested/topic", wantTarget: "release-v2", wantUser: "bob", wantTopic: "deep/nested/topic", wantOK: true},

		{ref: "refs/heads/for/main/", wantOK: false},         // no topic at all
		{ref: "refs/heads/for/main", wantOK: false},          // no rest after target
		{ref: "refs/heads/for/", wantOK: false},              // no target, no rest
		{ref: "refs/heads/for//alice/widget", wantOK: false}, // empty target
		{ref: "refs/heads/foo/bar", wantOK: false},           // wrong prefix entirely
		{ref: "refs/heads/for/main/alice/", wantOK: false},   // empty topic after a user segment
		{ref: "refs/heads/for/main//widget", wantOK: false},  // empty user segment
		{ref: "", wantOK: false},                             // empty ref
		{ref: "refs/heads/main", wantOK: false},              // an ordinary target branch, not a candidate
	}
	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			target, user, topic, ok := parseCandidateRef(tc.ref)
			if ok != tc.wantOK {
				t.Fatalf("parseCandidateRef(%q) ok = %v, want %v", tc.ref, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if target != tc.wantTarget || user != tc.wantUser || topic != tc.wantTopic {
				t.Errorf("parseCandidateRef(%q) = (%q,%q,%q), want (%q,%q,%q)",
					tc.ref, target, user, topic, tc.wantTarget, tc.wantUser, tc.wantTopic)
			}
		})
	}
}
