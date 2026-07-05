// Package slack implements a duplex core.Channel over Slack's socket-mode
// protocol (docs/plans/phase23.md §4.4): outbound, it threads one root
// message per run, threaded replies for each check, and edits the root to
// its final verdict; inbound, it turns a ":recycle:" reaction on an owned
// root message into a core.Command{Kind: core.CommandRetry}, and an ":x:"
// reaction into a core.Command{Kind: core.CommandCancel} (Feature 1, manual
// operator cancellation).
//
// It uses github.com/slack-go/slack + its socketmode subpackage: an
// app-level token opens the socket-mode WebSocket (via
// "apps.connections.open"), a bot token drives the Web API calls
// (chat.postMessage / chat.update). Both routes go through the same
// *slack.Client, so a single slack.OptionAPIURL(...) reroutes everything to
// a fake server in tests (§1 Spike B; verified against the slack-go v0.27.0
// source: apps.connections.open uses Client.endpoint exactly like every
// other Web API call, and socketmode.Client embeds a copy of that Client,
// so it inherits the same endpoint — no separate socket-mode URL override
// exists or is needed).
package slack

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	goslack "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/sgrankin/gauntlet/internal/core"
)

var _ core.Channel = (*Slack)(nil)

// Params configures a Slack channel. It is a package-local struct (§9.5):
// mapping from parsed config lives only in cmd, so this package never
// imports internal/config.
type Params struct {
	// Channel is the Slack channel ID to post into (e.g. "C0123456789").
	Channel string

	// AppToken is the app-level token ("xapp-...") used to open the
	// socket-mode connection (scope: connections:write).
	AppToken string

	// BotToken is the bot token ("xoxb-...") used for every Web API call
	// (scopes: chat:write, reactions:read).
	BotToken string

	// APIURL overrides the Slack API base URL. Test seam only: empty
	// means the real "https://slack.com/api/". A trailing "/" is added if
	// missing, matching slack-go's Client.endpoint+path concatenation.
	APIURL string

	// Log receives one line per dropped Emit/Command or Slack API error.
	// Defaults to os.Stderr when nil, matching LogChannel's spirit
	// (internal/channel/log.go).
	Log io.Writer
}

// Bounds on Slack's internal channels (§9.2: never leak, never block).
const (
	// outboxBuffer bounds Emit's internal queue. Emit never blocks the
	// reconcile loop: once this is full, further events are logged and
	// dropped rather than waiting for the drainer.
	outboxBuffer = 256

	// cmdsBuffer bounds the inbound Command queue, mirroring
	// channel.RecordingChannel's commandBuffer (internal/channel/record.go):
	// generous for a daemon that drains it every reconcile tick.
	cmdsBuffer = 64
)

// rootInfo identifies the (target, ref) a posted root message belongs to,
// so an owned reaction can be turned back into a retry Command.
type rootInfo struct {
	Target string
	Ref    string
}

// Slack is a duplex core.Channel implementing docs/plans/phase23.md §4.4.
// Emit only ever enqueues to outbox; the actual Slack calls happen on the
// drainer goroutine started by Run, so Emit itself never blocks and never
// fails the reconcile loop.
type Slack struct {
	channel string
	api     *goslack.Client
	smc     *socketmode.Client

	logMu sync.Mutex
	log   io.Writer

	outbox chan core.Event
	cmds   chan core.Command

	// mu guards the two run-tracking maps (§9.2 — bounded: every terminal
	// event deletes both entries for its run) and notify, a
	// closed-and-replaced-on-every-processed-event channel tests use to
	// synchronize without wall-clock sleeps, mirroring
	// channel.RecordingChannel's notify (internal/channel/record.go).
	mu      sync.Mutex
	runRoot map[string]string   // root-tracking key -> root message ts (batch-aware: BatchID when set, else RunID)
	roots   map[string]rootInfo // root message ts -> (target, ref)
	notify  chan struct{}

	// batchRecs buffers a batch's per-member terminal records, keyed by
	// BatchID, until a flush is triggered (docs/plans/phase5.md §3.3
	// addendum, hardened by the phase-5 review's F1): a batch posts ONE
	// final threaded summary listing every member, not one reply per member.
	// Each entry is deleted once that summary is posted, so — like
	// runRoot/roots — this never leaks an entry per finished batch (§9.2),
	// modulo the bounded staleness-sweep window documented on batchEntry.
	batchRecs map[string]*batchEntry

	// now returns the current time; overridden in tests (batchEntry's
	// staleness sweep, F1(c)) so the ~10-minute window can be exercised
	// without a real wall-clock wait. Defaults to time.Now.
	now func() time.Time
}

