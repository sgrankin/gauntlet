// Package slack implements a duplex core.Channel over Slack's socket-mode
// protocol (docs/plans/phase23.md §4.4): outbound, it threads one root
// message per run, threaded replies for each check, and edits the root to
// its final verdict; inbound, it turns a ":recycle:" reaction on an owned
// root message into a core.Command{Kind: core.CommandRetry}, and an ":x:"
// reaction into a core.Command{Kind: core.CommandCancel} (Feature 1, manual
// operator cancellation).
//
// Ownership of a root message is durable across process restarts and across
// the in-memory roots map's own cleanup (§9.2 forgets both run-tracking map
// entries the instant a run terminates — and a human reacting to a ❌ root
// overwhelmingly happens AFTER that point, not during the run, which is the
// only case the in-memory map can answer). Every root post attaches Slack
// message metadata (event_type "gauntlet_run") carrying {target, ref} for a
// single-run root, or {target} only for a batch root — deliberately omitting
// ref for a batch, since a reaction on a batch root cannot name which member
// it means (see handleForeignReaction/ackBatchGuidance). handleSocketEvent's
// reaction handling tries the in-memory roots map first (the still-running
// case, unchanged), and on a miss falls back to fetching the reacted message
// (conversations.history, with metadata) and verifying both bot authorship
// (via the bot's own user id, fetched once from auth.test at Run start) and
// the event_type before trusting its payload — never trusting a foreign
// message's reaction, since anyone can react to anyone's message.
//
// It uses github.com/slack-go/slack + its socketmode subpackage: an
// app-level token opens the socket-mode WebSocket (via
// "apps.connections.open"), a bot token drives the Web API calls
// (chat.postMessage / chat.update / conversations.history / reactions.add /
// auth.test). Both routes go through the same *slack.Client, so a single
// slack.OptionAPIURL(...) reroutes everything to a fake server in tests (§1
// Spike B; verified against the slack-go v0.27.0 source: apps.connections.open
// uses Client.endpoint exactly like every other Web API call, and
// socketmode.Client embeds a copy of that Client, so it inherits the same
// endpoint — no separate socket-mode URL override exists or is needed).
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

	// AllowedUsers, when non-empty, restricts reaction commands to these
	// Slack member IDs; reactions from anyone else are logged and ignored
	// before any ownership resolution happens. Empty means anyone who can
	// react in the channel may command the queue (the default).
	AllowedUsers []string
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
// so an owned reaction can be turned back into a retry Command. Populated
// for both single-run and batch roots identically — the in-memory fast path
// (handleSocketEvent's roots-map hit) is unchanged by durable ownership
// (§4.4 doc comment): it always resolves to whatever candidate's ref
// postRoot recorded when the root was first posted, batch or not. Only the
// metadata-fetch fallback (used once this entry has been forgotten, at
// terminal) distinguishes a batch root from a single-run one, by the
// payload's shape.
type rootInfo struct {
	Target string
	Ref    string
}

// gauntletRunEventType is the Slack message-metadata event_type attached to
// every root post (single-run and batch alike), so a reaction arriving after
// the in-memory roots map has forgotten the run can still be traced back to
// its owning (target, ref) — the fix for reaction-retry never having worked
// end-to-end: humans react to a terminal ❌ root, by which point §9.2's
// cleanup has already deleted the roots/runRoot entries.
const gauntletRunEventType = "gauntlet_run"

// Reaction-add emoji used to acknowledge an inbound reaction:
// ackEyes confirms a command was actually minted; ackQuestion instead marks
// a reaction on a batch root, which never mints a command (a bare reaction
// can't name which member it means).
const (
	ackEyes     = "eyes"
	ackQuestion = "question"
)

// batchGuidanceText is the threaded reply posted alongside ackQuestion when
// a reaction lands on a batch root: batch-member-level commands
// via Slack are explicitly out of scope — retrying or cancelling one member
// of a batch requires the API or CLI, which can name that member's ref.
const batchGuidanceText = "this is a batch root; a reaction can't target one member of it. To retry or cancel a single member, use the API (`POST /api/v1/retry` or `/api/v1/cancel`) or the CLI (`gauntlet retry`/`gauntlet cancel`) naming that member's ref directly."

