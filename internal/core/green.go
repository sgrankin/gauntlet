package core

// NodeGreen reports whether one graph node's result counts as green — the
// single rule every run-graph scheduler readiness decision, blocked-row
// synthesis, and culprit scan is written against.
//
// Green is Passed OR Skipped, with no Err. Skipped counts green because it
// is the node's OWN successful verdict that this revision needs nothing
// from it (the affected-only protocol): it satisfies `after` edges exactly
// like a pass, and a graph of passes and skips is a green graph. Err never
// counts green whatever the Status beside it says — a daemon-caused failure
// produced no verdict at all (CheckResult's Status-vs-Err contract), and
// CheckFailed being the zero value means an unassigned result must read as
// not-green, never as a silent pass.
//
// It lives in core because two schedulers now need the identical rule:
// internal/queue's advanceChecks/materializeChecks, which restated it
// inline three times, and internal/deploy's node-graph scheduler. The
// spike that chose "a second minimal scheduler, not an extraction"
// (docs/design/deployment.md, Phase D2) named this predicate as the one
// piece that IS genuinely shared — sharing the rule costs nothing and
// keeps the two tenants' notion of green from drifting apart, which is the
// only way their divergence could become a correctness bug rather than a
// design choice.
func NodeGreen(res CheckResult) bool {
	return res.Err == nil && (res.Status == CheckPassed || res.Status == CheckSkipped)
}