// batchEntry is one in-flight batch's buffered per-member terminal records
// (F1, the phase-5 review). recs is indexed by Position; Emit's outbox-full
// drop (§9.2 — "if the outbox is full, the event is logged and dropped") can
// silently lose ANY one member's terminal event, leaving a nil hole at that
// index — summarizeBatch tolerates that (F1a). arrived counts every member
// terminal event actually recorded so far, independent of which Position
// arrived: postBatchTerminal used to flush only when the record at
// Position == BatchSize-1 arrived, which both nil-dereferences on a hole
// before that point (a dropped middle member: summarizeBatch's own fix
// handles that) AND never fires at all if THAT particular event is the one
// dropped, leaking this entry (and runRoot/roots) forever (F1b — fixed by
// flushing on arrived == BatchSize as an alternative trigger). lastTouched
// is refreshed on every arrival and read by the staleness sweep
// (collectStaleBatchesLocked, F1c): an entry that never receives its
// flush-triggering arrival (because that, too, was dropped) is force-flushed
// with holes once it's older than batchStaleTimeout, so it can't buffer
// forever even in that doubly-unlucky case. The sweep only runs
// opportunistically, on some OTHER batch's terminal arrival (no new
// goroutine or timer) — so the residual worst case is "leaks until the next
// batch-terminal event for any batch," a bounded, documented trade-off, not
// an unbounded leak. target is captured once, from the first event's
// ev.Target, so finishBatch (which renders the root headline) has it
// available for every batch, including one flushed by the staleness sweep
// long after its own triggering event's ev.Target has gone out of scope.
type batchEntry struct {
	recs        []*core.RunRecord
	arrived     int
	lastTouched time.Time
	target      string
}

// batchStaleTimeout bounds how long a batch's buffered per-member records
// wait for a flush-triggering arrival before the staleness sweep
// (collectStaleBatchesLocked) force-flushes it anyway (F1c). Comfortably
// longer than any real check suite's per-batch window.
const batchStaleTimeout = 10 * time.Minute

// New returns a Slack channel configured by p. It performs no I/O; Run
// starts the socket-mode connection and the outbound drainer.
func New(p Params) *Slack {
	logw := p.Log
	if logw == nil {
		logw = os.Stderr
	}

	opts := []goslack.Option{goslack.OptionAppLevelToken(p.AppToken)}
	if apiURL := normalizeAPIURL(p.APIURL); apiURL != "" {
		opts = append(opts, goslack.OptionAPIURL(apiURL))
	}
	api := goslack.New(p.BotToken, opts...)
	smc := socketmode.New(api)

	return &Slack{
		channel:   p.Channel,
		api:       api,
		smc:       smc,
		log:       logw,
		outbox:    make(chan core.Event, outboxBuffer),
		cmds:      make(chan core.Command, cmdsBuffer),
		runRoot:   make(map[string]string),
		roots:     make(map[string]rootInfo),
		notify:    make(chan struct{}),
		batchRecs: make(map[string]*batchEntry),
		now:       time.Now,
	}
}

// normalizeAPIURL adds a trailing "/" when missing, matching slack-go's
// Client.endpoint+path concatenation (Client.endpoint is expected to end in
// "/", e.g. the real default "https://slack.com/api/"). Empty stays empty
// (meaning: use slack-go's own default).
func normalizeAPIURL(u string) string {
	if u == "" || strings.HasSuffix(u, "/") {
		return u
	}
	return u + "/"
}

// Emit enqueues ev for the drainer goroutine and returns immediately. It
// never blocks the reconcile loop and never fails it: if the outbox is
// full (the drainer isn't running, or Slack is slow), the event is logged
// and dropped. The error return exists only to satisfy core.Channel.
func (s *Slack) Emit(ctx context.Context, ev core.Event) error {
	select {
	case s.outbox <- ev:
	default:
		s.logf("slack: outbox full (%d), dropping event kind=%d run=%s", outboxBuffer, ev.Kind, ev.RunID)
	}
	return nil
}

