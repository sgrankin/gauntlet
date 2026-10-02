// The emit-site contract table: one entry per EventKind, declaring what
// that kind carries. It is checked for completeness against numEventKinds,
// so a kind added without an entry here fails — the mechanism behind
// DESIGN.md's "when events next grow, extend those contract tests first".
package core

import (
	"strings"
	"testing"
	"time"
)

// kindContract is what one EventKind must carry. The zero value describes
// the common case — a progress report with no record and no result — so an
// entry only ever spells out what is special about its kind.
type kindContract struct {
	name string

	// terminal: carries its subsystem's finished record (Record for a run,
	// Deploy for a deploy graph run).
	terminal bool

	// check: carries the just-finished CheckResult in Event.Check.
	check bool

	// deploy: lane-addressed — carries DeployEnv, RunID and DeploySHA, and
	// never a Candidate or a RunRecord.
	deploy bool
}

// contract is indexed by EventKind. Adding a kind to core without adding it
// here is what TestEventContract_CoversEveryKind exists to catch.
var contract = [numEventKinds]kindContract{
	EventQueued:         {name: "queued"},
	EventTrialClean:     {name: "trial_clean"},
	EventTrialConflict:  {name: "trial_conflict", terminal: true},
	EventCheckStarted:   {name: "check_started"},
	EventCheckFinished:  {name: "check_finished", check: true},
	EventLanded:         {name: "landed", terminal: true},
	EventRejected:       {name: "rejected", terminal: true},
	EventSkipped:        {name: "skipped", terminal: true},
	EventError:          {name: "error", terminal: true},
	EventIgnoredRef:     {name: "ignored_ref"},
	EventHookFinished:   {name: "hook_finished", check: true},
	EventHookStarted:    {name: "hook_started"},
	EventHookSkipped:    {name: "hook_skipped"},
	EventTrialMerged:    {name: "trial_merged"},
	EventVerified:       {name: "verified"},
	EventRetryRequested: {name: "retry_requested"},

	EventDeployStarted:      {name: "deploy_started", deploy: true},
	EventDeployNodeFinished: {name: "deploy_node_finished", deploy: true, check: true},
	EventDeployFinished:     {name: "deploy_finished", deploy: true, terminal: true},
}

// TestEventContract_CoversEveryKind is the guard that makes "extend the
// contract tests first" mechanical rather than aspirational: a new
// EventKind shifts numEventKinds, and an unfilled slot in the table has an
// empty name.
func TestEventContract_CoversEveryKind(t *testing.T) {
	for k := range numEventKinds {
		if contract[k].name == "" {
			t.Errorf("EventKind(%d) has no emit-site contract entry: declare what it carries before emitting it", int(k))
		}
	}
}

// TestEventContract_TerminalAndDeployClassification pins the two
// classifications ValidateEvent and every consumer switch depend on against
// the table above, so a kind whose entry says "terminal" but which
// TerminalKind doesn't recognize (the copy-paste that ships an event
// nothing persists) fails here.
func TestEventContract_TerminalAndDeployClassification(t *testing.T) {
	for k := range numEventKinds {
		c := contract[k]
		if got := TerminalKind(k); got != c.terminal {
			t.Errorf("TerminalKind(%s) = %v, want %v", c.name, got, c.terminal)
		}
		if got := DeployKind(k); got != c.deploy {
			t.Errorf("DeployKind(%s) = %v, want %v", c.name, got, c.deploy)
		}
	}
}

