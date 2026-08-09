package slack

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// deployStarted/deployNodeFinished/deployFinished build the three deploy
// events in the shape core.ValidateEvent demands, so these tests exercise
// the same event values internal/deploy actually emits.
func deployStarted(runID, env, sha, prev string) core.Event {
	return core.Event{
		Kind: core.EventDeployStarted, At: time.Now(), RunID: runID,
		DeployEnv: env, DeploySHA: sha, DeployedSHA: prev,
	}
}

func deployNodeFinished(runID, env, sha, node string, res core.CheckResult) core.Event {
	res.Name = node
	return core.Event{
		Kind: core.EventDeployNodeFinished, At: time.Now(), RunID: runID,
		DeployEnv: env, DeploySHA: sha, CheckName: node, Check: &res,
	}
}

func deployFinished(rec *core.DeployRecord) core.Event {
	return core.Event{
		Kind: core.EventDeployFinished, At: time.Now(), RunID: rec.RunID,
		DeployEnv: rec.Env, DeploySHA: rec.DeploySHA, DeployedSHA: rec.DeployedSHA,
		Deploy: rec,
	}
}

// TestSlack_DeployLifecycle walks the whole lane posting shape: a root per
// graph run, the root edited to its verdict at the end, and a final threaded
// summary under it.
func TestSlack_DeployLifecycle(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)

	mustEmit(t, s, ctx, deployStarted("deploy-1", "prod", "e5f6a7b8c9d0e1f2", "9c0d1e2f3a4b5c6d"))
	posts := fake.waitForPosts(1, testTimeout)
	root := posts[0]
	if root.method != "chat.postMessage" || root.threadTS != "" {
		t.Fatalf("root = %+v, want a top-level chat.postMessage", root)
	}
	for _, want := range []string{"deploying", "prod", "e5f6a7b8c9d0", "from 9c0d1e2f3a4b"} {
		if !strings.Contains(root.text, want) {
			t.Errorf("root text %q missing %q", root.text, want)
		}
	}

	started := time.Now().Add(-90 * time.Second)
	rec := &core.DeployRecord{
		RunID: "deploy-1", Env: "prod",
		DeploySHA: "e5f6a7b8c9d0e1f2", DeployedSHA: "9c0d1e2f3a4b5c6d",
		Nodes: []core.CheckResult{
			{Name: "migrate", Status: core.CheckPassed, Duration: 22 * time.Second},
			{Name: "app1", Status: core.CheckSkipped, Duration: time.Second},
		},
		Outcome: core.OutcomeLanded, StartedAt: started, EndedAt: started.Add(23 * time.Second),
	}
	mustEmit(t, s, ctx, deployFinished(rec))
	posts = fake.waitForPosts(3, testTimeout)

	edit := posts[1]
	if edit.method != "chat.update" || edit.ts != root.ts {
		t.Fatalf("second call = %+v, want a chat.update of the root %s", edit, root.ts)
	}
	if !strings.Contains(edit.text, "prod") || !strings.Contains(edit.text, "e5f6a7b8c9d0") {
		t.Errorf("edited root text = %q, want the lane and revision", edit.text)
	}

	summary := posts[2]
	if summary.threadTS != root.ts {
		t.Fatalf("summary = %+v, want it threaded under %s", summary, root.ts)
	}
	for _, want := range []string{"deployed", "deploy-1", "migrate", "app1", "9c0d1e2f3a4b → e5f6a7b8c9d0"} {
		if !strings.Contains(summary.text, want) {
			t.Errorf("summary %q missing %q", summary.text, want)
		}
	}

	// The run's tracking entry is released at terminal — a long-running
	// daemon must not leak one per deploy.
	waitForDeployRoots(t, s, testTimeout, 0)
}

// TestSlack_DeployNodeReplies: only NON-GREEN nodes are threaded, and a
// failing one carries its output tail. Skipped counts green (the skip
// protocol's whole point), so a quiet ten-app environment stays quiet.
func TestSlack_DeployNodeReplies(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	mustEmit(t, s, ctx, deployStarted("deploy-2", "prod2", "e5f6a7b8c9d0", ""))
	root := fake.waitForPosts(1, testTimeout)[0]
	if strings.Contains(root.text, "from ") {
		t.Errorf("root text %q names a previous revision on a first-ever deploy", root.text)
	}

	// Green and skipped nodes post nothing at all. Asserted by ORDER rather
	// than by a timed absence check: the drainer handles the outbox in
	// order, so if either of these had posted it would occupy the second
	// slot the red node's reply is asserted to hold below.
	mustEmit(t, s, ctx, deployNodeFinished("deploy-2", "prod2", "e5f6a7b8c9d0", "app1",
		core.CheckResult{Status: core.CheckPassed, Duration: time.Second, Output: "fine"}))
	mustEmit(t, s, ctx, deployNodeFinished("deploy-2", "prod2", "e5f6a7b8c9d0", "app2",
		core.CheckResult{Status: core.CheckSkipped, Duration: time.Second, Output: "no diff"}))

	// A red node threads under the root, with its tail.
	mustEmit(t, s, ctx, deployNodeFinished("deploy-2", "prod2", "e5f6a7b8c9d0", "migrate",
		core.CheckResult{Status: core.CheckFailed, Duration: 24 * time.Second, Output: "shard 1 ok\nlock wait timeout"}))
	reply := fake.waitForPosts(2, testTimeout)[1]
	if !strings.Contains(reply.text, "migrate") {
		t.Fatalf("second post = %q, want the red node's reply — a green or skipped node posted", reply.text)
	}
	if reply.threadTS != root.ts {
		t.Fatalf("node reply = %+v, want it threaded under %s", reply, root.ts)
	}
	for _, want := range []string{"migrate", "lock wait timeout", "```"} {
		if !strings.Contains(reply.text, want) {
			t.Errorf("node reply %q missing %q", reply.text, want)
		}
	}

	// An errored node (daemon-side, no verdict) threads too — Err is a
	// failure even when Status isn't CheckFailed.
	mustEmit(t, s, ctx, deployNodeFinished("deploy-2", "prod2", "e5f6a7b8c9d0", "app3",
		core.CheckResult{Status: core.CheckPassed, Err: errors.New("executor unreachable")}))
	errReply := fake.waitForPosts(3, testTimeout)[2]
	if !strings.Contains(errReply.text, "executor unreachable") {
		t.Errorf("errored node reply = %q, want it to name the error", errReply.text)
	}
}

