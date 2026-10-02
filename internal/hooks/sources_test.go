package hooks

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

type sourceTrackingGit struct {
	fakeGitRepo
	leaseMu sync.Mutex
	users   map[string]int
}

func (g *sourceTrackingGit) RetainSources(_ context.Context, sources ...string) (func(), error) {
	g.leaseMu.Lock()
	if g.users == nil {
		g.users = make(map[string]int)
	}
	for _, source := range sources {
		g.users[source]++
	}
	g.leaseMu.Unlock()
	return sync.OnceFunc(func() {
		g.leaseMu.Lock()
		defer g.leaseMu.Unlock()
		for _, source := range sources {
			g.users[source]--
		}
	}), nil
}

func (g *sourceTrackingGit) count(source string) int {
	g.leaseMu.Lock()
	defer g.leaseMu.Unlock()
	return g.users[source]
}

func TestHookSourcesCoverRunningAndBackloggedLandings(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "coalesce", true: "shutdown"}[force], func(t *testing.T) {
			git := &sourceTrackingGit{}
			ex := &blockingExecutor{blockName: "hook:deploy", blockRun: "one", started: make(chan struct{}), release: make(chan struct{})}
			r := New(Params{Git: git, Exec: ex, Hooks: map[string][]Hook{"main": {{Name: "deploy", Command: []string{"true"}}}}, Policies: map[string]Policy{"main": PolicyCoalesce}, WorkDir: t.TempDir(), Log: io.Discard})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			emit := func(id string) {
				t.Helper()
				if err := r.Emit(ctx, landedEvent("main", &core.RunRecord{RunID: id, MergeSHA: "merge", Candidate: core.Candidate{SHA: id}})); err != nil {
					t.Fatal(err)
				}
			}
			emit("one")
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			select {
			case <-ex.started:
			case <-time.After(5 * time.Second):
				t.Fatal("hook did not start")
			}
			emit("two")
			emit("three")
			for _, id := range []string{"one", "two", "three"} {
				if git.count(id) != 1 {
					t.Fatalf("%s was not retained", id)
				}
			}
			if force {
				cancel()
			} else {
				close(ex.release)
				r.Drain(t.Context())
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("runner did not stop")
			}
			for _, id := range []string{"one", "two", "three"} {
				if git.count(id) != 0 {
					t.Fatalf("%s leaked source protection", id)
				}
			}
			if !force {
				ids := ex.runIDs()
				if len(ids) != 2 || ids[0] != "one" || ids[1] != "three" {
					t.Fatalf("hooks=%v", ids)
				}
			}
		})
	}
}

func TestDroppedHookSourcesAreReleased(t *testing.T) {
	git := &sourceTrackingGit{}
	r := New(Params{Git: git, Log: io.Discard})
	ev := landedEvent("main", &core.RunRecord{RunID: "one", Candidate: core.Candidate{SHA: "source"}})
	for range queueBuffer {
		if err := r.Emit(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Emit(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if git.count("source") != queueBuffer {
		t.Fatal("overflow leaked a lease")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if git.count("source") != 0 {
		t.Fatal("shutdown leaked queued leases")
	}
	if err := r.Emit(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	if git.count("source") != 0 {
		t.Fatal("stopped runner accepted a lease")
	}
}
