package queue

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

type integrationReviews struct {
	candidates []core.Candidate
	landed     map[string]string
	reject     bool
}

func (s *integrationReviews) Candidates(context.Context) ([]core.Candidate, error) {
	return append([]core.Candidate(nil), s.candidates...), nil
}
func (s *integrationReviews) Validate(context.Context, core.Candidate) error {
	if s.reject {
		return fmt.Errorf("approval withdrawn")
	}
	return nil
}
func (s *integrationReviews) Landed(_ context.Context, c core.Candidate, sha string) error {
	if s.landed == nil {
		s.landed = map[string]string{}
	}
	s.landed[c.Ref] = sha
	var remaining []core.Candidate
	for _, next := range s.candidates {
		if next.Ref == c.Ref {
			continue
		}
		if next.DependsOn == c.Ref {
			next.DependsOn = ""
		}
		remaining = append(remaining, next)
	}
	s.candidates = remaining
	return nil
}

func TestIntegration_LinearStackBatch(t *testing.T) {
	remote := testutil.NewRemote(t)
	files := checkSpecFile("test")
	files["shared.txt"] = "base\n"
	remote.Seed("main", files)
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "a", map[string]string{"shared.txt": "base\nA\n"})
	a := remote.Ref(ref)
	remote.SetRef("refs/heads/source", a)
	remote.DeleteCandidate(ref)
	remote.DirectPush("source", map[string]string{"shared.txt": "base\nA\nB\n"})
	b := remote.Ref("refs/heads/source")
	ca := core.Candidate{Ref: "refs/heads/for/main/github/pr-0000000001", Target: "main", SHA: a, Source: "github", SourceBase: base, Message: "Change A\n\nPR body A", Version: "a"}
	cb := core.Candidate{Ref: "refs/heads/for/main/github/pr-0000000002", Target: "main", SHA: b, Source: "github", SourceBase: a, Message: "Change B\n\nPR body B", Version: "b", DependsOn: ca.Ref}
	reviews := &integrationReviews{candidates: []core.Candidate{cb, ca}}
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, remote, gated, config.Target{Name: "main", Branch: "main", Landing: "squash", Mode: "batch", MaxBatch: 2})
	h.d.cfg.Reviews = reviews
	h.reconcile()
	r := h.d.headRun("main")
	if r == nil || len(r.members) != 2 {
		t.Fatalf("no stack batch: %+v", r)
	}
	exactTip := r.chainTip
	h.awaitStarted(gated, r.runID, "test")
	h.releaseGated(gated, r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if got := remote.Ref("refs/heads/main"); got != exactTip {
		t.Fatalf("landed %s, tested %s", got, exactTip)
	}
	parent := remote.Parents(exactTip)
	if len(parent) != 1 {
		t.Fatalf("B parents: %v", parent)
	}
	first := parent[0]
	if p := remote.Parents(first); len(p) != 1 || p[0] != base {
		t.Fatalf("A parents: %v", p)
	}
	if reviews.landed[ca.Ref] != first || reviews.landed[cb.Ref] != exactTip {
		t.Fatalf("wrong association: %v", reviews.landed)
	}
	got, err := h.git.ReadFileFromTree(context.Background(), exactTip, "shared.txt")
	if err != nil || string(got) != "base\nA\nB\n" {
		t.Fatalf("stack delta lost: %q, %v", got, err)
	}
	for _, c := range []core.Candidate{ca, cb} {
		sha, err := h.git.FindLanding(context.Background(), exactTip, c.Ref, c.SHA, c.Version)
		if err != nil || sha != reviews.landed[c.Ref] {
			t.Fatalf("ledger lookup %s %v", sha, err)
		}
		if ancestor, err := h.git.IsAncestor(context.Background(), c.SHA, exactTip); err != nil || ancestor {
			t.Fatalf("source history reached linear target: %v %v", ancestor, err)
		}
	}
	if remote.Ref("refs/heads/source") != b {
		t.Fatal("contributor head changed")
	}
}