// wellShaped builds the minimal event that satisfies k's contract — the
// positive case each rule below is then violated one field at a time from.
func wellShaped(k EventKind) Event {
	c := contract[k]
	ev := Event{Kind: k, At: time.Unix(0, 0), RunID: "run-1"}
	if c.deploy {
		ev.DeployEnv = "prod"
		ev.DeploySHA = "desired-sha"
		ev.DeployedSHA = "observed-sha"
	} else {
		ev.Target = "main"
		ev.Candidate = Candidate{Ref: "refs/heads/for/main/alice/topic", Target: "main", SHA: "cand-sha"}
	}
	if c.check {
		ev.CheckName = "unit"
		ev.Check = &CheckResult{Name: "unit", Status: CheckPassed}
	}
	if c.terminal {
		if c.deploy {
			ev.Deploy = &DeployRecord{Env: ev.DeployEnv, RunID: ev.RunID, DeploySHA: ev.DeploySHA, DeployedSHA: ev.DeployedSHA, Outcome: OutcomeLanded}
		} else {
			ev.Record = &RunRecord{RunID: ev.RunID, Target: ev.Target, Candidate: ev.Candidate}
		}
	}
	return ev
}

func TestValidateEvent_WellShapedKindsPass(t *testing.T) {
	for k := range numEventKinds {
		if err := ValidateEvent(wellShaped(k)); err != nil {
			t.Errorf("ValidateEvent(%s) = %v, want nil", contract[k].name, err)
		}
	}
}

// TestValidateEvent_TerminalWithoutRecord covers the oldest rule in the
// contract, for both subsystems: a terminal event with no record is the
// shape history's writer silently drops.
func TestValidateEvent_TerminalWithoutRecord(t *testing.T) {
	for k := range numEventKinds {
		if !contract[k].terminal {
			continue
		}
		ev := wellShaped(k)
		ev.Record, ev.Deploy = nil, nil
		if err := ValidateEvent(ev); err == nil {
			t.Errorf("ValidateEvent(%s) with no record = nil, want an error", contract[k].name)
		}
	}
}

func TestValidateEvent_NonTerminalWithRecord(t *testing.T) {
	for k := range numEventKinds {
		if contract[k].terminal {
			continue
		}
		ev := wellShaped(k)
		ev.Record = &RunRecord{RunID: ev.RunID}
		if err := ValidateEvent(ev); err == nil {
			t.Errorf("ValidateEvent(%s) with a Record = nil, want an error (slack/history read one as a finished run)", contract[k].name)
		}
	}
}

// TestValidateEvent_FinishedKindsCarryTheirResult is the second historical
// gap (EventCheckFinished shipped without its CheckResult) extended to the
// deploy node event, which is the same shape for the same reason: a channel
// renders per-node verdicts mid-graph or it waits for the terminal record.
func TestValidateEvent_FinishedKindsCarryTheirResult(t *testing.T) {
	for k := range numEventKinds {
		if !contract[k].check {
			continue
		}
		ev := wellShaped(k)
		ev.Check = nil
		if err := ValidateEvent(ev); err == nil {
			t.Errorf("ValidateEvent(%s) with no Check = nil, want an error", contract[k].name)
		}
		ev = wellShaped(k)
		ev.CheckName = ""
		if err := ValidateEvent(ev); err == nil {
			t.Errorf("ValidateEvent(%s) with no CheckName = nil, want an error", contract[k].name)
		}
	}
}

func TestValidateEvent_DeployCoordinatesRequired(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Event)
		want string
	}{
		{"no environment", func(ev *Event) { ev.DeployEnv = "" }, "DeployEnv"},
		{"no run id", func(ev *Event) { ev.RunID = "" }, "RunID"},
		{"no revision", func(ev *Event) { ev.DeploySHA = "" }, "DeploySHA"},
		{"a candidate", func(ev *Event) { ev.Candidate = Candidate{Ref: "refs/heads/for/main/alice/topic"} }, "Candidate"},
		// The non-terminal deploy kinds trip the general
		// "non-terminal carries no Record" rule first; EventDeployFinished
		// trips the deploy-specific one. Both say "Record".
		{"a run record", func(ev *Event) { ev.Record = &RunRecord{RunID: "run-1"} }, "Record"},
	}
	for _, kind := range []EventKind{EventDeployStarted, EventDeployNodeFinished, EventDeployFinished} {
		for _, tc := range tests {
			ev := wellShaped(kind)
			tc.mut(&ev)
			err := ValidateEvent(ev)
			if err == nil {
				t.Errorf("ValidateEvent(%s) with %s = nil, want an error", contract[kind].name, tc.name)
				continue
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ValidateEvent(%s) with %s = %v, want it to name %s", contract[kind].name, tc.name, err, tc.want)
			}
		}
	}
}

