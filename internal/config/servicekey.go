package config

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
)

// ServiceKey returns the full cache key for svc under repo remote: the hex
// SHA-256 over a canonical, target-independent encoding of (remote, svc)
// (docs/plans/services.md §2; docs/plans/services-impl.md §2.5). The key
// hashes the PARSED, DEFAULTED struct — never raw KDL bytes (review m1) —
// so reordering env lines or reflowing whitespace never recycles an
// instance, while a future change to a default (applyServiceDefaults) DOES
// recycle instances whose specs relied on the old value, because
// ParseChecks applies defaults before this or any caller ever sees the
// struct.
//
// Encoding, fixed and total:
//
//	remote, Name, Image: length-prefixed
//	Port: 8-byte big-endian
//	Env: element count, then sorted by Name, each (Name,Value) length-prefixed
//	ReadyCommand: element count + each element length-prefixed
//	ReadyTimeout, IdleTTL: int64 nanoseconds, 8-byte big-endian
//
// Returns the full 64-hex digest. Truncation to a 12-hex alias (key[:12])
// is the services pool's job, for container/network names meant for humans
// — records and boot adoption must match on the FULL key (review m2/m6),
// never the truncated alias.
func ServiceKey(remote string, svc Service) string {
	h := sha256.New()
	writeString(h, remote)
	writeString(h, svc.Name)
	writeString(h, svc.Image)
	writeUint64(h, uint64(svc.Port))

	// Sorted by name so two declarations differing only in env line order
	// hash identically (review m1) — a copy, since svc is passed by value
	// but Env is a slice header sharing the caller's backing array; sorting
	// in place would be an observable side effect on the caller's Service.
	// Count-prefixed like ReadyCommand below: without it, the env region's
	// boundary is implied by field order alone, which is weaker than an
	// explicit 8-byte-aligned length.
	env := append([]EnvVar(nil), svc.Env...)
	sort.Slice(env, func(i, j int) bool { return env[i].Name < env[j].Name })
	writeUint64(h, uint64(len(env)))
	for _, e := range env {
		writeString(h, e.Name)
		writeString(h, e.Value)
	}

	writeUint64(h, uint64(len(svc.ReadyCommand)))
	for _, arg := range svc.ReadyCommand {
		writeString(h, arg)
	}

	writeUint64(h, uint64(svc.ReadyTimeout))
	writeUint64(h, uint64(svc.IdleTTL))

	return hex.EncodeToString(h.Sum(nil))
}

// writeString length-prefixes s (8-byte big-endian length, then the raw
// bytes) before writing it to h, so concatenating fields into one hash
// stream can never let two different field sequences collide on the same
// bytes — e.g. ("ab","c") and ("a","bc") would hash identically without a
// prefix.
func writeString(h hash.Hash, s string) {
	writeUint64(h, uint64(len(s)))
	h.Write([]byte(s))
}

func writeUint64(h hash.Hash, v uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	h.Write(buf[:])
}