func TestIntegration_LinearCrashRecovery(t *testing.T) {
	remote := testutil.NewRemote(t)
	remote.Seed("main", checkSpecFile("test"))
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "a", map[string]string{"a": "one\n"})
	source := remote.Ref(ref)
	target := config.Target{Name: "main", Branch: "main", Landing: "squash"}
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, remote, gated, target)
	h.reconcile()
	r := h.d.headRun("main")
	if r == nil {
		t.Fatal("no trial")
	}
	h.awaitStarted(gated, r.runID, "test")
	if err := h.git.CASUpdate(context.Background(), "refs/heads/main", base, r.chainTip); err != nil {
		t.Fatal(err)
	}
	gated.Release(r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	fresh := newIntegrationHarness(t, remote, executor.NewGatedExecutor(), target)
	fresh.reconcile()
	if remote.Ref(ref) != "" {
		t.Fatal("recovery did not delete slot")
	}
	if remote.Ref("refs/heads/main") != r.chainTip {
		t.Fatal("recovery rewrote landing")
	}
	records := fresh.ch.Records()
	if len(records) != 1 || !records[0].Recovered || records[0].MergeSHA != r.chainTip || records[0].Candidate.SHA != source {
		t.Fatalf("recovery: %+v", records)
	}
	if len(records[0].Checks) != 0 || !strings.Contains(records[0].Detail, "recorded in target history") {
		t.Fatalf("recovery checks/detail: %+v", records[0])
	}
}

func TestIntegration_LinearTargetMovedAndApprovalWithdrawn(t *testing.T) {
	for _, race := range []string{"target", "approval"} {
		t.Run(race, func(t *testing.T) {
			remote := testutil.NewRemote(t)
			remote.Seed("main", checkSpecFile("test"))
			base := remote.Ref("refs/heads/main")
			ref := remote.PushCandidate("main", "alice", "a", map[string]string{"a": "one\n"})
			source := remote.Ref(ref)
			remote.SetRef("refs/heads/source", source)
			remote.DeleteCandidate(ref)
			c := core.Candidate{Ref: "refs/heads/for/main/github/pr-0000000001", Target: "main", SHA: source, Source: "github", SourceBase: base, Message: "A", Version: "a"}
			reviews := &integrationReviews{candidates: []core.Candidate{c}}
			gated := executor.NewGatedExecutor()
			h := newIntegrationHarness(t, remote, gated, config.Target{Name: "main", Branch: "main", Landing: "squash"})
			h.d.cfg.Reviews = reviews
			h.reconcile()
			r := h.d.headRun("main")
			if r == nil {
				t.Fatal("no trial")
			}
			h.awaitStarted(gated, r.runID, "test")
			if race == "target" {
				remote.DirectPush("main", map[string]string{"other": "moved\n"})
			} else {
				reviews.reject = true
			}
			expected := remote.Ref("refs/heads/main")
			h.releaseGated(gated, r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
			if remote.Ref("refs/heads/main") != expected || len(reviews.landed) != 0 {
				t.Fatal("stale or unapproved trial landed")
			}
			if race == "target" {
				h.reconcile()
				next := h.d.headRun("main")
				if next == nil || next.baseOID != expected || next.chainTip == r.chainTip {
					t.Fatal("target movement did not rebuild trial")
				}
				h.awaitStarted(gated, next.runID, "test")
				reviews.candidates = nil
				h.reconcile()
			}
		})
	}
}

func TestIntegration_LinearReviewRecoveryAfterMetadataEdit(t *testing.T) {
	remote := testutil.NewRemote(t)
	remote.Seed("main", checkSpecFile("test"))
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "review", map[string]string{"a": "review\n"})
	source := remote.Ref(ref)
	remote.SetRef("refs/heads/source", source)
	remote.DeleteCandidate(ref)
	c := core.Candidate{Ref: "refs/heads/for/main/github/pr-0000000001", Target: "main", SHA: source, Source: "github", Message: "Old reviewed title", Version: "old"}
	reviews := &integrationReviews{candidates: []core.Candidate{c}}
	target := config.Target{Name: "main", Branch: "main", Landing: "squash"}
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, remote, gated, target)
	h.d.cfg.Reviews = reviews
	h.reconcile()
	r := h.d.headRun("main")
	if r == nil {
		t.Fatal("no trial")
	}
	h.awaitStarted(gated, r.runID, "test")
	if err := h.git.CASUpdate(context.Background(), "refs/heads/main", base, r.chainTip); err != nil {
		t.Fatal(err)
	}
	gated.Release(r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	c.Message = "Edited after landing"
	c.Version = "new"
	reviews.candidates = []core.Candidate{c}
	fresh := newIntegrationHarness(t, remote, executor.NewGatedExecutor(), target)
	fresh.d.cfg.Reviews = reviews
	fresh.reconcile()
	if len(reviews.candidates) != 0 || reviews.landed[c.Ref] != r.chainTip || remote.Ref("refs/heads/main") != r.chainTip {
		t.Fatal("metadata edit duplicated a landed review")
	}
	if records := fresh.ch.Records(); len(records) != 1 || !records[0].Recovered || len(records[0].Checks) != 0 {
		t.Fatalf("recovery records: %+v", records)
	}
}

