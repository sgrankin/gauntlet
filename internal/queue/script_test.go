// Growth-layer scenario harness (DESIGN.md's testing ledger row): a tiny
// rsc/script-style command DSL over txtar, built on
// github.com/rogpeppe/go-internal/testscript (spike-decided 2026-07-05 over
// the orphaned rsc.io/script). Scripts under testdata/script/*.txtar read
// like documentation of the queue's state machine; they DUPLICATE coverage
// that already exists as Go tests elsewhere in this package (daemon_test.go,
// moves_test.go, park_test.go, integration_*_test.go and friends) — those
// stay as the authoritative invariant record, since deleting them would trade
// a precise failure (a specific assertion, a specific line) for a fuzzier one
// (a script line number). The scripts are the readable layer on top, not a
// replacement.
//
// One Cmds set, two Setups, per the ledger's port pattern: TestScriptFake
// drives the in-memory fakeGitRepo harness (testHarness, daemon_test.go);
// TestScriptReal drives the real-git harness (integrationHarness,
// integration_test.go). Both are wrapped behind the small scriptHarness
// interface below, so command implementations don't care which is under
// them. For the three scenarios ported here, the fake harness satisfies the
// interface exactly as cheaply as the real one — nothing here needs genuine
// remote refs or real git's content-addressing quirks — so both suites run
// every script in testdata/script from the same Cmds map. A future scenario
// that's fundamentally about real-git plumbing (a real merge conflict, an
// actual second git process racing CAS) should run only under TestScriptReal
// rather than contorting the fake to fake that too.
//
// Command vocabulary (kept intentionally small and boring):
//
//	push-candidate <target> <user> <topic> <dir>
//	    Push a brand-new candidate ref for (target, user, topic), with file
//	    contents taken from <dir> — a directory already extracted from the
//	    txtar archive (a "-- dir/path --" section). user may be '' (an empty
//	    quoted argument) for the solo, no-user ref form.
//
//	repush <target> <user> <topic> <dir>
//	    Force-update an existing candidate ref with new content from <dir> —
//	    an author re-push (new SHA, same ref).
//
//	delete-candidate <target> <user> <topic>
//	    Delete the candidate ref, as if the author cancelled.
//
//	direct-push <target> <dir>
//	    Commit <dir>'s files directly onto target's branch, bypassing CAS —
//	    a human or a second daemon racing the queue.
//
//	tick
//	    Run one Daemon.ReconcileOnce pass.
//
//	await-started [@<selector>] <name>
//	    Block until check <name> has registered as started (the gated
//	    executor's Started signal) on the run <selector> names — or, with
//	    no selector, the pipeline's head run (lane.runs[0]), exactly the
//	    pre-P5-F "currently in-flight run" every existing script assumes.
//	    See "Run selectors" below.
//
//	release-check [@<selector>] <name> <passed|failed|skipped>
//	    Delivers <name>'s verdict on the run <selector> names (head run if
//	    omitted), then (like the Go harnesses' own release/releaseGated
//	    helpers) spins ReconcileOnce until a new event lands.
//
// Run selectors (docs/plans/phase5.md §5.1): a pipeline can hold more than
// one run at once (speculate's window), so await-started/release-check take
// an optional leading "@<selector>" argument naming which one:
//
//	@topic:<t>   the run whose head member's Topic == t
//	@#<i>        the run at 0-based lane position i (lane.runs[i])
//	(omitted)    the head run (lane.runs[0]) — every script that never
//	             passes a selector keeps working verbatim, unchanged
//
// Resolved via the Daemon's published Snapshot (resolveRunSelector, this
// file): both suites already expose it identically (scriptHarness's fake and
// real implementations both read the same *Daemon.Snapshot()), so the
// selector logic itself is written once, not per harness.
//
//	[!] assert-event <kind>
//	    Assert that an event of <kind> was (or, negated, was never) among
//	    every event captured so far. <kind> is one of: queued, trial-clean,
//	    trial-conflict, check-started, check-finished, landed, rejected,
//	    skipped, error, ignored-ref.
//
//	assert-target-is-merge <target>
//	    Assert target's current tip equals the last captured RunRecord's
//	    MergeSHA, and that its second parent is that record's candidate SHA
//	    verbatim (Invariant 1 / Invariant 6).
//
//	[!] assert-slot-gone <target> <user> <topic>
//	    Assert the candidate ref no longer exists (or, negated, that it
//	    still does).
//
//	assert-slot-parked <target> <user> <topic>
//	    Assert the candidate ref still exists and the last run recorded
//	    against it parked (Rejected, Conflict, or Error) rather than landed.
//
//	assert-slot-parked-none <target> <user> <topic>
//	    Assert the candidate ref still exists and is NOT parked (docs/plans/
//	    phase5.md §5.1): the Skipped-not-parked shape a pipeline bubble, a
//	    mid-window member move, or a head target move leaves behind — the
//	    ref is free to re-queue on the very next refill, unlike
//	    assert-slot-parked's sticky (Rejected/Conflict/Error) case.
//
//	set-mode <target> <mode> <n>
//	    Test-only escape hatch (docs/plans/phase5.md P5-E/P5-F): mutates
//	    target's Mode/MaxBatch/Window on the already-constructed Daemon.
//	    Setup builds one fixed target config shared by every script in this
//	    directory (Mode "", the serial default); batch/speculate scenarios
//	    call this as their first command to switch "main" into the relevant
//	    mode without a bespoke Setup per file. n sets MaxBatch when mode is
//	    "batch", Window when mode is "speculate"; ignored (but must still
//	    parse) otherwise.
//
//	assert-pipeline-depth <target> <n>
//	    Assert len(lane.runs) for target, via the Daemon's published
//	    Snapshot (0 for an idle target).
//
//	assert-landed-order <target> <topic>...
//	    Assert EventLanded events for target name candidates by Topic, in
//	    the given order (FIFO landing order — batch's single-run multi-
//	    member land, or a speculation window's sequence).
//
//	assert-target-chain <target> <topic>...
//	    Assert target's tip is the head of a --no-ff chain with exactly one
//	    merge commit per topic, oldest first (topics[0] is the innermost
//	    link, topics[last] is the tip itself), each merge's parent[1] equal
//	    to that topic's landed candidate SHA verbatim (Invariant 1/6 for
//	    the whole chain — assert-target-is-merge's chain generalization).
package queue

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
)

