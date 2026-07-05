package core

// Command kinds. Channels produce these; the queue applies them —
// Invariant 8 holds because the core defines the vocabulary and the
// application logic, while a channel only ever constructs a Command value
// (docs/plans/phase23.md §2.2).
const (
	// CommandRetry clears the park for (Command.Target, Command.Ref) at its
	// current SHA, if it is currently parked, so the next reconcile pass
	// re-tests it. The phase-2 mechanism docs/plans/phase1.md §9.1 reserved
	// for this ("a channel `retry` command (phase 2) will clear parks
	// explicitly").
	CommandRetry = "retry"
)