// refRetryKey is the (target, ref) a reaction-minted retry was minted for —
// the key refRetry tracks so the NEXT trial-clean for the same ref threads
// under the old root instead of starting a fresh one (see postRetryRoot).
type refRetryKey struct {
	Target string
	Ref    string
}

// refRetryEntry records which root ts a reaction-retry was minted from, and
// when that record expires if never consumed by a matching trial-clean.
type refRetryEntry struct {
	rootTS    string
	expiresAt time.Time
}

// refRetryTTL bounds how long a refRetry entry waits for the trial-clean it
// anticipates before it's treated as stale. A retry that
// never results in a new trial-clean (e.g. the ref was deleted, or the
// operator abandoned it) must not pin its root's identity forever.
const refRetryTTL = time.Hour

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
	// signalProcessed fires after both an outbound event (drainOutbox) AND
	// an inbound socket event (handleSocketEvents) have been fully handled,
	// so tests can synchronize on either direction's side effects.
	mu      sync.Mutex
	runRoot map[string]string   // root-tracking key -> root message ts (batch-aware: BatchID when set, else RunID)
	roots   map[string]rootInfo // root message ts -> (target, ref)
	notify  chan struct{}

	// refRetry tracks reaction-minted retries so the next trial-clean for
	// the same (target, ref) threads under the old root instead of starting
	// a fresh one (see postRetryRoot). Bounded the same way batchRecs is: entries
	// are deleted the instant they're consumed by a matching postRoot call,
	// or lazily swept (recordRefRetryLocked) whenever a new one is recorded,
	// so a retry that's never followed by a matching trial-clean doesn't pin
	// its root identity forever — refRetryTTL bounds the wait.
	refRetry map[refRetryKey]refRetryEntry

	// botUserID is this app's own bot user id, fetched once from auth.test
	// at Run start. It's the authorship check for a reaction
	// that misses the in-memory roots map: only a message this bot itself
	// posted can be trusted to carry a genuine gauntlet_run payload — anyone
	// can react to anyone's message, so a foreign message's metadata (if it
	// even has any) must never be treated as a command. Empty if auth.test
	// failed or hasn't run yet, in which case isOwnMessage never matches
	// (fail closed, not open).
	//
	// botID is the companion B… bot id from the same auth.test response.
	// Both are checked because a bot-posted message read back through
	// conversations.history is a subtype:"bot_message" object whose
	// top-level user field Slack does not reliably populate — identity may
	// be carried only in bot_id. Another app's messages carry a different
	// bot_id, so accepting either match doesn't widen the trust boundary.
	botUserID string
	botID     string

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

	// allowedUsers is Params.AllowedUsers as a set. Nil/empty = no
	// restriction. Write-once in New, lock-free reads (same publication
	// pattern as now).
	allowedUsers map[string]struct{}
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

	var allowed map[string]struct{}
	if len(p.AllowedUsers) > 0 {
		allowed = make(map[string]struct{}, len(p.AllowedUsers))
		for _, u := range p.AllowedUsers {
			allowed[u] = struct{}{}
		}
	}

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
		refRetry:  make(map[refRetryKey]refRetryEntry),
		now:       time.Now,

		allowedUsers: allowed,
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
//
// Before doing anything else, it calls auth.test once to learn this app's
// own bot user id — the authorship check the metadata-fetch
// reaction path needs to tell "our root, reacted to after we forgot it"
// apart from "someone else's message with a coincidentally similar shape".
// A failure here is logged, not fatal: the channel still posts/threads
// normally, it just can't resolve a reaction that misses the in-memory
// roots map (isOwnMessage never matches with an empty botUserID, so such
// reactions are safely ignored rather than trusted).
func (s *Slack) Run(ctx context.Context) error {
	if resp, err := s.api.AuthTestContext(ctx); err != nil {
		s.logf("slack: auth.test failed (reaction ownership fetch will be disabled): %v", err)
	} else {
		s.botUserID = resp.UserID
		s.botID = resp.BotID
	}

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
	case ev.Kind == core.EventHookSkipped:
		s.postHookSkipped(ctx, ev)
		// EventHookStarted is deliberately a no-op here (N3, phase-6 B-track
		// review, a behavior change from the phase-6 B-track plan): live
		// hook-in-progress state is the dashboard/API's job (S5's
		// hooks.LiveState surface), not Slack's. Posting a standalone
		// top-level message per hook start, on top of postHookFinished's
		// per-hook post, was ~2N standalone channel messages for every
		// N-hook landing — noise nobody acted on. EventHookSkipped (above)
		// stays: it's rare and load-bearing (S1-C's crash-recovery
		// discoverability), unlike routine hook-start chatter.
		//
		// EventRetryRequested is a history-only durability signal (S3, phase-6
		// B-track plan): "Other channels default-ignore it; only history acts."
		// Falling through here (no case) is deliberate, not an oversight.
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
	s.sweepExpiredRefRetryLocked()
	key := refRetryKey{Target: ev.Target, Ref: ev.Candidate.Ref}
	entry, retrying := s.refRetry[key]
	if retrying {
		// Consumed on use regardless of freshness — a second trial-clean
		// for the same ref must not thread under the same reaction-retry
		// root twice.
		delete(s.refRetry, key)
		retrying = !s.now().After(entry.expiresAt)
	}
	s.mu.Unlock()

	if retrying {
		s.postRetryRoot(ctx, ev, entry.rootTS)
		return
	}
	s.postFreshRoot(ctx, ev)
}