// Commands returns the channel core.Command values (core.CommandRetry from
// an owned ":recycle:" reaction, core.CommandCancel from an owned ":x:"
// reaction) are delivered on.
func (s *Slack) Commands() <-chan core.Command {
	return s.cmds
}

// Stats reports the current size of the run-tracking maps. Exported for
// tests to assert the maps are bounded (docs/plans/phase23.md §9.2): both
// must return to zero after every terminal event, however long the daemon
// runs.
func (s *Slack) Stats() (runs, roots int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runRoot), len(s.roots)
}

// Run starts the socket-mode connection and the outbound drainer, and
// blocks until ctx is done. Both goroutines it starts observe ctx.Done()
// directly, so they exit promptly regardless of what the socket-mode
// client's own run loop is doing at the time.
func (s *Slack) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.drainOutbox(ctx)
	}()
	go func() {
		defer wg.Done()
		s.handleSocketEvents(ctx)
	}()

	err := s.smc.RunContext(ctx)

	wg.Wait()

	if ctx.Err() != nil {
		// Shutdown was requested; RunContext returning
		// context.Canceled (or anything else, mid-teardown) is expected,
		// not a failure to report.
		return nil
	}
	return err
}

// --- outbound ---------------------------------------------------------

// drainOutbox is the Run(ctx)-started goroutine that performs every actual
// Slack Web API call, so Emit itself never blocks (§4.4).
func (s *Slack) drainOutbox(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-s.outbox:
			s.handleOutbound(ctx, ev)
			s.signalProcessed()
		}
	}
}

// handleOutbound dispatches ev to the right Slack call. Terminal events
// (Record != nil) are handled uniformly regardless of Kind, matching the
// terminal-event contract documented on core.Event; EventQueued and any
// event kind this channel doesn't recognize are silently ignored, per
// core.Channel's "ignore unknown kinds" contract (internal/channel/log.go)
// and §4.4's "don't spam the channel" guidance for low-value events.
func (s *Slack) handleOutbound(ctx context.Context, ev core.Event) {
	switch {
	case ev.Record != nil:
		s.postTerminal(ctx, ev)
	case ev.Kind == core.EventTrialClean:
		s.postRoot(ctx, ev)
	case ev.Kind == core.EventCheckStarted, ev.Kind == core.EventCheckFinished:
		s.postCheckReply(ctx, ev)
	case ev.Kind == core.EventIgnoredRef:
		s.logf("slack: ignored ref target=%s ref=%s", ev.Target, ev.Candidate.Ref)
	case ev.Kind == core.EventHookFinished:
		s.postHookFinished(ctx, ev)
	}
}

// postRoot posts the root message for a newly clean trial and records
// runID->rootTS and rootTS->(target,ref) so later events for this run
// (check replies, the terminal edit) and an inbound reaction can find it.
//
// Idempotent per ev.RunID: a batch's chain-building fires one
// EventTrialClean per member (startBatchRun's onClean callback runs once for
// every candidate whose own trial-merge came back clean), all sharing the
// batch's one bare RunID — so without this guard, a 3-member batch would
// post 3 separate "⏳ testing..." root messages, each silently orphaning the
// previous one's roots[] entry (never forgotten, since forget only ever
// removes the CURRENT ts). One root per run, posted at the first
// EventTrialClean, keeps both non-batch behavior and the map's §9.2 bound
// unchanged.
func (s *Slack) postRoot(ctx context.Context, ev core.Event) {
	s.mu.Lock()
	if _, exists := s.runRoot[ev.RunID]; exists {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	text := fmt.Sprintf("⏳ testing %s (%s) → %s", ev.Candidate.Topic, displayUser(ev.Candidate.User), ev.Target)
	_, ts, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(text, false))
	if err != nil {
		s.logf("slack: chat.postMessage failed run=%s: %v", ev.RunID, err)
		return
	}

	s.mu.Lock()
	s.runRoot[ev.RunID] = ts
	s.roots[ts] = rootInfo{Target: ev.Target, Ref: ev.Candidate.Ref}
	s.mu.Unlock()
}

