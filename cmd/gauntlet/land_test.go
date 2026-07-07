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