// postFreshRoot posts a brand-new root message for ev, attaching Slack
// message metadata (event_type gauntlet_run, payload {target, ref}) so a
// reaction arriving after this run has terminated — and its roots/runRoot
// entries forgotten — can still be traced back to (ev.Target,
// ev.Candidate.Ref) via handleForeignReaction. This is the provisional,
// single-run shape of the payload: if ev turns out to be the first member of
// a genuine multi-member batch (indistinguishable from a solo run at this
// point — batch membership is only known once postBatchTerminal sees
// rec.BatchSize > 1), finishBatch overwrites this metadata with the
// authoritative batch shape (payload {target} only, no ref) in the same
// chat.update call that edits the root to its final verdict — always before
// any post-termination reaction fetch could observe it, since fetching only
// ever happens after the in-memory roots-map entry (written below) has
// already been forgotten.
func (s *Slack) postFreshRoot(ctx context.Context, ev core.Event) {
	text := fmt.Sprintf("⏳ testing %s (%s) → %s", ev.Candidate.Topic, displayUser(ev.Candidate.User), ev.Target)
	meta := goslack.SlackMetadata{EventType: gauntletRunEventType, EventPayload: map[string]any{"target": ev.Target, "ref": ev.Candidate.Ref}}
	_, ts, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(text, false), goslack.MsgOptionMetadata(meta))
	if err != nil {
		s.logf("slack: chat.postMessage failed run=%s: %v", ev.RunID, err)
		return
	}

	s.mu.Lock()
	s.runRoot[ev.RunID] = ts
	s.roots[ts] = rootInfo{Target: ev.Target, Ref: ev.Candidate.Ref}
	s.mu.Unlock()
}

