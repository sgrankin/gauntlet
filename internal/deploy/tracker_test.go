package deploy_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/deploy"
)

// stubGit is a deploy.Git whose every method is a field, for the cases the
// scenario harness can't reach cheaply: a ref listing that fails, a push
// that fails with something other than a lost CAS.
//
// The lane-runner half of the interface lives in run_test.go, alongside the
// tests that need it. mu guards everything: a Tracker built with a runner
// calls this from lane goroutines while a reconcile pass calls it from the
// test's own. The tests in THIS file build no runner, so nothing there
// races and their direct field reads stay honest.
type stubGit struct {
	mu       sync.Mutex
	refs     map[string]string
	local    map[string]string
	refsErr  error
	localErr error
	casErr   func(remoteRef string) error
	casCalls []casCall

	// Lane-runner fixtures (run_test.go): tree is the revision content
	// every ReadFileFromTree/ExportTree serves, and the *Err fields make
	// each pre-graph step fail on demand.
	tree      map[string]string
	readErr   error
	pinErr    error
	exportErr error
	pins      map[string]bool
}

type casCall struct{ Ref, Old, New string }

func (g *stubGit) ListRefs(context.Context) (map[string]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refsErr != nil {
		return nil, g.refsErr
	}
	return g.refs, nil
}

func (g *stubGit) ListLocalRefs(_ context.Context, prefix string) (map[string]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.localErr != nil {
		return nil, g.localErr
	}
	out := map[string]string{}
	for name, oid := range g.local {
		if strings.HasPrefix(name, prefix) {
			out[name] = oid
		}
	}
	return out, nil
}

func (g *stubGit) CASUpdate(_ context.Context, remoteRef, oldOID, newOID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.casCalls = append(g.casCalls, casCall{Ref: remoteRef, Old: oldOID, New: newOID})
	if g.casErr != nil {
		if err := g.casErr(remoteRef); err != nil {
			return err
		}
	}
	// A successful push mutates the "remote" this stub also lists from —
	// no mirror lag here, unlike the scenario harness, because these tests
	// are about the tracker's decisions rather than about fetch cadence.
	if g.refs == nil {
		g.refs = map[string]string{}
	}
	if newOID == "" {
		delete(g.refs, remoteRef)
	} else {
		g.refs[remoteRef] = newOID
	}
	return nil
}

func TestRefNaming(t *testing.T) {
	if got := deploy.DesiredRef("prod"); got != "refs/heads/deploy/prod" {
		t.Errorf("DesiredRef(prod) = %q", got)
	}
	if got := deploy.ObservedRef("prod"); got != "refs/gauntlet/deployed/prod" {
		t.Errorf("ObservedRef(prod) = %q", got)
	}
	if !strings.HasPrefix(deploy.DesiredRef("prod"), deploy.DesiredRefPrefix) {
		t.Error("DesiredRef does not use DesiredRefPrefix")
	}
	if !strings.HasPrefix(deploy.ObservedRef("prod"), deploy.ObservedRefPrefix) {
		t.Error("ObservedRef does not use ObservedRefPrefix")
	}
}

// TestDesiredRefIsNotACandidateRef pins the queue-isolation property from
// this side of the boundary (internal/queue is not touched by this slice):
// the queue's candidate grammar is refs/heads/for/<target>/..., so a
// desired ref can never parse as a candidate and never raises an
// ignored-ref event. If the desired namespace ever moved under
// refs/heads/for/, every deploy would start looking like a submission.
func TestDesiredRefIsNotACandidateRef(t *testing.T) {
	const candidatePrefix = "refs/heads/for/"
	if strings.HasPrefix(deploy.DesiredRefPrefix, candidatePrefix) {
		t.Fatalf("DesiredRefPrefix %q is under the queue's candidate namespace %q",
			deploy.DesiredRefPrefix, candidatePrefix)
	}
	if strings.HasPrefix(deploy.ObservedRefPrefix, "refs/heads/") {
		t.Fatalf("ObservedRefPrefix %q is under refs/heads/; observed refs must trigger no branch machinery",
			deploy.ObservedRefPrefix)
	}
}

// TestFetchRefspecStaysOutOfRemotesNamespace: the refspec rides the
// queue's own fetch, so its destination must be outside
// refs/remotes/origin/* or it would inject refs straight into
// gitx.ListRefs's view of ground truth.
func TestFetchRefspecStaysOutOfRemotesNamespace(t *testing.T) {
	_, dst, ok := strings.Cut(deploy.FetchRefspec, ":")
	if !ok {
		t.Fatalf("FetchRefspec %q has no destination half", deploy.FetchRefspec)
	}
	if strings.HasPrefix(dst, "refs/remotes/") {
		t.Fatalf("FetchRefspec destination %q lands in the remote-tracking namespace", dst)
	}
	if dst != deploy.ObservedRefPrefix+"*" {
		t.Errorf("FetchRefspec destination = %q, want %q", dst, deploy.ObservedRefPrefix+"*")
	}
}