func TestLinearDependencySelection(t *testing.T) {
	parent := core.Candidate{Ref: "a", Target: "main", SHA: "a-sha"}
	child := core.Candidate{Ref: "b", Target: "main", SHA: "b-sha", DependsOn: "a"}
	d := &Daemon{order: map[string]map[string]int64{"main": {"a": 2, "b": 1}}, done: map[string]map[string]parkEntry{"main": {}}}
	candidates := map[string]core.Candidate{"a": parent, "b": child}
	selected := d.pickUpTo("main", candidates, 2, nil)
	if len(selected) != 2 || selected[0] != parent || selected[1] != child {
		t.Fatalf("wrong dependency order: %v", selected)
	}
	d.done["main"]["a"] = parkEntry{SHA: parent.SHA, Outcome: core.OutcomeRejected}
	if got := d.pickUpTo("main", candidates, 2, nil); len(got) != 0 {
		t.Fatalf("parked parent bypassed: %v", got)
	}
	delete(d.done["main"], "a")
	delete(candidates, "a")
	if got := d.pickUpTo("main", candidates, 2, nil); len(got) != 0 {
		t.Fatalf("missing parent bypassed: %v", got)
	}
	if got := d.pickUpTo("main", candidates, 2, map[string]bool{"a": true}); len(got) != 1 || got[0] != child {
		t.Fatalf("predicted prerequisite not accepted: %v", got)
	}
}

func TestIntegration_GerritChangeIDStaysInFinalFooter(t *testing.T) {
	remote := testutil.NewRemote(t)
	remote.Seed("main", checkSpecFile("test"))
	base := remote.Ref("refs/heads/main")
	ref := remote.PushCandidate("main", "alice", "change", map[string]string{"a": "gerrit\n"})
	sha := remote.Ref(ref)
	remote.SetRef("refs/heads/source", sha)
	remote.DeleteCandidate(ref)
	id := "Change-Id: I" + strings.Repeat("a", 40)
	c := core.Candidate{Ref: "refs/heads/for/main/gerrit/change-0000000001", Target: "main", SHA: sha, Source: "gerrit", SourceBase: base, Message: "Fix\n\nBody\n\n" + id + "\n", Version: "review", ReviewURL: "https://review.example.com/c/project/+/1"}
	reviews := &integrationReviews{candidates: []core.Candidate{c}}
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, remote, gated, config.Target{Name: "main", Branch: "main", Landing: "squash"})
	h.d.cfg.Reviews = reviews
	h.reconcile()
	r := h.d.headRun("main")
	if r == nil {
		t.Fatal("no trial")
	}
	h.awaitStarted(gated, r.runID, "test")
	message, err := h.git.CommitMessage(context.Background(), r.chainTip)
	if err != nil {
		t.Fatal(err)
	}
	footer := message[strings.LastIndex(message, "\n\n")+2:]
	if !strings.HasPrefix(footer, id+"\n") || !strings.Contains(footer, "Gauntlet-Source: "+sha) {
		t.Fatalf("Gerrit Change-Id outside final footer: %s", message)
	}
	h.releaseGated(gated, r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
}

func TestIntegration_CancelWaitingReviewPreservesMetadata(t *testing.T) {
	remote := testutil.NewRemote(t)
	remote.Seed("main", checkSpecFile("test"))
	ref := remote.PushCandidate("main", "alice", "review", map[string]string{"a": "review\n"})
	source := remote.Ref(ref)
	remote.SetRef("refs/heads/source", source)
	remote.DeleteCandidate(ref)
	c := core.Candidate{Ref: "refs/heads/for/main/github/pr-0000000001", Target: "main", SHA: source, Source: "github", Message: "Title", Version: "review-metadata"}
	h := newIntegrationHarness(t, remote, executor.NewGatedExecutor(), config.Target{Name: "main", Branch: "main", Landing: "squash"})
	h.d.cfg.Reviews = &integrationReviews{candidates: []core.Candidate{c}}
	h.ch.SendCommand(core.Command{Kind: core.CommandCancel, Target: "main", Ref: c.Ref})
	h.reconcile()
	h.reconcile()
	if h.d.headRun("main") != nil {
		t.Fatal("cancelled waiting review started checks")
	}
	if got := h.d.done["main"][c.Ref]; got.Version != c.Version || got.SHA != source {
		t.Fatalf("cancel lost review identity: %+v", got)
	}
	records := h.ch.Records()
	if len(records) != 1 || records[0].Candidate != c {
		t.Fatalf("cancel record lost metadata: %+v", records)
	}
}
