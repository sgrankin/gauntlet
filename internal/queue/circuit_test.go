package queue

import (
	"errors"
	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"path/filepath"
	"testing"
	"time"
)

func TestInfrastructureBreakerPersistsAndNeverClearsPause(t *testing.T) {
	h := newHarness(t)
	h.d.cfg.CircuitBreaker = &config.CircuitBreaker{Threshold: 3, Window: time.Minute, Backoff: time.Second, MaxBackoff: 8 * time.Second}
	h.d.cfg.ControlPath = filepath.Join(t.TempDir(), "controls.json")
	now := time.Now()
	h.d.now = func() time.Time { return now }
	failure := func(ref string) {
		h.d.observeInfrastructure(core.Event{Target: "main", Candidate: core.Candidate{Ref: ref, SHA: "revision"}, Check: &core.CheckResult{Err: errors.New("builder unavailable")}})
	}
	failure("a")
	failure("a")
	failure("b")
	if h.d.circuitBlocked("main") {
		t.Fatal("same revision counted twice")
	}
	failure("c")
	if !h.d.circuitBlocked("main") {
		t.Fatal("shared infrastructure failure did not open breaker")
	}
	state, err := loadControls(h.d.cfg.ControlPath)
	if err != nil || state.Circuits["main"].Until.IsZero() {
		t.Fatalf("breaker not durable: %+v %v", state, err)
	}
	now = now.Add(time.Second)
	if h.d.circuitBlocked("main") {
		t.Fatal("probe did not become eligible")
	}
	failure("c")
	if h.d.controls.Circuits["main"].Backoff != 2*time.Second {
		t.Fatal("failed probe did not back off")
	}
	h.d.controls.Pauses["main"] = Pause{Actor: "operator", Reason: "incident"}
	h.d.observeInfrastructure(core.Event{Target: "main", RunID: "old-work", Candidate: core.Candidate{Ref: "c"}, Check: &core.CheckResult{Status: core.CheckPassed}})
	if !h.d.circuitBlocked("main") {
		t.Fatal("old work incorrectly recovered breaker")
	}
	circuit := h.d.controls.Circuits["main"]
	circuit.ProbeRunID = "probe"
	h.d.controls.Circuits["main"] = circuit
	h.d.observeInfrastructure(core.Event{Target: "main", RunID: "probe", Candidate: core.Candidate{Ref: "c"}, Record: &core.RunRecord{Outcome: core.OutcomeLanded, Checks: []core.CheckResult{{Status: core.CheckPassed}}}})
	if _, ok := h.d.controls.Pauses["main"]; !ok {
		t.Fatal("probe cleared manual pause")
	}
	if h.d.circuitBlocked("main") {
		t.Fatal("healthy check did not recover breaker")
	}
}