// postRetryRoot handles ev's trial-clean when it matches a refRetry record:
// rather than posting a fresh root, it threads continuity under
// rootTS — the root a prior reaction-retry was minted from — posting a
// threaded "retesting" notice, re-editing rootTS's own text to show the
// retry is underway, and re-pointing this run's tracking entries at rootTS
// so every subsequent event for ev.RunID (check replies, the terminal edit)
// lands there too, exactly as if this had been the run's root all along.
func (s *Slack) postRetryRoot(ctx context.Context, ev core.Event, rootTS string) {
	if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText("⏳ retesting (retry)", false), goslack.MsgOptionTS(rootTS)); err != nil {
		s.logf("slack: retry-continuity threaded notice failed run=%s: %v", ev.RunID, err)
	}

	text := fmt.Sprintf("⏳ retrying %s (%s) → %s", ev.Candidate.Topic, displayUser(ev.Candidate.User), ev.Target)
	if _, _, _, err := s.api.UpdateMessageContext(ctx, s.channel, rootTS, goslack.MsgOptionText(text, false)); err != nil {
		s.logf("slack: retry-continuity root re-edit failed run=%s: %v", ev.RunID, err)
	}

	s.mu.Lock()
	s.runRoot[ev.RunID] = rootTS
	s.roots[rootTS] = rootInfo{Target: ev.Target, Ref: ev.Candidate.Ref}
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
	// Re-attach metadata here too (not just at postFreshRoot): this confirms
	// the single-run shape ({target, ref}) as authoritative now that the run
	// has actually finished as a solo run, not a batch — see postFreshRoot's
	// doc comment on why the metadata posted at root time is provisional.
	meta := goslack.SlackMetadata{EventType: gauntletRunEventType, EventPayload: map[string]any{"target": ev.Target, "ref": ev.Candidate.Ref}}
	if _, _, _, err := s.api.UpdateMessageContext(ctx, s.channel, rootTS, goslack.MsgOptionText(headline, false), goslack.MsgOptionMetadata(meta)); err != nil {
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
		// Authoritative batch metadata: payload carries {target}
		// only, deliberately omitting ref — a reaction on a batch root can't
		// name which member it means, so handleForeignReaction's payload
		// shape check (ref present/absent) is what tells a fetched batch
		// root apart from a single-run one after termination. A batch of
		// exactly one member is the one exception (mirroring batchHeadline/
		// summarizeBatch's own "degrades to serial behavior byte for byte",
		// §4.1): there's no ambiguity about which member a reaction means,
		// so it gets the single-run shape (ref included) too, same as a
		// genuine serial run.
		meta := goslack.SlackMetadata{EventType: gauntletRunEventType, EventPayload: map[string]any{"target": target}}
		if len(recs) == 1 {
			if head := firstBatchRec(recs); head != nil {
				meta.EventPayload["ref"] = head.Candidate.Ref
			}
		}
		if _, _, _, err := s.api.UpdateMessageContext(ctx, s.channel, rootTS, goslack.MsgOptionText(headline, false), goslack.MsgOptionMetadata(meta)); err != nil {
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

// postHookSkipped posts a standalone channel message when a recovery-skipped
// landing's hooks never ran at all (S1-C discoverability): a crash-recovered
// landing has no merge SHA to export a tree from
// (hooks.Runner.runLanding's "recovered landing, skipping hooks" doc), so its
// hooks are skipped entirely — this is the whole point of S1-C's durable,
// surfaced-everywhere marker.
func (s *Slack) postHookSkipped(ctx context.Context, ev core.Event) {
	text := fmt.Sprintf("⚠ hooks skipped (recovery) → %s", ev.Target)
	if ev.Detail != "" {
		text += ": " + ev.Detail
	}
	if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(text, false)); err != nil {
		s.logf("slack: hook-skipped post failed run=%s: %v", ev.RunID, err)
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
			s.handleSocketEvent(ctx, evt)
			s.signalProcessed()
		}
	}
}

func (s *Slack) handleSocketEvent(ctx context.Context, evt socketmode.Event) {
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
	kind, ok := reactionCommandKind(reaction.Reaction)
	if !ok {
		// Every other reaction (and every other inner event type) is
		// ignored.
		return
	}
	if reaction.Item.Type != "message" {
		// A reaction on a file or file comment has no message timestamp to
		// resolve; skip it rather than issue a pointless history fetch.
		return
	}
	if len(s.allowedUsers) > 0 {
		if _, ok := s.allowedUsers[reaction.User]; !ok {
			// Deliberately silent toward the channel (no ❓ ack — don't
			// invite probing); loud in the daemon log so a misconfigured
			// allowlist is diagnosable.
			s.logf("slack: ignoring :%s: from user %s not in allowed-users", reaction.Reaction, reaction.User)
			return
		}
	}

	ts := reaction.Item.Timestamp
	s.mu.Lock()
	info, owned := s.roots[ts]
	s.mu.Unlock()
	if owned {
		// The still-running (or not-yet-forgotten) case: unchanged by
		// durable ownership (package doc comment) — always resolves via
		// whatever rootInfo postRoot recorded, batch root or not.
		s.mintCommand(ctx, kind, ts, info.Target, info.Ref)
		return
	}

	// Miss: either a reaction on someone else's message, or — the bug this
	// design fixes — a reaction on OUR OWN root after its run has already
	// terminated and §9.2's cleanup forgot it. Fetch the reacted message
	// (with metadata) and verify ownership before trusting anything in it.
	s.handleForeignReaction(ctx, reaction.Item.Channel, ts, kind)
}

