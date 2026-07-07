package core

// Command kinds. Channels produce these; the queue applies them —
// Invariant 8 holds because the core defines the vocabulary and the
// application logic, while a channel only ever constructs a Command value.
const (
	// CommandRetry clears the park for (Command.Target, Command.Ref) at its
	// current SHA, if it is currently parked, so the next reconcile pass
	// re-tests it.
	CommandRetry = "retry"

	// CommandCancel is manual operator cancellation for
	// (Command.Target, Command.Ref): stop whatever is currently happening to
	// this candidate and park it at its current SHA, exactly like a red
	// verdict (Detail "cancelled by operator") — so it stays out of the
	// queue until a retry or a re-push, the same as any other park.
	//
	//   - A member of an in-flight run: that run is cancelled (the same
	//     invalidation machinery a ref move uses); serial/speculate park
	//     the member directly, batch parks only the named member and
	//     re-queues its siblings (Skipped, unparked).
	//   - A ref that's only queued (not yet picked): parked directly at its
	//     current SHA — cancel-before-start, so it's never picked up at all.
	//   - Unknown, or already parked at its current SHA: a no-op (idempotent).
	CommandCancel = "cancel"
)