// harnessKey is the Env.Values key Setup stores the scriptHarness under, and
// commands retrieve it from via ts.Value.
type harnessKey struct{}

// scriptHarness is the minimal surface the command DSL needs, satisfied by
// both fakeScriptHarness (wrapping testHarness) and realScriptHarness
// (wrapping integrationHarness). It exists so command implementations are
// written once against an interface, not against either concrete harness.
type scriptHarness interface {
	// pushCandidate creates a fresh candidate ref for (target, user, topic)
	// with files, returning the ref name and its SHA.
	pushCandidate(target, user, topic string, files map[string]string) (ref, sha string)

	// repush force-updates the existing candidate ref for (target, user,
	// topic) with files, returning its new SHA.
	repush(target, user, topic string, files map[string]string) (sha string)

	// deleteCandidate removes the candidate ref for (target, user, topic).
	deleteCandidate(target, user, topic string)

	// directPush commits files directly onto target's branch, bypassing CAS.
	directPush(target string, files map[string]string)

	// tick runs one ReconcileOnce pass.
	tick()

	// awaitStarted blocks until name has registered as started on the run
	// selector names (resolveRunSelector; "" is the head run).
	awaitStarted(selector, name string)

	// releaseCheck delivers result for name on the run selector names
	// ("" is the head run) and spins until a new event is captured.
	releaseCheck(selector, name string, status core.CheckStatus)

	// events and records return the full RecordingChannel history so far.
	events() []core.Event
	records() []*core.RunRecord

	// targetRef returns target's current tip OID, or "" if it has none.
	targetRef(target string) string

	// slotRef returns the candidate ref's current OID for (target, user,
	// topic), or "" if the slot doesn't exist.
	slotRef(target, user, topic string) string

	// commitParents returns oid's parent OIDs in order, or nil if oid is
	// unknown.
	commitParents(oid string) []string

	// setMode mutates target's Mode/MaxBatch/Window on the already-
	// constructed Daemon (set-mode's backing implementation; see that
	// command's doc).
	setMode(target, mode string, n int)

	// pipelineDepth returns len(lane.runs) for target via the Daemon's
	// published Snapshot (0 if idle or unknown) — assert-pipeline-depth's
	// data source.
	pipelineDepth(target string) int

	// slotParked reports whether the candidate ref for (target, user,
	// topic) is currently parked (a sticky Daemon.done entry) at its
	// current SHA — assert-slot-parked-none's data source.
	slotParked(target, user, topic string) bool
}