// reactionCommandKind maps a reaction_added emoji name to the core.Command
// kind it means: ":recycle:" -> retry (re-queue the same SHA); ":x:" ->
// cancel (park it and stop whatever is currently in flight for it). ok is
// false for every other reaction name, which callers must ignore.
func reactionCommandKind(name string) (kind string, ok bool) {
	switch name {
	case "recycle":
		return core.CommandRetry, true
	case "x":
		return core.CommandCancel, true
	default:
		return "", false
	}
}

// mintCommand enqueues cmd (kind, target, ref) on s.cmds, and — only once
// the enqueue actually succeeds — acknowledges the reaction with ackEyes
// ("when a command is minted, immediately reactions.add"). For a retry it
// also records refRetry, so the NEXT trial-clean for (target, ref) threads
// continuity under ts instead of starting a fresh root. Shared by both the
// in-memory roots-map hit and the metadata-fetch fallback: from here on,
// minting a command looks identical regardless of which path found its
// owner.
//
// Ordering is load-bearing (fresh-context review, F1): the refRetry record
// is written BEFORE the channel send. The daemon consuming s.cmds can act on
// a retry immediately — re-queue the ref, run its trial merge, and Emit the
// resulting EventTrialClean — and Go's memory model only orders writes made
// before a channel send against that receiver; recording after the send
// would let postRoot occasionally miss the entry and post a fresh root
// instead of threading under ts. A send that fails (cmds buffer full: the
// command is dropped, nothing will ever consume it) rolls the record back.
//
// That rollback is the ONLY dead-entry cleanup besides the TTL. A retry
// that reaches the daemon but turns out to be a no-op (the ref wasn't
// parked — already landed, or already cleared) leaves its entry armed: if
// the same ref name is genuinely re-pushed within refRetryTTL, that push's
// trial-clean threads under the old root and re-edits it, rather than
// starting a fresh root. The channel can't tell those apart — a trial-clean
// carries no "was this my retry?" marker — so this is an accepted
// presentation-only wrinkle (the queue itself lands the push correctly),
// bounded by the TTL.
func (s *Slack) mintCommand(ctx context.Context, kind, ts, target, ref string) {
	if kind == core.CommandRetry {
		s.mu.Lock()
		s.recordRefRetryLocked(target, ref, ts)
		s.mu.Unlock()
	}

	cmd := core.Command{Kind: kind, Target: target, Ref: ref}
	select {
	case s.cmds <- cmd:
	default:
		s.logf("slack: cmds buffer full (%d), dropping %s target=%s ref=%s", cmdsBuffer, kind, target, ref)
		if kind == core.CommandRetry {
			s.mu.Lock()
			delete(s.refRetry, refRetryKey{Target: target, Ref: ref})
			s.mu.Unlock()
		}
		return
	}

	s.ack(ctx, ts, ackEyes)
}

// recordRefRetryLocked records that a retry was just minted from root ts for
// (target, ref), sweeping expired entries first. Must be called with s.mu
// held.
func (s *Slack) recordRefRetryLocked(target, ref, ts string) {
	s.sweepExpiredRefRetryLocked()
	s.refRetry[refRetryKey{Target: target, Ref: ref}] = refRetryEntry{rootTS: ts, expiresAt: s.now().Add(refRetryTTL)}
}

// sweepExpiredRefRetryLocked drops every expired refRetry entry. It
// piggybacks on the two places that already hold s.mu and touch the map —
// minting a retry, and every trial-clean's postRoot — the same pattern
// collectStaleBatchesLocked uses for batchRecs, so a never-consumed record
// is cleared by the next trial-clean on ANY ref after its TTL, not just by
// the next minted retry. Must be called with s.mu held.
func (s *Slack) sweepExpiredRefRetryLocked() {
	now := s.now()
	for k, e := range s.refRetry {
		if now.After(e.expiresAt) {
			delete(s.refRetry, k)
		}
	}
}