// postCheckReply posts a terse threaded reply for a check starting or
// finishing. A check-started event carries only the check's name (nothing
// else is known yet). A check-finished event now carries ev.Check (F-a,
// DESIGN.md "Full per-check log files") — the just-finished CheckResult —
// so its reply can show the verdict and duration immediately instead of
// waiting for the run's terminal threaded summary (postTerminal); ev.Check
// is nil-checked and falls back to the old name-only line for any event
// that (still) doesn't carry one.
func (s *Slack) postCheckReply(ctx context.Context, ev core.Event) {
	rootTS, ok := s.lookupRoot(ev.RunID)
	if !ok {
		return
	}

	var text string
	switch ev.Kind {
	case core.EventCheckStarted:
		text = fmt.Sprintf("▶️ %s", ev.CheckName)
	case core.EventCheckFinished:
		if ev.Check != nil {
			text = fmt.Sprintf("%s %s (%s)", checkEmoji(ev.Check.Status), ev.CheckName, ev.Check.Duration.Round(time.Millisecond))
		} else {
			text = fmt.Sprintf("◾ %s finished", ev.CheckName)
		}
	}

	if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(text, false), goslack.MsgOptionTS(rootTS)); err != nil {
		s.logf("slack: threaded reply failed run=%s check=%s: %v", ev.RunID, ev.CheckName, err)
	}
}

// postTerminal edits the root message to its final verdict, posts a final
// threaded summary, and then deletes both map entries for this run (§9.2 —
// a long-running daemon must not leak an entry per run).
//
// Batch-aware (docs/plans/phase5.md §3.3 addendum): a batch's per-member
// records now carry distinct RunIDs (queue's memberRunID fix for the
// history PRIMARY KEY collision), but the root was posted — and is tracked
// in runRoot/roots — under the batch's shared BatchID (== the bare RunID
// EventTrialClean carried at chain-build time, postRoot's tracking key).
// So the root lookup here joins on rec.BatchID when it's set, falling back
// to ev.RunID (== rec.RunID) otherwise — byte-identical to before for every
// non-batch event, whose BatchID is always "". A batch's terminal events
// then route to postBatchTerminal instead of editing/replying per member.
func (s *Slack) postTerminal(ctx context.Context, ev core.Event) {
	rec := ev.Record
	joinKey := ev.RunID
	if rec.BatchID != "" {
		joinKey = rec.BatchID
	}

	rootTS, ok := s.lookupRoot(joinKey)
	if !ok {
		// No known root: e.g. this channel started after the trial-clean
		// event, or the root post itself was dropped (outbox overflow).
		// Nothing to edit, nothing to clean up.
		return
	}

	if rec.BatchID != "" {
		s.postBatchTerminal(ctx, ev, rec, rootTS)
		return
	}

	headline := fmt.Sprintf("%s %s (%s) → %s", outcomeEmoji(rec.Outcome), ev.Candidate.Topic, displayUser(ev.Candidate.User), ev.Target)
	if _, _, _, err := s.api.UpdateMessageContext(ctx, s.channel, rootTS, goslack.MsgOptionText(headline, false)); err != nil {
		s.logf("slack: chat.update failed run=%s: %v", ev.RunID, err)
	}

	summary := summarizeRun(rec)
	if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(summary, false), goslack.MsgOptionTS(rootTS)); err != nil {
		s.logf("slack: final threaded summary failed run=%s: %v", ev.RunID, err)
	}

	s.forget(ev.RunID, rootTS)
}