func TestReconcileOnce_NoEnvironments(t *testing.T) {
	git := &stubGit{refs: map[string]string{"refs/heads/main": "aaa"}}
	tr := deploy.New(deploy.Params{Git: git})

	if err := tr.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(git.casCalls) != 0 {
		t.Errorf("CASUpdate calls = %+v, want none", git.casCalls)
	}
	snap := tr.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot = nil after a successful pass")
	}
	if len(snap.Lanes) != 0 {
		t.Errorf("Lanes = %+v, want none", snap.Lanes)
	}
	if len(git.refs) != 1 || git.refs["refs/heads/main"] != "aaa" {
		t.Errorf("refs = %v, want unchanged", git.refs)
	}
}

func TestReconcileOnce_RefListingErrors(t *testing.T) {
	boom := errors.New("boom")
	envs := []deploy.Environment{{Name: "dev", SourceBranch: "main", Mode: deploy.ModeTrack}}

	t.Run("ListRefs", func(t *testing.T) {
		git := &stubGit{refsErr: boom}
		tr := deploy.New(deploy.Params{Environments: envs, Git: git})
		err := tr.ReconcileOnce(context.Background())
		if !errors.Is(err, boom) {
			t.Fatalf("ReconcileOnce = %v, want it to wrap %v", err, boom)
		}
		if snap := tr.Snapshot(); snap != nil {
			t.Errorf("Snapshot = %+v, want nil (a failed pass publishes nothing)", snap)
		}
		if len(git.casCalls) != 0 {
			t.Errorf("CASUpdate calls = %+v, want none — the tracker knew nothing", git.casCalls)
		}
	})

	t.Run("ListLocalRefs", func(t *testing.T) {
		git := &stubGit{refs: map[string]string{"refs/heads/main": "aaa"}, localErr: boom}
		tr := deploy.New(deploy.Params{Environments: envs, Git: git})
		if err := tr.ReconcileOnce(context.Background()); !errors.Is(err, boom) {
			t.Fatalf("ReconcileOnce = %v, want it to wrap %v", err, boom)
		}
		if snap := tr.Snapshot(); snap != nil {
			t.Errorf("Snapshot = %+v, want nil", snap)
		}
	})

	t.Run("a later failed pass leaves the last good snapshot in place", func(t *testing.T) {
		git := &stubGit{refs: map[string]string{"refs/heads/main": "aaa"}}
		tr := deploy.New(deploy.Params{Environments: envs, Git: git})
		if err := tr.ReconcileOnce(context.Background()); err != nil {
			t.Fatalf("first ReconcileOnce: %v", err)
		}
		good := tr.Snapshot()
		git.refsErr = boom
		if err := tr.ReconcileOnce(context.Background()); err == nil {
			t.Fatal("second ReconcileOnce succeeded, want an error")
		}
		if got := tr.Snapshot(); got == nil || !got.At.Equal(good.At) {
			t.Errorf("Snapshot = %+v, want the previous complete one", got)
		}
	})
}

// TestReconcileOnce_PushErrorIsPerLane: a push failure is recorded on its
// own lane and stops nothing — the lanes after it still run, and
// ReconcileOnce still reports success, because environments are
// independent and one unwritable ref is no reason to freeze the rest.
func TestReconcileOnce_PushErrorIsPerLane(t *testing.T) {
	boom := errors.New("remote rejected")
	git := &stubGit{
		refs: map[string]string{"refs/heads/main": "aaa"},
		casErr: func(ref string) error {
			if ref == deploy.DesiredRef("first") {
				return boom
			}
			return nil
		},
	}
	var log strings.Builder
	tr := deploy.New(deploy.Params{
		Environments: []deploy.Environment{
			{Name: "first", SourceBranch: "main", Mode: deploy.ModeTrack},
			{Name: "second", SourceBranch: "main", Mode: deploy.ModeTrack},
		},
		Git: git,
		Log: &log,
	})

	if err := tr.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce = %v, want nil (a push failure is per-lane)", err)
	}
	if len(git.casCalls) != 2 {
		t.Fatalf("CASUpdate calls = %+v, want both lanes attempted", git.casCalls)
	}
	snap := tr.Snapshot()
	if len(snap.Lanes) != 2 {
		t.Fatalf("Lanes = %+v, want 2", snap.Lanes)
	}
	if !strings.Contains(snap.Lanes[0].LastError, "remote rejected") {
		t.Errorf("Lanes[0].LastError = %q, want the push failure", snap.Lanes[0].LastError)
	}
	if snap.Lanes[0].Desired != "" {
		t.Errorf("Lanes[0].Desired = %q, want it unchanged after a failed push", snap.Lanes[0].Desired)
	}
	if snap.Lanes[1].LastError != "" {
		t.Errorf("Lanes[1].LastError = %q, want clean", snap.Lanes[1].LastError)
	}
	if snap.Lanes[1].Desired != "aaa" {
		t.Errorf("Lanes[1].Desired = %q, want it advanced to main's tip", snap.Lanes[1].Desired)
	}
	if !strings.Contains(log.String(), "first") {
		t.Errorf("Log = %q, want one line naming the failing environment", log.String())
	}
}

