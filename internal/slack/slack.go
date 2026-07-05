// Package slack implements a duplex core.Channel over Slack's socket-mode
// protocol (docs/plans/phase23.md §4.4): outbound, it threads one root
// message per run, threaded replies for each check, and edits the root to
// its final verdict; inbound, it turns a ":recycle:" reaction on an owned
// root message into a core.Command{Kind: core.CommandRetry}.
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
	runRoot map[string]string   // core.Event.RunID -> root message ts
	roots   map[string]rootInfo // root message ts -> (target, ref)
	notify  chan struct{}
}

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
		channel: p.Channel,
		api:     api,
		smc:     smc,
		log:     logw,
		outbox:  make(chan core.Event, outboxBuffer),
		cmds:    make(chan core.Command, cmdsBuffer),
		runRoot: make(map[string]string),
		roots:   make(map[string]rootInfo),
		notify:  make(chan struct{}),
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

// Commands returns the channel core.Command values (currently only
// core.CommandRetry, from an owned ":recycle:" reaction) are delivered on.
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
	}
}

// postRoot posts the root message for a newly clean trial and records
// runID->rootTS and rootTS->(target,ref) so later events for this run
// (check replies, the terminal edit) and an inbound reaction can find it.
func (s *Slack) postRoot(ctx context.Context, ev core.Event) {
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
// finishing. core.Event carries only the check's name at this point (the
// verdict and duration are only known once the check finishes and land in
// the terminal RunRecord — see postTerminal), so these interim replies are
// necessarily just a progress marker; the final threaded summary is where
// each check's verdict and duration actually appear.
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
		text = fmt.Sprintf("◾ %s finished", ev.CheckName)
	}

	if _, _, err := s.api.PostMessageContext(ctx, s.channel, goslack.MsgOptionText(text, false), goslack.MsgOptionTS(rootTS)); err != nil {
		s.logf("slack: threaded reply failed run=%s check=%s: %v", ev.RunID, ev.CheckName, err)
	}
}

// postTerminal edits the root message to its final verdict, posts a final
// threaded summary (outcome, one line per check, run ID), and then deletes
// both map entries for this run (§9.2 — a long-running daemon must not
// leak an entry per run).
func (s *Slack) postTerminal(ctx context.Context, ev core.Event) {
	rootTS, ok := s.lookupRoot(ev.RunID)
	if !ok {
		// No known root: e.g. this channel started after the trial-clean
		// event, or the root post itself was dropped (outbox overflow).
		// Nothing to edit, nothing to clean up.
		return
	}

	rec := ev.Record
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
	if !ok || reaction.Reaction != "recycle" {
		// Unknown reactions (and every other inner event type) ignored.
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

	cmd := core.Command{Kind: core.CommandRetry, Target: info.Target, Ref: info.Ref}
	select {
	case s.cmds <- cmd:
	default:
		s.logf("slack: cmds buffer full (%d), dropping retry target=%s ref=%s", cmdsBuffer, info.Target, info.Ref)
	}
}
