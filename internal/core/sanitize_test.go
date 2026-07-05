package core

import "testing"

func TestSanitizeName_OnlyAllowedCharsSurviveUnescaped(t *testing.T) {
	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-"
	in := allowed + " /:@!#$%^&*()+={}[]|\\;\"'<>,?~`"
	got := SanitizeName(in)
	for i, r := range got {
		if i < len(allowed) {
			continue // exact-match check below
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
		default:
			t.Fatalf("SanitizeName produced disallowed rune %q in %q", r, got)
		}
	}
	if got[:len(allowed)] != allowed {
		t.Fatalf("SanitizeName must preserve the allowed prefix verbatim, got %q", got)
	}
}

func TestSanitizeName_StripsPathSeparators(t *testing.T) {
	cases := []struct{ in, want string }{
		{"../evil", "..-evil"},
		{"../../etc/passwd", "..-..-etc-passwd"},
		{"a/b/c", "a-b-c"},
		{"plain-name", "plain-name"},
		{"", ""},
	}
	for _, c := range cases {
		if got := SanitizeName(c.in); got != c.want {
			t.Errorf("SanitizeName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
