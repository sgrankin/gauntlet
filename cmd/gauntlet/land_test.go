package main

import "testing"

// Argv/refspec construction only — no exec, no git, no network: tests must
// not run git for real.

func TestLandRefspec(t *testing.T) {
	got := landRefspec("main", "alice", "feat/foo")
	want := "HEAD:refs/heads/for/main/alice/feat/foo"
	if got != want {
		t.Errorf("landRefspec() = %q, want %q", got, want)
	}
}

func TestTargetFromRemoteHead(t *testing.T) {
	cases := []struct {
		symref, remote string
		want           string // "" ⇒ error expected
	}{
		{"origin/main", "origin", "main"},
		{"origin/release/v2", "origin", ""}, // slashed branch can't name a target
		{"upstream/main", "origin", ""},
		{"origin/", "origin", ""},
		{"main", "origin", ""},
	}
	for _, c := range cases {
		got, err := targetFromRemoteHead(c.symref, c.remote)
		if c.want == "" {
			if err == nil {
				t.Errorf("targetFromRemoteHead(%q, %q) = %q, want error", c.symref, c.remote, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("targetFromRemoteHead(%q, %q) = %q, %v, want %q", c.symref, c.remote, got, err, c.want)
		}
	}
}

func TestTopicFromRefs(t *testing.T) {
	cases := []struct {
		name   string
		refs   []string
		target string
		want   string // "" ⇒ error expected
	}{
		{"none", nil, "main", ""},
		{"one", []string{"refs/heads/my-feature"}, "main", "my-feature"},
		{"slashed topic", []string{"refs/heads/feat/foo"}, "main", "feat/foo"},
		{"only the target", []string{"refs/heads/main"}, "main", ""},
		{"target plus one", []string{"refs/heads/main", "refs/heads/my-feature"}, "main", "my-feature"},
		{"ambiguous", []string{"refs/heads/a", "refs/heads/b"}, "main", ""},
		{"non-branch ref ignored", []string{"refs/tags/v1", "refs/heads/my-feature"}, "main", "my-feature"},
	}
	for _, c := range cases {
		got, err := topicFromRefs(c.refs, c.target)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: topicFromRefs(%v, %q) = %q, want error", c.name, c.refs, c.target, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: topicFromRefs(%v, %q) = %q, %v, want %q", c.name, c.refs, c.target, got, err, c.want)
		}
	}
}

func TestSlugifyUser(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Alice Smith", "alice-smith"},
		{"alice", "alice"},
		{"  Bob!! ", "bob"},
		{"O'Brien_Jr.", "o-brien-jr"},
		{"", ""},
	}
	for _, c := range cases {
		if got := slugifyUser(c.in); got != c.want {
			t.Errorf("slugifyUser(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
