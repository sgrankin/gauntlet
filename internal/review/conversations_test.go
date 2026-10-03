package review

import (
	"context"
	"testing"
)

func threadPage(more bool, resolved ...bool) any {
	nodes := make([]any, 0, len(resolved))
	for _, r := range resolved {
		nodes = append(nodes, map[string]bool{"isResolved": r})
	}
	return map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
		"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": "next"},
	}}}}}
}

func TestGitHubConversationGate(t *testing.T) {
	for _, tc := range []struct {
		name                string
		pages               []any
		eligible, wantError bool
	}{
		{"no threads", []any{threadPage(false)}, true, false},
		{"resolved", []any{threadPage(false, true)}, true, false},
		{"unresolved", []any{threadPage(false, false)}, false, false},
		{"later unresolved", []any{threadPage(true, true), threadPage(false, false)}, false, false},
		{"all pages resolved", []any{threadPage(true, true), threadPage(false, true)}, true, false},
		{"nonadvancing cursor", []any{threadPage(true, true), threadPage(true, true)}, false, true},
		{"GraphQL error with partial data", []any{map[string]any{"errors": []any{map[string]string{"message": "forbidden"}}, "data": threadPage(false).(map[string]any)["data"]}}, false, true},
		{"missing PR", []any{map[string]any{"data": nil}}, false, true},
		{"missing thread data", []any{map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{}}}}}}, false, true},
		{"transport failure", nil, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitHubFixture(t)
			f.g.p.Approvals = 0
			f.approvals[1] = false
			f.g.p.RequireResolvedConversations = true
			f.threadPages = tc.pages
			f.request(1, "@gauntlet merge")
			// HTTP failure is deliberately injected without fixture diagnostics.
			if tc.name == "transport failure" {
				f.threadStatus = 403
			}
			cs, err := f.g.Candidates(context.Background())
			if (err != nil) != tc.wantError || (len(cs) == 1) != tc.eligible {
				t.Fatalf("candidates=%+v err=%v", cs, err)
			}
			if len(tc.pages) == 2 && (len(f.threadCursors) != 2 || f.threadCursors[1] != "next") {
				t.Fatalf("pagination cursor lost: %v", f.threadCursors)
			}
		})
	}
}

func TestGitHubConversationEnterpriseEndpoint(t *testing.T) {
	f := newGitHubFixture(t)
	f.g.p.APIURL += "/api/v3"
	f.threadPages = []any{threadPage(false)}
	resolved, err := f.g.conversationsResolved(context.Background(), 1)
	if err != nil || !resolved {
		t.Fatalf("enterprise GraphQL endpoint: %v %v", resolved, err)
	}
}

func TestGitHubConversationResolutionRecheckedBeforeLanding(t *testing.T) {
	f := newGitHubFixture(t)
	f.g.p.RequireResolvedConversations = true
	f.threadPages = []any{threadPage(false, true), threadPage(false, false)}
	f.request(1, "@gauntlet merge")
	cs, err := f.g.Candidates(context.Background())
	if err != nil || len(cs) != 1 {
		t.Fatalf("resolved PR not admitted: %+v %v", cs, err)
	}
	if err := f.g.Validate(context.Background(), cs[0]); err == nil {
		t.Fatal("reopened conversation accepted before landing")
	}
}

func TestGitHubConversationGateDefaultsOff(t *testing.T) {
	f := newGitHubFixture(t)
	f.g.p.Approvals = 0
	f.approvals[1] = false
	f.request(1, "@gauntlet merge")
	cs, err := f.g.Candidates(context.Background())
	if err != nil || len(cs) != 1 || f.threadCalls != 0 {
		t.Fatalf("optional gate queried or zero approvals rejected: %+v %v", cs, err)
	}
}