// TestValidateEvent_DeployedSHAMayBeEmpty: an environment's first-ever
// deploy has no previous observed SHA, so empty is a REAL value there — the
// one deploy coordinate that must never be required.
func TestValidateEvent_DeployedSHAMayBeEmpty(t *testing.T) {
	for _, kind := range []EventKind{EventDeployStarted, EventDeployNodeFinished, EventDeployFinished} {
		ev := wellShaped(kind)
		ev.DeployedSHA = ""
		if ev.Deploy != nil {
			ev.Deploy.DeployedSHA = ""
		}
		if err := ValidateEvent(ev); err != nil {
			t.Errorf("ValidateEvent(%s) with an empty DeployedSHA = %v, want nil (first-ever deploy)", contract[kind].name, err)
		}
	}
}

// TestValidateEvent_DeployFinishedRecordMustAgree: the event's coordinates
// and its record's are two views of one fact. A consumer may read either,
// so they may not disagree — the failure mode is a dashboard row and a
// history row describing different runs.
func TestValidateEvent_DeployFinishedRecordMustAgree(t *testing.T) {
	mutations := map[string]func(*DeployRecord){
		"environment": func(r *DeployRecord) { r.Env = "dev" },
		"run id":      func(r *DeployRecord) { r.RunID = "run-2" },
		"revision":    func(r *DeployRecord) { r.DeploySHA = "other-sha" },
		"previous":    func(r *DeployRecord) { r.DeployedSHA = "other-sha" },
	}
	for what, mut := range mutations {
		ev := wellShaped(EventDeployFinished)
		mut(ev.Deploy)
		if err := ValidateEvent(ev); err == nil {
			t.Errorf("ValidateEvent(deploy_finished) whose record disagrees about the %s = nil, want an error", what)
		}
	}
}

// TestValidateEvent_NonDeployKindsCarryNoDeployFields keeps the deploy
// fields as kind-scoped as CheckName and MergeSHA already are: a candidate
// event that sets one is a copy-paste, not a feature.
func TestValidateEvent_NonDeployKindsCarryNoDeployFields(t *testing.T) {
	for k := range numEventKinds {
		if contract[k].deploy {
			continue
		}
		for what, mut := range map[string]func(*Event){
			"DeployEnv":    func(ev *Event) { ev.DeployEnv = "prod" },
			"DeploySHA":    func(ev *Event) { ev.DeploySHA = "sha" },
			"DeployedSHA":  func(ev *Event) { ev.DeployedSHA = "sha" },
			"DeployRecord": func(ev *Event) { ev.Deploy = &DeployRecord{Env: "prod"} },
		} {
			ev := wellShaped(k)
			mut(&ev)
			if err := ValidateEvent(ev); err == nil {
				t.Errorf("ValidateEvent(%s) carrying %s = nil, want an error", contract[k].name, what)
			}
		}
	}
}

// TestValidateEvent_UnknownKindIsShapeChecked: an EventKind from a future
// build is validated as an ordinary progress report rather than panicking
// or waving anything through — the same "unknown kinds are additive, never
// fatal" stance channels take (internal/channel/log.go).
func TestValidateEvent_UnknownKindIsShapeChecked(t *testing.T) {
	if err := ValidateEvent(Event{Kind: EventKind(999), Target: "main"}); err != nil {
		t.Errorf("ValidateEvent(unknown kind) = %v, want nil", err)
	}
	if err := ValidateEvent(Event{Kind: EventKind(999), Record: &RunRecord{}}); err == nil {
		t.Error("ValidateEvent(unknown kind with a Record) = nil, want an error")
	}
}
