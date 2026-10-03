package slack

// Deploy-lane posting (docs/architecture/deployment.md, "Surfaces"): one ROOT
// message per graph run, node failures threaded under it, the root edited to
// its verdict when the run concludes. Structurally the candidate-run shape
// (postFreshRoot/postCheckReply/postTerminal) — decided over a
// per-environment thread precisely because it mirrors how a run posts today
// — but with two deliberate differences that are the whole reason this lives
// in its own file rather than being folded into those functions:
//
//  1. A deploy root is tracked in its OWN map (deployRoot), never in roots.
//     roots is the reaction-ownership index: an entry there says "a reaction
//     on this message means a core.Command for (target, ref)". A deploy lane
//     has neither a target nor a ref — that is exactly why deploy retry and
//     cancel are env-addressed API/MCP/dashboard calls rather than Commands
//     — so an entry there could only ever mint a command for the wrong
//     thing.
//  2. A deploy root carries NO gauntlet_run message metadata. That metadata
//     is the DURABLE half of the same ownership scheme: it is what lets a
//     reaction arriving after the in-memory entry is forgotten still resolve
//     to (target, ref) via handleForeignReaction. Attaching it to a deploy
//     root — with what payload? — would be inventing an ownership claim the
//     command vocabulary cannot honor.
//
// Together those two make reactions on a deploy root FAIL CLOSED: the
// in-memory lookup misses, the metadata fetch finds no gauntlet_run payload,
// and the reaction is ignored. "No Slack reaction commands in v1" is
// therefore a structural property here, not a rule someone has to remember
// — and it is tested as one.

import (
	"context"
	"fmt"
	"strings"
	"time"

	goslack "github.com/slack-go/slack"

	"github.com/sgrankin/gauntlet/internal/core"
)

// postDeployStarted posts the root message for one environment's graph run
// and records runID -> ts in deployRoot so this run's later events (node
// failures, the terminal edit) can find it.
//
// Idempotent per RunID, mirroring postRoot: a duplicated started event must
// not orphan the first root's tracking entry.
func (s *Slack) postDeployStarted(ctx context.Context, ev core.Event) {
	s.mu.Lock()
	_, exists := s.deployRoot[ev.RunID]
	s.mu.Unlock()
	if exists {
		return
	}

	text := fmt.Sprintf("⏳ deploying %s %s", ev.DeployEnv, shortSHA(ev.DeploySHA))
	if ev.DeployedSHA != "" {
		text += fmt.Sprintf(" from %s", shortSHA(ev.DeployedSHA))
	}
	// No MsgOptionMetadata: see this file's header comment — a deploy root
	// deliberately carries no ownership claim.
	_, ts, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(text, false))
	if err != nil {
		s.logf("slack: deploy root post failed run=%s env=%s: %v", ev.RunID, ev.DeployEnv, err)
		return
	}

	s.mu.Lock()
	s.deployRoot[ev.RunID] = ts
	s.mu.Unlock()
}

// postDeployNodeFinished threads one node's failure under its run's root.
//
// GREEN NODES POST NOTHING, deliberately — unlike a candidate run, whose
// per-check replies are the live progress view someone is watching. A
// tracked environment deploys on every landing, so a reply per node would be
// (nodes × environments) messages per merge for outcomes nobody acts on; the
// dashboard's /deploys page is the live view, and Slack's job here is the
// exceptions. Skipped counts green (core.NodeGreen), which is the whole
// point of the diff-based skip protocol: a ten-app environment that skips
// nine is a quiet success, not nine notifications.
func (s *Slack) postDeployNodeFinished(ctx context.Context, ev core.Event) {
	if ev.Check == nil || core.NodeGreen(*ev.Check) {
		return
	}
	rootTS, ok := s.lookupDeployRoot(ev.RunID)
	if !ok {
		// No root: this channel started mid-run, or the root post was
		// dropped (outbox overflow). Nothing to thread under.
		return
	}

	text := fmt.Sprintf("%s %s (%s)", checkEmoji(ev.Check.Status), ev.CheckName, ev.Check.Duration.Round(time.Millisecond))
	if ev.Check.Err != nil {
		text += fmt.Sprintf(" — %v", ev.Check.Err)
	}
	if tail := core.FailureTail(ev.Check, failureTailMaxLines, failureTailMaxBytes); tail != "" {
		text += fmt.Sprintf("\n```\n%s\n```", tail)
	}
	if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(text, false), goslack.MsgOptionTS(rootTS)); err != nil {
		s.logf("slack: deploy node reply failed run=%s node=%s: %v", ev.RunID, ev.CheckName, err)
	}
}