// TestSlack_DeployTerminalWithoutRootPostsStandalone: a spec rejection parks
// the lane before any node runs, so no started event was ever emitted and
// there is no root to edit. That terminal must NOT be dropped the way an
// unrooted candidate-run terminal is — the park is the whole message.
func TestSlack_DeployTerminalWithoutRootPostsStandalone(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	now := time.Now()
	rec := &core.DeployRecord{
		RunID: "deploy-reject", Env: "prod2", DeploySHA: "e5f6a7b8c9d0",
		Outcome: core.OutcomeRejected, Detail: "revision declares no deploy nodes",
		StartedAt: now, EndedAt: now,
	}
	mustEmit(t, s, ctx, deployFinished(rec))

	posts := fake.waitForPosts(1, testTimeout)
	if posts[0].method != "chat.postMessage" || posts[0].threadTS != "" {
		t.Fatalf("post = %+v, want one standalone top-level message", posts[0])
	}
	for _, want := range []string{"prod2", "e5f6a7b8c9d0", "revision declares no deploy nodes"} {
		if !strings.Contains(posts[0].text, want) {
			t.Errorf("standalone terminal %q missing %q", posts[0].text, want)
		}
	}
}

// TestSlack_DeployRootFailsClosedForReactions is the binding v1 rule made
// structural: a reaction on a deploy root mints NOTHING. A deploy lane has
// no (target, ref) for a core.Command to name, so the root is deliberately
// absent from the reaction-ownership map AND carries no gauntlet_run
// metadata — the in-memory lookup misses and the metadata fetch finds no
// ownership claim, in that order. If either half were ever added "for
// symmetry", this test is what fails.
func TestSlack_DeployRootFailsClosedForReactions(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	mustEmit(t, s, ctx, deployStarted("deploy-3", "prod", "e5f6a7b8c9d0", ""))
	root := fake.waitForPosts(1, testTimeout)[0]

	if root.metadataSet {
		t.Errorf("deploy root carried message metadata (%q/%v); a deploy root must claim no ownership", root.eventType, root.payload)
	}
	// The tracking entry lands after the post's response is processed, so
	// wait for it before reading either map.
	waitForDeployRoots(t, s, testTimeout, 1)
	s.mu.Lock()
	_, inRoots := s.roots[root.ts]
	s.mu.Unlock()
	if inRoots {
		t.Error("deploy root is in the reaction-ownership map; it must only ever be in deployRoot")
	}

	conn := fake.waitForConn(testTimeout)
	fake.sendReaction(conn, "U1", "recycle", root.ts)
	select {
	case cmd, ok := <-s.Commands():
		t.Fatalf("a reaction on a deploy root minted %+v (ok=%v); deploy commands are env-addressed API calls, never reactions", cmd, ok)
	case <-time.After(300 * time.Millisecond):
		// expected: nothing arrived
	}
	if got := fake.snapshotReactions(); len(got) != 0 {
		t.Fatalf("a reaction on a deploy root was acknowledged (%+v); it must be ignored silently", got)
	}
}

// TestSlack_DeployStartedIsIdempotent: a duplicated started event must not
// post a second root and orphan the first one's tracking entry — the same
// guard postRoot has for a batch's repeated trial-cleans.
func TestSlack_DeployStartedIsIdempotent(t *testing.T) {
	s, fake, ctx := newTestSlack(t, nil)
	ev := deployStarted("deploy-4", "dev", "e5f6a7b8c9d0", "")
	mustEmit(t, s, ctx, ev)
	fake.waitForPosts(1, testTimeout)
	waitForDeployRoots(t, s, testTimeout, 1)

	// The duplicate, then a DIFFERENT lane's root: the outbox drains in
	// order, so the second recorded post is the second lane's root iff the
	// duplicate posted nothing.
	mustEmit(t, s, ctx, ev)
	mustEmit(t, s, ctx, deployStarted("deploy-5", "prod", "9c0d1e2f3a4b", ""))
	posts := fake.waitForPosts(2, testTimeout)
	if !strings.Contains(posts[1].text, "prod") {
		t.Fatalf("second post = %q, want the prod root — the duplicate started event posted a second dev root", posts[1].text)
	}
	waitForDeployRoots(t, s, testTimeout, 2)
}

// waitForDeployRoots blocks until deployRoot holds want entries,
// synchronizing on s.notify exactly as waitForStats does.
func waitForDeployRoots(t *testing.T, s *Slack, timeout time.Duration, want int) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		got := len(s.deployRoot)
		wake := s.notify
		s.mu.Unlock()
		if got == want {
			return
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("deployRoot entries = %d, want %d", got, want)
		}
	}
}