// postBatchTerminal handles one member's terminal event within a batch
// (postTerminal's batch branch, docs/plans/phase5.md §3.3 addendum). Every
// member of a batch shares the same Outcome by construction — landRun/
// finishRun/finishBatchRed/rejectBatch all assign one outcome to the whole
// batch, never singling out a member's own verdict (a genuine per-member
// split doesn't exist: a batch either lands whole or Skips/parks whole).
//
// Rather than one noisy threaded reply per member, each member's record is
// buffered (keyed by BatchID, indexed by Position) in a batchEntry until a
// flush triggers (F1b): either every member has actually arrived
// (be.arrived == rec.BatchSize) or the nominal last member (Position ==
// BatchSize-1) has arrived — tracking arrived separately from "did the
// Position-(BatchSize-1) event show up" means a dropped LAST member (which
// would never satisfy the old Position-only check) still flushes once every
// OTHER member is in, and a dropped MIDDLE member still flushes at the real
// last position instead of blocking forever on a count a drop makes
// unreachable. The root's headline edit and the one threaded summary reply
// are both produced together at flush time (finishBatch), from whichever
// members have actually arrived by then (nil holes tolerated, F1a) — a
// live-Slack follow-up to F1: the headline used to update eagerly at
// Position 0's own arrival, using only that one member's info, which is
// exactly why a batch of one member rendered as "batch <runID> (1 members)
// → target" instead of the normal single-run phrasing, and a multi-member
// batch's headline never got to name any of its members. Deferring to
// flush time fixes both: batchHeadline (below) renders a size-1 batch
// identically to a serial run, and a size>1 batch's headline lists every
// member topic that's arrived by flush time. Once flushed, both
// run-tracking maps — plus this batch's buffered records — are cleaned up
// (§9.2). Every arrival also opportunistically sweeps for any OTHER batch
// stuck long enough to be stale (F1c) and flushes those too, so a batch
// whose own flush-triggering arrival was itself dropped doesn't buffer
// forever either.
func (s *Slack) postBatchTerminal(ctx context.Context, ev core.Event, rec *core.RunRecord, rootTS string) {
	s.mu.Lock()
	be := s.batchRecs[rec.BatchID]
	if be == nil {
		be = &batchEntry{recs: make([]*core.RunRecord, rec.BatchSize), target: ev.Target}
		s.batchRecs[rec.BatchID] = be
	}
	if rec.Position >= 0 && rec.Position < len(be.recs) {
		be.recs[rec.Position] = rec
	}
	be.arrived++
	be.lastTouched = s.now()
	flush := be.arrived >= rec.BatchSize || rec.Position == rec.BatchSize-1
	target := be.target
	recs := be.recs
	stale := s.collectStaleBatchesLocked(rec.BatchID)
	s.mu.Unlock()

	if flush {
		s.finishBatch(ctx, rec.BatchID, rootTS, target, recs)
	}
	for _, sb := range stale {
		s.finishBatch(ctx, sb.batchID, sb.rootTS, sb.target, sb.recs)
	}
}

// staleBatch is one collectStaleBatchesLocked result: a batch whose
// buffered records have sat past batchStaleTimeout without a
// flush-triggering arrival (F1c).
type staleBatch struct {
	batchID string
	rootTS  string // "" if no root is known (defensive; see collectStaleBatchesLocked)
	target  string
	recs    []*core.RunRecord
}

// collectStaleBatchesLocked scans s.batchRecs for every entry other than
// excludeBatchID (the one postBatchTerminal is already handling this call)
// whose lastTouched is older than batchStaleTimeout, returning each for the
// caller to flush-with-holes and forget once s.mu is released. Must be
// called with s.mu held.
func (s *Slack) collectStaleBatchesLocked(excludeBatchID string) []staleBatch {
	var stale []staleBatch
	now := s.now()
	for batchID, be := range s.batchRecs {
		if batchID == excludeBatchID || now.Sub(be.lastTouched) < batchStaleTimeout {
			continue
		}
		// rootTS should always be found here: postBatchTerminal only ever
		// creates a batchRecs entry after a successful lookupRoot, and
		// nothing deletes runRoot's entry for a batch out from under a live
		// batchRecs entry except this same flush path (which deletes both
		// together). The lookup is defensive, not load-bearing.
		rootTS := s.runRoot[batchID]
		stale = append(stale, staleBatch{batchID: batchID, rootTS: rootTS, target: be.target, recs: be.recs})
	}
	return stale
}

// finishBatch posts batchID's final root headline edit and threaded summary
// (both skipped if rootTS is unknown — nothing to edit or reply to) and
// forgets every trace of it: batchRecs, runRoot, and roots (§9.2).
func (s *Slack) finishBatch(ctx context.Context, batchID, rootTS, target string, recs []*core.RunRecord) {
	if rootTS != "" {
		headline := batchHeadline(target, recs)
		if _, _, _, err := s.api.UpdateMessageContext(ctx, s.channel, rootTS, goslack.MsgOptionText(headline, false)); err != nil {
			s.logf("slack: chat.update failed batch=%s: %v", batchID, err)
		}

		summary := summarizeBatch(recs)
		if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(summary, false), goslack.MsgOptionTS(rootTS)); err != nil {
			s.logf("slack: final batch summary failed batch=%s: %v", batchID, err)
		}
	}

	s.mu.Lock()
	delete(s.batchRecs, batchID)
	s.mu.Unlock()
	s.forget(batchID, rootTS)
}