// --- fakeScriptHarness: adapts testHarness (daemon_test.go) ---

type fakeScriptHarness struct{ h *testHarness }

var _ scriptHarness = fakeScriptHarness{}

func (f fakeScriptHarness) pushCandidate(target, user, topic string, files map[string]string) (string, string) {
	ref := candidateRef(target, user, topic)
	return ref, f.h.git.pushCandidate(ref, "", files)
}

func (f fakeScriptHarness) repush(target, user, topic string, files map[string]string) string {
	ref := candidateRef(target, user, topic)
	return f.h.git.pushCandidate(ref, "", files)
}

func (f fakeScriptHarness) deleteCandidate(target, user, topic string) {
	f.h.git.deleteCandidate(candidateRef(target, user, topic))
}

func (f fakeScriptHarness) directPush(target string, files map[string]string) {
	f.h.git.directPush(target, files)
}

func (f fakeScriptHarness) tick() { f.h.reconcile() }

func (f fakeScriptHarness) awaitStarted(selector, name string) {
	f.h.awaitStarted(f.resolveRunID(selector), name)
}

func (f fakeScriptHarness) releaseCheck(selector, name string, status core.CheckStatus) {
	f.h.release(f.resolveRunID(selector), name, core.CheckResult{Name: name, Status: status})
}

// resolveRunID resolves selector against the Daemon's published Snapshot
// (resolveRunSelector), failing the test with a clear message if it names no
// in-flight run.
func (f fakeScriptHarness) resolveRunID(selector string) string {
	f.h.t.Helper()
	runID := resolveRunSelector(f.h.d.Snapshot(), selector)
	if runID == "" {
		f.h.t.Fatalf("run selector %q did not resolve to any in-flight run", selector)
	}
	return runID
}

func (f fakeScriptHarness) events() []core.Event       { return f.h.ch.Events() }
func (f fakeScriptHarness) records() []*core.RunRecord { return f.h.ch.Records() }

func (f fakeScriptHarness) targetRef(target string) string {
	return f.h.git.ref("refs/heads/" + target)
}

func (f fakeScriptHarness) slotRef(target, user, topic string) string {
	return f.h.git.ref(candidateRef(target, user, topic))
}

func (f fakeScriptHarness) commitParents(oid string) []string {
	c, ok := f.h.git.commits[oid]
	if !ok {
		return nil
	}
	return c.parents
}

func (f fakeScriptHarness) setMode(target, mode string, n int) {
	for i := range f.h.d.cfg.Targets {
		if f.h.d.cfg.Targets[i].Name == target {
			f.h.d.cfg.Targets[i].Mode = mode
			f.h.d.cfg.Targets[i].MaxBatch = n
			f.h.d.cfg.Targets[i].Window = n
		}
	}
}

func (f fakeScriptHarness) pipelineDepth(target string) int {
	return snapshotPipelineDepth(f.h.d, target)
}

func (f fakeScriptHarness) slotParked(target, user, topic string) bool {
	ref := candidateRef(target, user, topic)
	sha := f.h.git.ref(ref)
	if sha == "" {
		return false
	}
	entry, ok := f.h.d.done[target][ref]
	return ok && entry.SHA == sha
}

// --- realScriptHarness: adapts integrationHarness (integration_test.go) ---

// realScriptHarness pairs an integrationHarness with the GatedExecutor it was
// built against — integrationHarness itself doesn't retain the executor
// (some integration tests use a real LocalExecutor instead), so scripts carry
// it alongside.
type realScriptHarness struct {
	h     *integrationHarness
	gated *executor.GatedExecutor
}

var _ scriptHarness = realScriptHarness{}

func (r realScriptHarness) pushCandidate(target, user, topic string, files map[string]string) (string, string) {
	ref := r.h.remote.PushCandidate(target, user, topic, files)
	return ref, r.h.remote.Ref(ref)
}

func (r realScriptHarness) repush(target, user, topic string, files map[string]string) string {
	ref := candidateRef(target, user, topic)
	return r.h.remote.MoveCandidate(ref, files)
}