// TestReconcileOnce_LostCASIsNotLogged: a lost CAS is recorded but not
// logged — it is an ordinary outcome under a second writer, and logging it
// would train operators to ignore the log.
func TestReconcileOnce_LostCASIsNotLogged(t *testing.T) {
	git := &stubGit{
		refs:   map[string]string{"refs/heads/main": "aaa"},
		casErr: func(string) error { return fmt.Errorf("push: %w", core.ErrCASStale) },
	}
	var log strings.Builder
	tr := deploy.New(deploy.Params{
		Environments: []deploy.Environment{{Name: "dev", SourceBranch: "main", Mode: deploy.ModeTrack}},
		Git:          git,
		Log:          &log,
	})

	if err := tr.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce = %v, want nil", err)
	}
	lane := tr.Snapshot().Lanes[0]
	if lane.LastError == "" {
		t.Error("LastError is empty, want the lost CAS recorded")
	}
	if log.String() != "" {
		t.Errorf("Log = %q, want silence for an ordinary lost CAS", log.String())
	}
	// Exactly one attempt: no retry inside the tick.
	if len(git.casCalls) != 1 {
		t.Errorf("CASUpdate calls = %+v, want exactly one (no in-tick retry)", git.casCalls)
	}
}

// TestReconcileOnce_ManualNeverWrites is the invariant a manual
// environment exists for, asserted at the Git surface rather than through
// refs: no CAS is even attempted.
func TestReconcileOnce_ManualNeverWrites(t *testing.T) {
	git := &stubGit{refs: map[string]string{"refs/heads/main": "aaa"}}
	tr := deploy.New(deploy.Params{
		Environments: []deploy.Environment{{Name: "prod", SourceBranch: "main", Mode: deploy.ModeManual}},
		Git:          git,
	})
	for i := 0; i < 3; i++ {
		if err := tr.ReconcileOnce(context.Background()); err != nil {
			t.Fatalf("ReconcileOnce: %v", err)
		}
	}
	if len(git.casCalls) != 0 {
		t.Fatalf("CASUpdate calls = %+v, want none for a manual environment", git.casCalls)
	}
	lane := tr.Snapshot().Lanes[0]
	if lane.Desired != "" {
		t.Errorf("Desired = %q, want empty (never deployed)", lane.Desired)
	}
	if !lane.Drift {
		t.Error("Drift = false; a manual lane whose source has a tip it isn't running IS drifting")
	}
}

// TestReconcileOnce_FirstTickCreatesDesiredRef pins the CAS-create: the
// tracker asserts the ref does not exist (old == "") rather than
// force-pushing over whatever is there.
func TestReconcileOnce_FirstTickCreatesDesiredRef(t *testing.T) {
	git := &stubGit{refs: map[string]string{"refs/heads/main": "aaa"}}
	tr := deploy.New(deploy.Params{
		Environments: []deploy.Environment{{Name: "dev", SourceBranch: "main", Mode: deploy.ModeTrack}},
		Git:          git,
	})
	if err := tr.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	want := casCall{Ref: "refs/heads/deploy/dev", Old: "", New: "aaa"}
	if len(git.casCalls) != 1 || git.casCalls[0] != want {
		t.Fatalf("CASUpdate calls = %+v, want exactly %+v", git.casCalls, want)
	}
}

