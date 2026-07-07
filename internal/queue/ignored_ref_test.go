package queue

import (
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

// TestReconcile_IgnoredRefEmittedOnce covers checkIgnoredRefs (see
// docs/design/core.md, "Candidate ref grammar"): a well-formed candidate
// ref naming a target that isn't configured must produce exactly one
// core.EventIgnoredRef per (ref, SHA) — not one every tick.
func TestReconcile_IgnoredRefEmittedOnce(t *testing.T) {
	h := newHarness(t) // configures only "main"
	h.git.seed("main", nil)
	ref := candidateRef("staging", "alice", "widget") // "staging" is not configured
	sha := h.git.pushCandidate(ref, "", checkSpecFile("test"))

	h.reconcile()
	h.reconcile()
	h.reconcile()

	var matches []core.Event
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventIgnoredRef {
			matches = append(matches, e)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("EventIgnoredRef count = %d across 3 ticks, want exactly 1 (deduped)", len(matches))
	}
	if matches[0].Candidate.Ref != ref || matches[0].Candidate.SHA != sha {
		t.Fatalf("EventIgnoredRef candidate = %+v, want ref=%q sha=%q", matches[0].Candidate, ref, sha)
	}
	if matches[0].Detail == "" {
		t.Error("EventIgnoredRef.Detail is empty; should name the unconfigured target")
	}
	// Never treated as a real candidate: no CAS activity at all.
	if len(h.git.casLog) != 0 {
		t.Fatalf("casLog = %+v, want none (an ignored ref must never be trial-merged or landed)", h.git.casLog)
	}
}

// TestReconcile_IgnoredRefReemitsOnSHAChange: a re-push (new SHA) of an
// ignored ref is a new (ref, SHA) pair and gets its own EventIgnoredRef.
func TestReconcile_IgnoredRefReemitsOnSHAChange(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("staging", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.reconcile()

	h.git.pushCandidate(ref, "", checkSpecFile("test", "extra")) // new SHA
	h.reconcile()

	var count int
	for _, e := range h.ch.Events() {
		if e.Kind == core.EventIgnoredRef {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("EventIgnoredRef count = %d, want 2 (one per distinct SHA)", count)
	}
}

// TestReconcile_IgnoredRefPrunedWhenRefVanishes: once an ignored ref is
// deleted, its dedupe entry is pruned so d.ignoredRefs can't grow
// unboundedly over a long-running daemon's lifetime.
func TestReconcile_IgnoredRefPrunedWhenRefVanishes(t *testing.T) {
	h := newHarness(t)
	h.git.seed("main", nil)
	ref := candidateRef("staging", "alice", "widget")
	h.git.pushCandidate(ref, "", checkSpecFile("test"))
	h.reconcile()

	if len(h.d.ignoredRefs) != 1 {
		t.Fatalf("ignoredRefs = %v, want exactly one tracked ref", h.d.ignoredRefs)
	}

	h.git.deleteCandidate(ref)
	h.reconcile()

	if len(h.d.ignoredRefs) != 0 {
		t.Fatalf("ignoredRefs = %v, want empty after the ref vanished", h.d.ignoredRefs)
	}
}