func (r realScriptHarness) deleteCandidate(target, user, topic string) {
	r.h.remote.DeleteCandidate(candidateRef(target, user, topic))
}

func (r realScriptHarness) directPush(target string, files map[string]string) {
	r.h.remote.DirectPush(target, files)
}

func (r realScriptHarness) tick() { r.h.reconcile() }

func (r realScriptHarness) awaitStarted(selector, name string) {
	r.h.awaitStarted(r.gated, r.resolveRunID(selector), name)
}

func (r realScriptHarness) releaseCheck(selector, name string, status core.CheckStatus) {
	r.h.releaseGated(r.gated, r.resolveRunID(selector), name, core.CheckResult{Name: name, Status: status})
}

// resolveRunID resolves selector against the Daemon's published Snapshot
// (resolveRunSelector), failing the test with a clear message if it names no
// in-flight run.
func (r realScriptHarness) resolveRunID(selector string) string {
	r.h.t.Helper()
	runID := resolveRunSelector(r.h.d.Snapshot(), selector)
	if runID == "" {
		r.h.t.Fatalf("run selector %q did not resolve to any in-flight run", selector)
	}
	return runID
}

func (r realScriptHarness) events() []core.Event       { return r.h.ch.Events() }
func (r realScriptHarness) records() []*core.RunRecord { return r.h.ch.Records() }

func (r realScriptHarness) targetRef(target string) string {
	return r.h.remote.Ref("refs/heads/" + target)
}

func (r realScriptHarness) slotRef(target, user, topic string) string {
	return r.h.remote.Ref(candidateRef(target, user, topic))
}

func (r realScriptHarness) commitParents(oid string) []string {
	return r.h.remote.Parents(oid)
}

func (r realScriptHarness) setMode(target, mode string, n int) {
	for i := range r.h.d.cfg.Targets {
		if r.h.d.cfg.Targets[i].Name == target {
			r.h.d.cfg.Targets[i].Mode = mode
			r.h.d.cfg.Targets[i].MaxBatch = n
			r.h.d.cfg.Targets[i].Window = n
		}
	}
}

func (r realScriptHarness) pipelineDepth(target string) int {
	return snapshotPipelineDepth(r.h.d, target)
}

func (r realScriptHarness) slotParked(target, user, topic string) bool {
	ref := candidateRef(target, user, topic)
	sha := r.h.remote.Ref(ref)
	if sha == "" {
		return false
	}
	entry, ok := r.h.d.done[target][ref]
	return ok && entry.SHA == sha
}

// snapshotPipelineDepth reads len(lane.runs) for target out of d's most
// recently published Snapshot (shared by both scriptHarness
// implementations — Daemon.Snapshot is the same public API either way).
func snapshotPipelineDepth(d *Daemon, target string) int {
	snap := d.Snapshot()
	if snap == nil {
		return 0
	}
	for _, ts := range snap.Targets {
		if ts.Name == target {
			return len(ts.Pipeline)
		}
	}
	return 0
}

// resolveRunSelector resolves a DSL run selector (docs/plans/phase5.md
// §5.1) against snap, the Daemon's published Snapshot: "" (omitted) is the
// head run of the first target with any in-flight runs — every script that
// never passes a selector keeps addressing "the currently in-flight run"
// exactly as before P5-F. "topic:<t>" is the run whose head member's Topic
// == t; "#<i>" is the run at 0-based lane position i. Both are searched
// across every target's Pipeline: every script in this package drives
// exactly one target at a time, so there is no ambiguity in practice.
// Returns "" if snap is nil or the selector names nothing (an out-of-range
// index, an unknown topic, or no in-flight run at all) — callers turn that
// into a clear test failure themselves rather than silently addressing the
// wrong run.
func resolveRunSelector(snap *Snapshot, selector string) string {
	if snap == nil {
		return ""
	}
	switch {
	case selector == "":
		for _, ts := range snap.Targets {
			if len(ts.Pipeline) > 0 {
				return ts.Pipeline[0].RunID
			}
		}
		return ""
	case strings.HasPrefix(selector, "topic:"):
		topic := strings.TrimPrefix(selector, "topic:")
		for _, ts := range snap.Targets {
			for _, r := range ts.Pipeline {
				if r.Candidate.Topic == topic {
					return r.RunID
				}
			}
		}
		return ""
	case strings.HasPrefix(selector, "#"):
		idx, err := strconv.Atoi(strings.TrimPrefix(selector, "#"))
		if err != nil {
			return ""
		}
		for _, ts := range snap.Targets {
			if idx >= 0 && idx < len(ts.Pipeline) {
				return ts.Pipeline[idx].RunID
			}
		}
		return ""
	default:
		return ""
	}
}