// TestSnapshotFields walks every published field of a lane against a
// hand-built ref picture, including LastAdvance through an injected clock.
func TestSnapshotFields(t *testing.T) {
	clock := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	git := &stubGit{
		refs: map[string]string{
			"refs/heads/main":             "tip2",
			"refs/heads/deploy/dev":       "tip2",
			"refs/heads/deploy/prod":      "tip1",
			"refs/heads/deploy/stale":     "tip1",
			"refs/heads/deploy/undeploye": "tip1",
		},
		local: map[string]string{
			"refs/gauntlet/deployed/dev":   "tip2",
			"refs/gauntlet/deployed/stale": "tip1",
		},
	}
	tr := deploy.New(deploy.Params{
		Environments: []deploy.Environment{
			// In sync with its source and with what it deployed.
			{Name: "dev", SourceBranch: "main", Mode: deploy.ModeTrack},
			// Manual, deployed nothing, source ahead: drift, not in sync.
			{Name: "prod", SourceEnv: "dev", Mode: deploy.ModeManual},
			// Manual, running exactly what it deployed, source ahead.
			{Name: "stale", SourceBranch: "main", Mode: deploy.ModeManual},
			// Source environment has never deployed: no tip at all.
			{Name: "undeploye", SourceEnv: "prod", Mode: deploy.ModeManual},
		},
		Git: git,
		Now: func() time.Time { return clock },
	})
	if err := tr.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	snap := tr.Snapshot()
	if !snap.At.Equal(clock) {
		t.Errorf("At = %v, want the injected %v", snap.At, clock)
	}

	want := []deploy.LaneState{
		{Env: "dev", Mode: deploy.ModeTrack, Source: "main", SourceTip: "tip2", Desired: "tip2", Observed: "tip2", InSync: true},
		{Env: "prod", Mode: deploy.ModeManual, Source: "env=dev", SourceTip: "tip2", Desired: "tip1", Observed: "", Drift: true},
		{Env: "stale", Mode: deploy.ModeManual, Source: "main", SourceTip: "tip2", Desired: "tip1", Observed: "tip1", InSync: true, Drift: true},
		{Env: "undeploye", Mode: deploy.ModeManual, Source: "env=prod", SourceTip: "", Desired: "tip1", Observed: ""},
	}
	if len(snap.Lanes) != len(want) {
		t.Fatalf("Lanes = %+v, want %d lanes in declaration order", snap.Lanes, len(want))
	}
	for i, w := range want {
		if got := snap.Lanes[i]; got != w {
			t.Errorf("Lanes[%d] = %+v\n           want %+v", i, got, w)
		}
	}

	// LastAdvance stays zero for lanes this tracker never moved, and takes
	// the injected clock for one it does.
	for _, lane := range snap.Lanes {
		if !lane.LastAdvance.IsZero() {
			t.Errorf("%s: LastAdvance = %v, want zero (nothing advanced this pass)", lane.Env, lane.LastAdvance)
		}
	}

	git.refs["refs/heads/main"] = "tip3"
	clock = clock.Add(time.Minute)
	if err := tr.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second ReconcileOnce: %v", err)
	}
	dev := tr.Snapshot().Lanes[0]
	if !dev.LastAdvance.Equal(clock) {
		t.Errorf("dev.LastAdvance = %v, want the injected %v", dev.LastAdvance, clock)
	}
	if dev.Desired != "tip3" {
		t.Errorf("dev.Desired = %q, want tip3", dev.Desired)
	}
	// LastAdvance is remembered across ticks that change nothing.
	clock = clock.Add(time.Minute)
	advanced := dev.LastAdvance
	if err := tr.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("third ReconcileOnce: %v", err)
	}
	if got := tr.Snapshot().Lanes[0].LastAdvance; !got.Equal(advanced) {
		t.Errorf("dev.LastAdvance = %v after a no-op tick, want it held at %v", got, advanced)
	}
}

// TestSnapshotIsTheCallersOwnCopy: a caller may sort or retain Lanes
// without the next pass mutating it underneath them.
func TestSnapshotIsTheCallersOwnCopy(t *testing.T) {
	git := &stubGit{refs: map[string]string{"refs/heads/main": "aaa"}}
	tr := deploy.New(deploy.Params{
		Environments: []deploy.Environment{{Name: "dev", SourceBranch: "main", Mode: deploy.ModeManual}},
		Git:          git,
	})
	if err := tr.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	a, b := tr.Snapshot(), tr.Snapshot()
	if &a.Lanes[0] == &b.Lanes[0] {
		t.Fatal("two Snapshot calls share a Lanes backing array")
	}
	a.Lanes[0].Env = "mutated"
	if tr.Snapshot().Lanes[0].Env != "dev" {
		t.Error("mutating a returned Snapshot changed the published one")
	}
}

func TestSnapshotBeforeFirstPass(t *testing.T) {
	tr := deploy.New(deploy.Params{Git: &stubGit{}})
	if snap := tr.Snapshot(); snap != nil {
		t.Errorf("Snapshot = %+v before any pass, want nil", snap)
	}
}
