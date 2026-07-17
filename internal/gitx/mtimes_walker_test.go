package gitx

import (
	"strings"
	"testing"
)

// White-box tests for the log walker's NUL-token framing: the property
// under test is that PATHS ARE OPAQUE BYTES — 0x01 (the header sentinel)
// and newlines inside a path must parse as data, because position, not
// content, decides what a token is.

func TestLogWalker_Framing(t *testing.T) {
	// Three records: an ordinary commit, a root commit whose only entry's
	// path contains both \x01 and \n, and a two-parent merge entry with a
	// rename.
	stream := "\x01aaa bbb 100\x00\nM\x00normal.txt\x00A\x00second\x00" +
		"\x01ccc 200\x00\nA\x00we\x01ird\nname\x00" +
		"\x01ddd eee fff 300\x00\nR100\x00old.txt\x00new.txt\x00"
	w := newLogWalker(strings.NewReader(stream))

	r1, ok, err := w.next()
	if err != nil || !ok {
		t.Fatalf("next 1: ok=%v err=%v", ok, err)
	}
	if r1.hash != "aaa" || r1.parents != 1 || r1.time.Unix() != 100 {
		t.Errorf("r1 = %q parents=%d ct=%d, want aaa/1/100", r1.hash, r1.parents, r1.time.Unix())
	}
	if len(r1.paths) != 2 || r1.paths[0] != "normal.txt" || r1.paths[1] != "second" {
		t.Errorf("r1.paths = %q", r1.paths)
	}

	r2, ok, err := w.next()
	if err != nil || !ok {
		t.Fatalf("next 2: ok=%v err=%v", ok, err)
	}
	if r2.hash != "ccc" || r2.parents != 0 {
		t.Errorf("r2 = %q parents=%d, want ccc/0 (root)", r2.hash, r2.parents)
	}
	if len(r2.paths) != 1 || r2.paths[0] != "we\x01ird\nname" {
		t.Errorf("r2.paths = %q, want the raw control-byte path", r2.paths)
	}

	r3, ok, err := w.next()
	if err != nil || !ok {
		t.Fatalf("next 3: ok=%v err=%v", ok, err)
	}
	if r3.hash != "ddd" || r3.parents != 2 {
		t.Errorf("r3 = %q parents=%d, want ddd/2", r3.hash, r3.parents)
	}
	if len(r3.paths) != 1 || r3.paths[0] != "new.txt" {
		t.Errorf("r3.paths = %q, want the rename's NEW path only", r3.paths)
	}

	if _, ok, err := w.next(); ok || err != nil {
		t.Fatalf("next 4: ok=%v err=%v, want clean EOF", ok, err)
	}
}

func TestLogWalker_HeaderOnlyRecord(t *testing.T) {
	// An empty commit (or a merge all of whose parent diffs were
	// suppressed) emits a bare header with no \n and no entries.
	stream := "\x01aaa bbb 100\x00" + "\x01ccc ddd 200\x00\nM\x00f\x00"
	w := newLogWalker(strings.NewReader(stream))
	r1, ok, err := w.next()
	if err != nil || !ok {
		t.Fatalf("next 1: ok=%v err=%v", ok, err)
	}
	if r1.hash != "aaa" || len(r1.paths) != 0 {
		t.Errorf("r1 = %q paths=%q, want aaa with no paths", r1.hash, r1.paths)
	}
	r2, ok, err := w.next()
	if err != nil || !ok {
		t.Fatalf("next 2: ok=%v err=%v", ok, err)
	}
	if r2.hash != "ccc" || len(r2.paths) != 1 {
		t.Errorf("r2 = %q paths=%q", r2.hash, r2.paths)
	}
}

func TestLogWalker_DesyncFailsLoudly(t *testing.T) {
	cases := []struct {
		name   string
		stream string
	}{
		{"garbage in status position", "\x01aaa bbb 100\x00\nM\x00f\x00not-a-status\x00g\x00"},
		{"stream not starting at a header", "M\x00f\x00"},
		{"header missing committer time", "\x01aaa\x00"},
		{"truncated entry after status", "\x01aaa bbb 100\x00\nM\x00"},
		{"truncated rename entry", "\x01aaa bbb 100\x00\nR100\x00old\x00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newLogWalker(strings.NewReader(tc.stream))
			for {
				_, ok, err := w.next()
				if err != nil {
					return // loud failure: what the contract wants
				}
				if !ok {
					t.Fatal("walker reached clean EOF on a malformed stream, want an error")
				}
			}
		})
	}
}