// --- test entrypoints ---

// scriptSeed is the target-branch content every scenario starts from,
// shared by both suites so scripts don't need a "seed" command of their own.
var scriptSeed = map[string]string{"README.md": "seed\n"}

// TestScriptFake runs every testdata/script/*.txtar scenario against the
// in-memory fake-git harness (testHarness). Setup builds a fresh harness per
// script via newHarness — the same constructor daemon_test.go's own tests
// use — so this suite exercises no new daemon-wiring logic, only the DSL on
// top of it.
//
// Setup runs on the *testing.T passed to TestScriptFake itself (testscript's
// Env only exposes a minimal T interface — Skip/Fatal/Run/Verbose — not a
// full *testing.T, so it can't be handed to newHarness). In practice this
// only affects where a genuine harness-construction failure would be
// reported (at the suite's end, not the individual script's); every
// assertion a script actually makes runs through ts.Fatalf and fails its own
// subtest precisely.
func TestScriptFake(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:  "testdata/script",
		Cmds: commands(),
		Setup: func(env *testscript.Env) error {
			h := newHarness(t)
			h.git.seed("main", scriptSeed)
			env.Values[harnessKey{}] = scriptHarness(fakeScriptHarness{h: h})
			return nil
		},
	})
}

// TestScriptReal runs every testdata/script/*.txtar scenario against the
// real-git harness (integrationHarness, integration_test.go), reusing
// newIntegrationHarness the same way TestScriptFake reuses newHarness. It
// shares Cmds and every scenario file with TestScriptFake: none of the three
// ported scenarios needs anything fake-git can't cheaply satisfy (no genuine
// remote-ref plumbing, no real-git content-addressing edge case), so there is
// no fake-only subset to carve out here.
func TestScriptReal(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:  "testdata/script",
		Cmds: commands(),
		Setup: func(env *testscript.Env) error {
			gated := executor.NewGatedExecutor()
			h := newIntegrationHarness(t, nil, gated)
			h.remote.Seed("main", scriptSeed)
			env.Values[harnessKey{}] = scriptHarness(realScriptHarness{h: h, gated: gated})
			return nil
		},
	})
}

// --- commands ---

// commands returns the Cmds map shared by TestScriptFake and TestScriptReal.
// See the package-doc-style command vocabulary comment at the top of this
// file for what each one does.
func commands() map[string]func(ts *testscript.TestScript, neg bool, args []string) {
	return map[string]func(ts *testscript.TestScript, neg bool, args []string){
		"push-candidate":          cmdPushCandidate,
		"repush":                  cmdRepush,
		"delete-candidate":        cmdDeleteCandidate,
		"direct-push":             cmdDirectPush,
		"tick":                    cmdTick,
		"await-started":           cmdAwaitStarted,
		"release-check":           cmdReleaseCheck,
		"assert-event":            cmdAssertEvent,
		"assert-target-is-merge":  cmdAssertTargetIsMerge,
		"assert-slot-gone":        cmdAssertSlotGone,
		"assert-slot-parked":      cmdAssertSlotParked,
		"assert-slot-parked-none": cmdAssertSlotParkedNone,
		"set-mode":                cmdSetMode,
		"assert-pipeline-depth":   cmdAssertPipelineDepth,
		"assert-landed-order":     cmdAssertLandedOrder,
		"assert-target-chain":     cmdAssertTargetChain,
	}
}

// getHarness retrieves the scriptHarness Setup stored in env.Values, failing
// the script with a clear message if Setup never ran (a Cmds/Params wiring
// bug, not a script bug).
func getHarness(ts *testscript.TestScript) scriptHarness {
	h, _ := ts.Value(harnessKey{}).(scriptHarness)
	if h == nil {
		ts.Fatalf("no scriptHarness registered; Setup did not set harnessKey{} in Env.Values")
	}
	return h
}

