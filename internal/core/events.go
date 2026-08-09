package core

import "fmt"

// The emit-site contract, in executable form.
//
// Event shapes are this design's standing soft underbelly (DESIGN.md, "Event
// shapes are the soft underbelly"): two review cycles found the same family
// of bug — an emitter that shipped an event missing the one field its
// consumers join on. The rules below were prose and a per-package test
// helper; ValidateEvent makes them one shared, importable statement, so a
// new emitter in a new package (internal/deploy is the first) can be held to
// them by its own tests rather than by whichever assertions its author
// happened to think of.
//
// It validates SHAPE only — which fields a kind must and must not carry —
// never content: nothing here knows whether a SHA exists or a name is
// spelled right.

// TerminalKind reports whether k is a terminal event kind: one that
// concludes a run (or a deploy graph run) and therefore carries that run's
// finished record. Every other kind is a progress report.
func TerminalKind(k EventKind) bool {
	switch k {
	case EventLanded, EventRejected, EventTrialConflict, EventSkipped, EventError:
		return true
	case EventDeployFinished:
		return true
	default:
		return false
	}
}

// DeployKind reports whether k is one of the deploy-lane kinds — the events
// addressed by environment rather than by candidate ref.
func DeployKind(k EventKind) bool {
	switch k {
	case EventDeployStarted, EventDeployNodeFinished, EventDeployFinished:
		return true
	default:
		return false
	}
}

// ValidateEvent reports whether ev satisfies the emit-site contract, naming
// the first violation it finds. A nil return means the event is well-shaped
// for its kind; it says nothing about whether the kind was the right one to
// emit.
//
// Emitters do NOT call this — the daemon must never fail a run because a
// notification was malformed, and a channel that can't render an event
// already ignores it (core.Channel's unknown-kind contract). It exists for
// TESTS: any package that emits events asserts every event it produced
// passes, which is the cheap standing guard the two historical gaps
// (EventTrialClean without its RunID, EventCheckFinished without its
// CheckResult) would each have tripped.
func ValidateEvent(ev Event) error {
	switch {
	case TerminalKind(ev.Kind) && !DeployKind(ev.Kind) && ev.Record == nil:
		// The rule that survives even where no run object ever existed: a
		// crash-recovered landing synthesizes a complete record rather than
		// emitting EventLanded with a nil one, because history's SQLite
		// writer joins on it.
		return fmt.Errorf("event kind %d is terminal and must carry a Record", ev.Kind)
	case !TerminalKind(ev.Kind) && ev.Record != nil:
		// The trial-merge/verified events carry their merge identity in
		// MergeSHA precisely so this stays true: a non-terminal event with
		// a Record reads as a finished run to internal/slack and
		// internal/history.
		return fmt.Errorf("event kind %d is not terminal and must not carry a Record", ev.Kind)
	}

	switch ev.Kind {
	case EventCheckFinished, EventHookFinished, EventDeployNodeFinished:
		if ev.Check == nil {
			return fmt.Errorf("event kind %d must carry the finished Check", ev.Kind)
		}
		if ev.CheckName == "" {
			return fmt.Errorf("event kind %d must name what finished", ev.Kind)
		}
	default:
		if ev.Check != nil {
			return fmt.Errorf("event kind %d must not carry a Check", ev.Kind)
		}
	}

	if !DeployKind(ev.Kind) {
		switch {
		case ev.Deploy != nil:
			return fmt.Errorf("event kind %d is not a deploy event and must not carry a DeployRecord", ev.Kind)
		case ev.DeployEnv != "" || ev.DeploySHA != "" || ev.DeployedSHA != "":
			return fmt.Errorf("event kind %d is not a deploy event and must not carry deploy coordinates", ev.Kind)
		}
		return nil
	}

	// Deploy events are lane-addressed: environment, run ID, and the
	// revision the run is about, on EVERY one of them — a channel must be
	// able to render "prod is deploying <sha>" from a started event alone.
	// DeployedSHA is deliberately NOT required: empty is a real value there
	// (an environment's first-ever deploy has no previous observed SHA).
	switch {
	case ev.Record != nil:
		// EventDeployFinished is terminal but its record is a
		// *DeployRecord: a RunRecord here would make slack/history render a
		// deploy as a landed candidate run.
		return fmt.Errorf("deploy event kind %d must not carry a RunRecord", ev.Kind)
	case ev.DeployEnv == "":
		return fmt.Errorf("deploy event kind %d must carry DeployEnv", ev.Kind)
	case ev.RunID == "":
		return fmt.Errorf("deploy event kind %d must carry the deploy RunID", ev.Kind)
	case ev.DeploySHA == "":
		return fmt.Errorf("deploy event kind %d must carry DeploySHA", ev.Kind)
	case ev.Candidate != (Candidate{}):
		return fmt.Errorf("deploy event kind %d must not carry a Candidate: a deploy lane has no candidate ref", ev.Kind)
	}
	if ev.Kind == EventDeployFinished {
		if ev.Deploy == nil {
			return fmt.Errorf("event kind %d is terminal and must carry a DeployRecord", ev.Kind)
		}
		// The record and the event must agree about which run this is —
		// they are two views of one fact, and a consumer is free to read
		// either.
		if ev.Deploy.Env != ev.DeployEnv || ev.Deploy.RunID != ev.RunID || ev.Deploy.DeploySHA != ev.DeploySHA || ev.Deploy.DeployedSHA != ev.DeployedSHA {
			return fmt.Errorf("event kind %d disagrees with its DeployRecord about the run's identity", ev.Kind)
		}
	} else if ev.Deploy != nil {
		return fmt.Errorf("event kind %d is not terminal and must not carry a DeployRecord", ev.Kind)
	}
	return nil
}