// postHookFinished posts a standalone (non-threaded) channel message for a
// failed post-land hook (closing-review FIX 1). By hook time the run's root
// ts is already forgotten — postTerminal's forget call runs at landing time,
// before any hook has even started, and that cleanup is correct as-is — so
// there is no thread to reply on; this posts a fresh top-level message
// instead. A passed hook posts nothing here: the log channel already renders
// every hook result (pass and fail), and posting on every pass would spam
// the channel for no operational value.
func (s *Slack) postHookFinished(ctx context.Context, ev core.Event) {
	if ev.Check == nil || (ev.Check.Status != core.CheckFailed && ev.Check.Err == nil) {
		return
	}

	text := fmt.Sprintf("⚠ hook %s failed after landing %s (%s) → %s", ev.CheckName, ev.Candidate.Topic, displayUser(ev.Candidate.User), ev.Target)
	if tail := core.FailureTail(ev.Check, failureTailMaxLines, failureTailMaxBytes); tail != "" {
		text += fmt.Sprintf("\n```\n%s\n```", tail)
	}

	if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(text, false)); err != nil {
		s.logf("slack: hook-failed post failed run=%s hook=%s: %v", ev.RunID, ev.CheckName, err)
	}
}

// lookupRoot returns the root ts recorded for runID, if any.
func (s *Slack) lookupRoot(runID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.runRoot[runID]
	return ts, ok
}

// forget deletes both map entries for a finished run (§9.2).
func (s *Slack) forget(runID, rootTS string) {
	s.mu.Lock()
	delete(s.runRoot, runID)
	delete(s.roots, rootTS)
	s.mu.Unlock()
}

// signalProcessed wakes anything waiting on notify. It exists so tests can
// synchronize on "one outbound event has been fully handled" (including
// any map mutation) without a wall-clock sleep, mirroring
// channel.RecordingChannel's notify (internal/channel/record.go).
func (s *Slack) signalProcessed() {
	s.mu.Lock()
	old := s.notify
	s.notify = make(chan struct{})
	s.mu.Unlock()
	close(old)
}

// Caps on the failing-check tail included in the final threaded summary
// (DESIGN.md Watch: "Channels should include the failing check's output
// tail in terminal notifications"). Slack caps a message's text at 3000
// characters; these are deliberately tighter than LogChannel's (which has no
// hard ceiling) to leave headroom for the rest of summarizeRun's text (the
// headline, per-check lines, run ID, Detail) plus the code-block fencing.
const (
	failureTailMaxLines = 20
	failureTailMaxBytes = 2500
)

// summarizeRun renders rec as a final threaded summary: outcome, one line
// per check (verdict + duration), the run ID, Detail when present, and — for
// a run with a failing check — that check's output tail in a code block.
func summarizeRun(rec *core.RunRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — run %s", outcomeLabel(rec.Outcome), rec.RunID)
	for _, c := range rec.Checks {
		fmt.Fprintf(&b, "\n%s %s (%s)", checkEmoji(c.Status), c.Name, c.Duration.Round(time.Millisecond))
	}
	if rec.Detail != "" {
		fmt.Fprintf(&b, "\n%s", rec.Detail)
	}
	if res := rec.FirstFailure(); res != nil {
		if tail := core.FailureTail(res, failureTailMaxLines, failureTailMaxBytes); tail != "" {
			fmt.Fprintf(&b, "\n```\n%s\n```", tail)
		}
	}
	return b.String()
}

// firstBatchRec returns the first non-nil record in recs, or nil if every
// slot is a hole (F1a, the phase-5 review: Emit's outbox-full drop, §9.2,
// can lose any one member's terminal event). Shared by summarizeBatch and
// batchHeadline — both need "a representative member" for the fields every
// member of a batch shares by construction (Outcome, Checks; §3.3).
func firstBatchRec(recs []*core.RunRecord) *core.RunRecord {
	for _, r := range recs {
		if r != nil {
			return r
		}
	}
	return nil
}