// readFilesDir reads every regular file under dir (interpreted relative to
// the script's current directory, per ts.MkAbs) into a path -> content map,
// keyed by slash-separated paths relative to dir — the shape push-candidate,
// repush, and direct-push need for the fake and real git harnesses alike.
// dir is ordinarily a directory unpacked from the script's own txtar
// archive (a "-- dir/path --" section).
func readFilesDir(ts *testscript.TestScript, dir string) map[string]string {
	abs := ts.MkAbs(dir)
	info, err := os.Stat(abs)
	if err != nil {
		ts.Fatalf("read files dir %s: %v", dir, err)
	}
	if !info.IsDir() {
		ts.Fatalf("read files dir %s: not a directory", dir)
	}
	out := map[string]string{}
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		ts.Fatalf("read files dir %s: %v", dir, err)
	}
	return out
}

func cmdPushCandidate(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("push-candidate does not support !")
	}
	if len(args) != 4 {
		ts.Fatalf("usage: push-candidate <target> <user> <topic> <dir>")
	}
	target, user, topic, dir := args[0], args[1], args[2], args[3]
	files := readFilesDir(ts, dir)
	ref, sha := getHarness(ts).pushCandidate(target, user, topic, files)
	ts.Logf("push-candidate: %s -> %s (from %s)", ref, sha, dir)
}

func cmdRepush(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("repush does not support !")
	}
	if len(args) != 4 {
		ts.Fatalf("usage: repush <target> <user> <topic> <dir>")
	}
	target, user, topic, dir := args[0], args[1], args[2], args[3]
	files := readFilesDir(ts, dir)
	sha := getHarness(ts).repush(target, user, topic, files)
	ts.Logf("repush: %s -> %s (from %s)", candidateRef(target, user, topic), sha, dir)
}

func cmdDeleteCandidate(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("delete-candidate does not support !")
	}
	if len(args) != 3 {
		ts.Fatalf("usage: delete-candidate <target> <user> <topic>")
	}
	getHarness(ts).deleteCandidate(args[0], args[1], args[2])
}

func cmdDirectPush(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("direct-push does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: direct-push <target> <dir>")
	}
	target, dir := args[0], args[1]
	files := readFilesDir(ts, dir)
	getHarness(ts).directPush(target, files)
}

func cmdTick(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("tick does not support !")
	}
	if len(args) != 0 {
		ts.Fatalf("usage: tick")
	}
	getHarness(ts).tick()
}

// parseSelector strips an optional leading "@<selector>" argument
// (docs/plans/phase5.md §5.1) off args, returning the selector's bare form
// (the "@" itself stripped, so resolveRunSelector sees "topic:foo" or "#1")
// and the remaining arguments. No leading "@" means no selector: "" (head
// run, resolveRunSelector's own "omitted" case).
func parseSelector(args []string) (selector string, rest []string) {
	if len(args) > 0 && strings.HasPrefix(args[0], "@") {
		return strings.TrimPrefix(args[0], "@"), args[1:]
	}
	return "", args
}

func cmdAwaitStarted(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("await-started does not support !")
	}
	selector, rest := parseSelector(args)
	if len(rest) != 1 {
		ts.Fatalf("usage: await-started [@<selector>] <name>")
	}
	getHarness(ts).awaitStarted(selector, rest[0])
}

func cmdReleaseCheck(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("release-check does not support !")
	}
	selector, rest := parseSelector(args)
	if len(rest) != 2 {
		ts.Fatalf("usage: release-check [@<selector>] <name> <passed|failed|skipped>")
	}
	name := rest[0]
	status := parseCheckStatus(ts, rest[1])
	getHarness(ts).releaseCheck(selector, name, status)
}

func parseCheckStatus(ts *testscript.TestScript, s string) core.CheckStatus {
	switch s {
	case "passed":
		return core.CheckPassed
	case "failed":
		return core.CheckFailed
	case "skipped":
		return core.CheckSkipped
	default:
		ts.Fatalf("release-check: unknown status %q, want one of passed|failed|skipped", s)
		panic("unreachable") // ts.Fatalf panics with an internal sentinel; this satisfies the compiler.
	}
}

func parseEventKind(ts *testscript.TestScript, s string) core.EventKind {
	switch s {
	case "queued":
		return core.EventQueued
	case "trial-clean":
		return core.EventTrialClean
	case "trial-conflict":
		return core.EventTrialConflict
	case "check-started":
		return core.EventCheckStarted
	case "check-finished":
		return core.EventCheckFinished
	case "landed":
		return core.EventLanded
	case "rejected":
		return core.EventRejected
	case "skipped":
		return core.EventSkipped
	case "error":
		return core.EventError
	case "ignored-ref":
		return core.EventIgnoredRef
	default:
		ts.Fatalf("assert-event: unknown kind %q", s)
		panic("unreachable")
	}
}