// postDeployFinished concludes a graph run: edit the root to its verdict,
// post the final threaded summary, forget the run.
//
// The no-root case is NOT a drop, unlike postTerminal's: a deploy run can
// conclude without ever having started one. A spec rejection (the revision
// declares no deploy nodes, or the environment names a node that doesn't
// exist in it) parks the lane before any node runs and emits no started
// event at all — and that park is exactly the fact an operator most needs
// told. So a rootless terminal posts one standalone channel message rather
// than going silent.
func (s *Slack) postDeployFinished(ctx context.Context, ev core.Event) {
	rec := ev.Deploy
	if rec == nil {
		return
	}

	headline := fmt.Sprintf("%s deploy %s %s", deployOutcomeEmoji(rec.Outcome), rec.Env, shortSHA(rec.DeploySHA))
	if rec.Culprit != "" {
		headline += fmt.Sprintf(" — %s failed", rec.Culprit)
	}

	rootTS, ok := s.lookupDeployRoot(ev.RunID)
	if !ok {
		if rec.Detail != "" {
			headline += fmt.Sprintf(" (%s)", rec.Detail)
		}
		if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(headline, false)); err != nil {
			s.logf("slack: deploy standalone terminal post failed run=%s env=%s: %v", ev.RunID, rec.Env, err)
		}
		return
	}

	if _, _, _, err := s.api.UpdateMessageContext(ctx, s.channel, rootTS, goslack.MsgOptionText(headline, false)); err != nil {
		s.logf("slack: deploy chat.update failed run=%s: %v", ev.RunID, err)
	}
	if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(summarizeDeploy(rec), false), goslack.MsgOptionTS(rootTS)); err != nil {
		s.logf("slack: deploy summary failed run=%s: %v", ev.RunID, err)
	}
	s.forgetDeploy(ev.RunID)
}

// lookupDeployRoot returns the root ts recorded for a deploy run, if any.
func (s *Slack) lookupDeployRoot(runID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.deployRoot[runID]
	return ts, ok
}

// forgetDeploy drops a concluded run's tracking entry — one map, not two,
// since a deploy root never claimed reaction ownership to release.
func (s *Slack) forgetDeploy(runID string) {
	s.mu.Lock()
	delete(s.deployRoot, runID)
	s.mu.Unlock()
}

// summarizeDeploy renders rec as the final threaded summary: the revisions
// the lane moved between, one line per declared node (blocked rows
// included — "which half of the environment is deployed" is the question a
// half-failed graph raises), the run ID, and the culprit's output tail.
func summarizeDeploy(rec *core.DeployRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — deploy %s", deployOutcomeLabel(rec.Outcome), rec.RunID)
	if rec.DeployedSHA != "" {
		fmt.Fprintf(&b, "\n%s → %s", shortSHA(rec.DeployedSHA), shortSHA(rec.DeploySHA))
	} else {
		fmt.Fprintf(&b, "\n%s (first deploy)", shortSHA(rec.DeploySHA))
	}
	for _, n := range rec.Nodes {
		fmt.Fprintf(&b, "\n%s %s (%s)", checkEmoji(n.Status), n.Name, n.Duration.Round(time.Millisecond))
	}
	if rec.Detail != "" {
		fmt.Fprintf(&b, "\n%s", rec.Detail)
	}
	if tail := core.FailureTail(firstFailingNode(rec), failureTailMaxLines, failureTailMaxBytes); tail != "" {
		fmt.Fprintf(&b, "\n```\n%s\n```", tail)
	}
	return b.String()
}

// shortSHA abbreviates an OID for display: 12 characters, the same length
// the dashboard's own shortSHA uses, which is enough to disambiguate in
// practice while fitting a one-line Slack headline.
func shortSHA(sha string) string {
	const n = 12
	if len(sha) > n {
		return sha[:n]
	}
	return sha
}

// firstFailingNode is core.RunRecord.FirstFailure's shape for a
// DeployRecord — the node whose output tail a terminal summary should carry.
func firstFailingNode(rec *core.DeployRecord) *core.CheckResult {
	for i := range rec.Nodes {
		n := &rec.Nodes[i]
		if n.Status == core.CheckFailed || n.Err != nil {
			return n
		}
	}
	return nil
}

// deployOutcomeEmoji/deployOutcomeLabel read the run outcome vocabulary in
// DEPLOY words: a green graph "deployed" (it did not "land" anything), a red
// or errored one "parked" (the lane holds at that revision until a retry or
// a new desired SHA — the operative fact, not the verdict word). Kept
// separate from outcomeEmoji/outcomeLabel rather than reusing them, since
// the same core.Outcome genuinely means a different thing on this side.
func deployOutcomeEmoji(o core.Outcome) string {
	switch o {
	case core.OutcomeLanded:
		return "✅"
	case core.OutcomeRejected:
		return "❌"
	case core.OutcomeError:
		return "🔥"
	case core.OutcomeSkipped:
		return "⏹"
	default:
		return "◾"
	}
}

func deployOutcomeLabel(o core.Outcome) string {
	switch o {
	case core.OutcomeLanded:
		return "deployed"
	case core.OutcomeRejected:
		return "parked (a node failed)"
	case core.OutcomeError:
		return "parked (daemon-side error)"
	case core.OutcomeSkipped:
		return "cancelled"
	default:
		return "finished"
	}
}
