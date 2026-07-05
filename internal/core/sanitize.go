package core

import "strings"

// SanitizeName replaces every rune outside [A-Za-z0-9_.-] with '-' — the
// portable-safe subset shared by container runtime names (docker/podman/
// Apple container, internal/executor's containerName) and filesystem path
// segments alike (internal/queue's per-check log file names). It always
// strips '/' (and every other path separator), so the result never
// contains a directory traversal — but "." and ".." pass through
// unchanged, since both characters are individually allowed; a caller
// building a path component from an otherwise-untrusted string (as
// internal/queue does) should append a fixed suffix (e.g. ".log") so the
// final component can never equal "." or ".." outright.
func SanitizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