func cmdAssertEvent(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 1 {
		ts.Fatalf("usage: [!] assert-event <kind>")
	}
	kind := parseEventKind(ts, args[0])
	evs := getHarness(ts).events()
	found := false
	for _, e := range evs {
		if e.Kind == kind {
			found = true
			break
		}
	}
	switch {
	case neg && found:
		ts.Fatalf("assert-event: found an unexpected %s event (want none among %d captured events)", args[0], len(evs))
	case !neg && !found:
		ts.Fatalf("assert-event: no %s event found among %d captured events", args[0], len(evs))
	}
}

func cmdAssertTargetIsMerge(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-target-is-merge does not support !")
	}
	if len(args) != 1 {
		ts.Fatalf("usage: assert-target-is-merge <target>")
	}
	target := args[0]
	h := getHarness(ts)
	recs := h.records()
	if len(recs) == 0 {
		ts.Fatalf("assert-target-is-merge: no run records captured yet")
	}
	last := recs[len(recs)-1]
	tip := h.targetRef(target)
	if tip == "" {
		ts.Fatalf("assert-target-is-merge: target %q has no ref", target)
	}
	if tip != last.MergeSHA {
		ts.Fatalf("assert-target-is-merge: target %s tip = %s, want the last record's MergeSHA %s", target, tip, last.MergeSHA)
	}
	parents := h.commitParents(tip)
	if len(parents) != 2 || parents[1] != last.Candidate.SHA {
		ts.Fatalf("assert-target-is-merge: parents of %s = %v, want [<base> %s] (Invariant 1/6: candidate SHA verbatim as parent[1])", tip, parents, last.Candidate.SHA)
	}
}

func cmdAssertSlotGone(ts *testscript.TestScript, neg bool, args []string) {
	if len(args) != 3 {
		ts.Fatalf("usage: [!] assert-slot-gone <target> <user> <topic>")
	}
	target, user, topic := args[0], args[1], args[2]
	ref := candidateRef(target, user, topic)
	exists := getHarness(ts).slotRef(target, user, topic) != ""
	switch {
	case neg && !exists:
		ts.Fatalf("assert-slot-gone: %s is already gone, want it present", ref)
	case !neg && exists:
		ts.Fatalf("assert-slot-gone: %s still exists, want it gone", ref)
	}
}

func cmdAssertSlotParked(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-slot-parked does not support !; use assert-slot-gone instead")
	}
	if len(args) != 3 {
		ts.Fatalf("usage: assert-slot-parked <target> <user> <topic>")
	}
	target, user, topic := args[0], args[1], args[2]
	ref := candidateRef(target, user, topic)
	h := getHarness(ts)
	if h.slotRef(target, user, topic) == "" {
		ts.Fatalf("assert-slot-parked: %s does not exist", ref)
	}
	var last *core.RunRecord
	for _, r := range h.records() {
		if r.Candidate.Ref == ref {
			last = r // walk forward: keep the most recent
		}
	}
	if last == nil {
		ts.Fatalf("assert-slot-parked: no run record found for %s", ref)
	}
	switch last.Outcome {
	case core.OutcomeRejected, core.OutcomeConflict, core.OutcomeError:
	default:
		ts.Fatalf("assert-slot-parked: %s last outcome = %v, want a parking outcome (Rejected, Conflict, or Error)", ref, last.Outcome)
	}
}

// cmdAssertSlotParkedNone asserts the ref-still-exists-but-unparked shape a
// pipeline bubble or a mid-window invalidation leaves behind (docs/plans/
// phase5.md §5.1): Skipped, NOT parked, free to re-queue on the very next
// refill — the complement of cmdAssertSlotParked's sticky Rejected/Conflict/
// Error case.
func cmdAssertSlotParkedNone(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-slot-parked-none does not support !; use assert-slot-parked instead")
	}
	if len(args) != 3 {
		ts.Fatalf("usage: assert-slot-parked-none <target> <user> <topic>")
	}
	target, user, topic := args[0], args[1], args[2]
	ref := candidateRef(target, user, topic)
	h := getHarness(ts)
	if h.slotRef(target, user, topic) == "" {
		ts.Fatalf("assert-slot-parked-none: %s does not exist (want present, unparked)", ref)
	}
	if h.slotParked(target, user, topic) {
		ts.Fatalf("assert-slot-parked-none: %s is parked, want unparked (a bubble/invalidation re-queue)", ref)
	}
}