// batchHeadline renders the root message's final-verdict chat.update text
// for a batch (F1's live-Slack follow-up, the phase-5 review): a batch
// formed with exactly one member — max-batch 1, or a queue that only ever
// offered one candidate; §4.1 promises this "degrades to serial behavior"
// byte for byte — now renders IDENTICALLY to a serial run's own headline
// (postTerminal's, below), not "batch <runID> (1 members) → target" (the
// live bug report: broken grammar, and it drops the topic/user entirely). A
// genuine multi-member batch instead says "batch of N", naming whichever
// member topics have actually arrived by flush time (recs may have nil
// holes — F1a; "reasonable" effort, not a guarantee of completeness, since a
// dropped middle member's own topic is simply unknown here). The doubly
// unlucky all-holes case (every member's event dropped — only reachable via
// the staleness sweep, since a flush trigger requires at least one arrival
// recorded in-bounds) renders one uniform "(all events dropped)" line with
// a ⚠️ placeholder, deliberately NOT guessing an outcome emoji: the real
// outcome is simply unknown here.
func batchHeadline(target string, recs []*core.RunRecord) string {
	head := firstBatchRec(recs)
	if head == nil {
		return fmt.Sprintf("⚠️ batch of %d (all events dropped) → %s", len(recs), target)
	}
	if len(recs) == 1 {
		return fmt.Sprintf("%s %s (%s) → %s", outcomeEmoji(head.Outcome), head.Candidate.Topic, displayUser(head.Candidate.User), target)
	}
	var topics []string
	for _, r := range recs {
		if r != nil {
			topics = append(topics, r.Candidate.Topic)
		}
	}
	return fmt.Sprintf("%s batch of %d (%s) → %s", outcomeEmoji(head.Outcome), len(recs), strings.Join(topics, ", "), target)
}