// handleForeignReaction resolves a reaction whose ts missed the in-memory
// roots map: it fetches the reacted message — with metadata —
// via conversations.history (latest=oldest=ts, inclusive, limit 1: the
// documented way to fetch exactly one message by ts), and only trusts it if
// both (a) this bot itself posted it (s.botUserID, from auth.test at Run
// start) and (b) its metadata event_type is gauntlet_run. Anything else
// (message not found, some other app's or user's message, a message with
// unrelated or no metadata) is ignored silently — never minting a command
// from a message we can't prove we own. A batch root (payload carrying
// target but no ref — see finishBatch) mints no command at all; instead it's
// acknowledged with ackQuestion plus a threaded reply explaining that a
// single member can't be targeted via a bare reaction.
func (s *Slack) handleForeignReaction(ctx context.Context, channel, ts, kind string) {
	if channel == "" {
		channel = s.channel
	}
	resp, err := s.api.GetConversationHistoryContext(ctx, &goslack.GetConversationHistoryParameters{
		ChannelID:          channel,
		Latest:             ts,
		Oldest:             ts,
		Inclusive:          true,
		Limit:              1,
		IncludeAllMetadata: true,
	})
	if err != nil {
		s.logf("slack: conversations.history ts=%s failed: %v", ts, err)
		return
	}
	if len(resp.Messages) == 0 {
		// Not found (deleted, wrong channel, or outside retention): nothing
		// to own.
		return
	}

	msg := resp.Messages[0]
	if !s.isOwnMessage(msg) || msg.Metadata.EventType != gauntletRunEventType {
		// Foreign message: someone else's post, or ours but not a
		// gauntlet_run root (e.g. a threaded reply or summary). Ignored
		// silently — reacting to those was never meaningful.
		return
	}

	target, _ := msg.Metadata.EventPayload["target"].(string)
	if target == "" {
		// Ours and the right event_type, but somehow missing the one field
		// every payload always carries: too malformed to act on.
		return
	}
	ref, hasRef := msg.Metadata.EventPayload["ref"].(string)
	if !hasRef || ref == "" {
		// Batch root (finishBatch's payload omits ref): out of scope for a
		// bare reaction — see batchGuidanceText.
		s.ackBatchGuidance(ctx, ts)
		return
	}

	s.mintCommand(ctx, kind, ts, target, ref)
}

// isOwnMessage reports whether msg was posted by this bot, per the user id
// and bot id fetched from auth.test at Run start. A bot-posted message read
// back through conversations.history may carry its identity in either field
// — user is not reliably populated on subtype:"bot_message" objects — so
// either match suffices; a foreign app's message matches neither. Fails
// closed: empty ids (auth.test never ran, or failed) never match anything,
// rather than treating "unknown" as "ours".
func (s *Slack) isOwnMessage(msg goslack.Message) bool {
	if s.botUserID != "" && msg.User == s.botUserID {
		return true
	}
	return s.botID != "" && msg.BotID == s.botID
}

// ack adds emoji as a reaction to the message at ts in this channel
// (reactions.add). s.channel — not the reaction event's own channel — is
// deliberately correct here, not an oversight: an ack only ever follows a
// successful ownership resolution, and every message this bot posts (the
// only messages that can pass isOwnMessage + the gauntlet_run event_type
// check, or appear in the roots map) lives in s.channel by construction.
// Errors are logged, not returned: like every other Slack call failure in
// this package, losing an acknowledgment must never do anything worse than
// lose an acknowledgment.
func (s *Slack) ack(ctx context.Context, ts, emoji string) {
	if err := s.api.AddReactionContext(ctx, emoji, goslack.NewRefToMessage(s.channel, ts)); err != nil {
		s.logf("slack: reactions.add %s ts=%s failed: %v", emoji, ts, err)
	}
}

// ackBatchGuidance acknowledges a reaction on a batch root: a
// ackQuestion reaction plus a threaded reply pointing at the API/CLI for
// member-level commands, since a bare Slack reaction can't name which
// member it means.
func (s *Slack) ackBatchGuidance(ctx context.Context, ts string) {
	s.ack(ctx, ts, ackQuestion)
	if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(batchGuidanceText, false), goslack.MsgOptionTS(ts)); err != nil {
		s.logf("slack: batch-guidance reply ts=%s failed: %v", ts, err)
	}
}