func cmdSetMode(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("set-mode does not support !")
	}
	if len(args) != 3 {
		ts.Fatalf("usage: set-mode <target> <mode> <n>")
	}
	target, mode := args[0], args[1]
	var n int
	if _, err := fmt.Sscanf(args[2], "%d", &n); err != nil {
		ts.Fatalf("set-mode: invalid n %q: %v", args[2], err)
	}
	getHarness(ts).setMode(target, mode, n)
}

func cmdAssertPipelineDepth(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-pipeline-depth does not support !")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: assert-pipeline-depth <target> <n>")
	}
	var want int
	if _, err := fmt.Sscanf(args[1], "%d", &want); err != nil {
		ts.Fatalf("assert-pipeline-depth: invalid n %q: %v", args[1], err)
	}
	target := args[0]
	got := getHarness(ts).pipelineDepth(target)
	if got != want {
		ts.Fatalf("assert-pipeline-depth: %s pipeline depth = %d, want %d", target, got, want)
	}
}

func cmdAssertLandedOrder(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-landed-order does not support !")
	}
	if len(args) < 2 {
		ts.Fatalf("usage: assert-landed-order <target> <topic>...")
	}
	target := args[0]
	wantTopics := args[1:]
	h := getHarness(ts)

	var gotTopics []string
	for _, e := range h.events() {
		if e.Kind == core.EventLanded && e.Target == target {
			gotTopics = append(gotTopics, e.Candidate.Topic)
		}
	}
	if len(gotTopics) != len(wantTopics) {
		ts.Fatalf("assert-landed-order: got %d EventLanded topics %v for target %s, want %d %v", len(gotTopics), gotTopics, target, len(wantTopics), wantTopics)
	}
	for i := range wantTopics {
		if gotTopics[i] != wantTopics[i] {
			ts.Fatalf("assert-landed-order: EventLanded[%d].Topic = %q, want %q (got order %v, want %v)", i, gotTopics[i], wantTopics[i], gotTopics, wantTopics)
		}
	}
}

// cmdAssertTargetChain generalizes cmdAssertTargetIsMerge to a whole chain
// (docs/plans/phase5.md §5.1): topics are given oldest-first (build/FIFO
// order), so target's tip is topics[last]'s own link, and walking
// parent[0] from the tip steps back through the chain in reverse.
func cmdAssertTargetChain(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("assert-target-chain does not support !")
	}
	if len(args) < 2 {
		ts.Fatalf("usage: assert-target-chain <target> <topic>...")
	}
	target := args[0]
	topics := args[1:]
	h := getHarness(ts)

	// Resolve each topic to its landed candidate SHA from the most recent
	// matching RunRecord (assert-slot-parked's own "walk forward, keep the
	// latest" rule).
	shaByTopic := make(map[string]string)
	for _, r := range h.records() {
		shaByTopic[r.Candidate.Topic] = r.Candidate.SHA
	}
	wantSHAs := make([]string, len(topics))
	for i, topic := range topics {
		sha, ok := shaByTopic[topic]
		if !ok {
			ts.Fatalf("assert-target-chain: no run record found for topic %q", topic)
		}
		wantSHAs[i] = sha
	}

	tip := h.targetRef(target)
	if tip == "" {
		ts.Fatalf("assert-target-chain: target %q has no ref", target)
	}

	oid := tip
	for i := len(topics) - 1; i >= 0; i-- {
		parents := h.commitParents(oid)
		if len(parents) != 2 {
			ts.Fatalf("assert-target-chain: commit %s (topic %q) has %d parents, want 2 (a --no-ff chain link)", oid, topics[i], len(parents))
		}
		if parents[1] != wantSHAs[i] {
			ts.Fatalf("assert-target-chain: commit %s parent[1] = %s, want topic %q's candidate SHA %s verbatim", oid, parents[1], topics[i], wantSHAs[i])
		}
		oid = parents[0]
	}
}