// summarizeBatch renders the ONE final threaded reply for a whole batch
// (§3.3 addendum: "one reply per member is noisy"). A batch of exactly one
// member renders identically to a serial run's own summary — the same
// summarizeRun, on the same RunRecord shape (F1's live-Slack follow-up:
// §4.1's "degrades to serial behavior" byte for byte, extended to Slack's
// rendering). A genuine multi-member batch's checks are duplicated onto
// every member's record (§3.3's own documented shape — the suite ran once,
// against the chain tip), so the outcome label and check-verdict lines
// render once, from the first non-nil member found (head, firstBatchRec —
// recs is indexed by Position, but a slot may be nil, F1a); each present
// member then gets its own line naming it and its own (now-distinct,
// post-fix) RunID, plus its own Detail when non-empty — landRun's per-member
// slot-delete CAS can genuinely differ member to member (a stale delete vs.
// a clean one), even though Outcome itself never does. Any nil hole is
// counted and surfaced as one trailing "…and K member(s) whose events were
// dropped" line rather than silently omitted.
func summarizeBatch(recs []*core.RunRecord) string {
	if len(recs) == 1 {
		if recs[0] == nil {
			return "batch summary unavailable: the only member's event was dropped"
		}
		return summarizeRun(recs[0])
	}

	head := firstBatchRec(recs)
	if head == nil {
		// Every member's event was dropped: nothing to render a headline or
		// check list from. Still worth a message — a silent batch that
		// never gets a summary at all is worse than one that says so.
		return fmt.Sprintf("batch summary unavailable: all %d member event(s) were dropped", len(recs))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s — batch of %d (%s)", outcomeLabel(head.Outcome), len(recs), head.BatchID)
	for _, c := range head.Checks {
		fmt.Fprintf(&b, "\n%s %s (%s)", checkEmoji(c.Status), c.Name, c.Duration.Round(time.Millisecond))
	}
	if res := head.FirstFailure(); res != nil {
		if tail := core.FailureTail(res, failureTailMaxLines, failureTailMaxBytes); tail != "" {
			fmt.Fprintf(&b, "\n```\n%s\n```", tail)
		}
	}
	var holes int
	for _, r := range recs {
		if r == nil {
			holes++
			continue
		}
		fmt.Fprintf(&b, "\n%d. %s (%s) — run %s", r.Position, r.Candidate.Topic, displayUser(r.Candidate.User), r.RunID)
		if r.Detail != "" {
			fmt.Fprintf(&b, ": %s", r.Detail)
		}
	}
	if holes > 0 {
		fmt.Fprintf(&b, "\n…and %d member(s) whose events were dropped", holes)
	}
	return b.String()
}

// displayUser renders a solo-setup empty Candidate.User (core.Candidate's
// doc: "User may be \"\" for solo setups") as something other than an
// empty pair of parens.
func displayUser(user string) string {
	if user == "" {
		return "solo"
	}
	return user
}

// outcomeEmoji maps a run's outcome to the root-message glyph (§4.4:
// "chat.update the root to ✅/❌/⚠").
func outcomeEmoji(o core.Outcome) string {
	switch o {
	case core.OutcomeLanded:
		return "✅"
	case core.OutcomeSkipped:
		return "⚠️"
	default: // OutcomeRejected, OutcomeConflict, OutcomeError
		return "❌"
	}
}

// outcomeLabel renders o for the threaded summary line, matching
// channel/log.go's outcomeString vocabulary.
func outcomeLabel(o core.Outcome) string {
	switch o {
	case core.OutcomeLanded:
		return "landed"
	case core.OutcomeRejected:
		return "rejected"
	case core.OutcomeConflict:
		return "conflict"
	case core.OutcomeSkipped:
		return "skipped"
	case core.OutcomeError:
		return "error"
	default:
		return fmt.Sprintf("unknown(%d)", int(o))
	}
}

// checkEmoji maps a check's status to the threaded-summary glyph.
func checkEmoji(s core.CheckStatus) string {
	switch s {
	case core.CheckPassed:
		return "✓"
	case core.CheckSkipped:
		return "⊘"
	default: // CheckFailed
		return "✗"
	}
}

// logf writes a log-and-drop line. Like LogChannel.Emit and
// ghstatus.Channel.logf, a write failure here is not itself reported
// anywhere: losing a diagnostic line must never do anything worse than
// lose a diagnostic line.
func (s *Slack) logf(format string, args ...any) {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	fmt.Fprintf(s.log, format+"\n", args...)
}

// --- inbound ------------------------------------------------------------

// handleSocketEvents is the Run(ctx)-started goroutine that reads
// socket-mode events (only reaction_added matters here) and turns an
// owned ":recycle:" into a core.Command.
func (s *Slack) handleSocketEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-s.smc.Events:
			if !ok {
				return
			}
			s.handleSocketEvent(evt)
		}
	}
}

func (s *Slack) handleSocketEvent(evt socketmode.Event) {
	if evt.Type != socketmode.EventTypeEventsAPI {
		// Connecting/connected/hello/disconnect/error events, interactive
		// callbacks, slash commands: none of them are this channel's
		// concern.
		return
	}

	apiEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}

	// Every events_api envelope must be acknowledged regardless of what it
	// contains, or Slack will retry delivery.
	if evt.Request != nil {
		if err := s.smc.Ack(*evt.Request); err != nil {
			s.logf("slack: ack failed: %v", err)
		}
	}

	if apiEvent.Type != slackevents.CallbackEvent {
		return
	}
	reaction, ok := apiEvent.InnerEvent.Data.(*slackevents.ReactionAddedEvent)
	if !ok {
		return
	}
	// ":recycle:" -> retry (re-queue the same SHA); ":x:" -> cancel (park it
	// and stop whatever is currently in flight for it). Every other reaction
	// (and every other inner event type) is ignored.
	var kind string
	switch reaction.Reaction {
	case "recycle":
		kind = core.CommandRetry
	case "x":
		kind = core.CommandCancel
	default:
		return
	}

	s.mu.Lock()
	info, owned := s.roots[reaction.Item.Timestamp]
	s.mu.Unlock()
	if !owned {
		// Reaction on a timestamp we didn't post (someone else's message,
		// or a root we've already forgotten): ignored.
		return
	}

	cmd := core.Command{Kind: kind, Target: info.Target, Ref: info.Ref}
	select {
	case s.cmds <- cmd:
	default:
		s.logf("slack: cmds buffer full (%d), dropping %s target=%s ref=%s", cmdsBuffer, kind, info.Target, info.Ref)
	}
}
