// Package channel implements gauntlet's core.Channel: the duplex event/command
// surface between the queue and the outside world. Phase 1 ships one real
// implementation, LogChannel, plus RecordingChannel, a test double used by
// queue and integration tests.
package channel

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/sgrankin/gauntlet/internal/core"
)

var _ core.Channel = (*LogChannel)(nil)

// LogChannel is the phase-1 core.Channel implementation. It renders every
// Event as one structured, greppable line (key=value pairs), and for
// terminal events additionally renders the carried RunRecord as a compact
// summary line. It is output-only: Commands never yields.
//
// New core.EventKind values are additive (e.g. core.EventIgnoredRef, phase
// 2): eventKindString's default case renders any kind it doesn't recognize
// as "unknown(N)" rather than panicking or dropping the line, and every
// core.Channel implementation is expected to hold the same contract —
// ignore event kinds it doesn't understand, never fail Emit over one.
type LogChannel struct {
	mu   sync.Mutex
	w    io.Writer
	cmds chan core.Command
}

// NewLogChannel returns a LogChannel writing to w. A nil w defaults to
// os.Stderr.
func NewLogChannel(w io.Writer) *LogChannel {
	if w == nil {
		w = os.Stderr
	}
	return &LogChannel{
		w:    w,
		cmds: make(chan core.Command),
	}
}

// Emit renders ev and writes it to the configured writer.
//
// Emit never blocks the reconcile loop and never fails it: a write failure
// is swallowed rather than returned, since losing a log line must never stop
// the queue. The error return exists only to satisfy core.Channel.
func (c *LogChannel) Emit(ctx context.Context, ev core.Event) error {
	line := formatEvent(ev)
	if ev.Record != nil {
		line += "\n" + formatRunRecord(ev.Record)
		if block := formatFailureBlock(ev.Record); block != "" {
			line += "\n" + block
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintln(c.w, line)
	return nil
}

// Caps on the failing-check tail LogChannel appends after a terminal
// summary (DESIGN.md Watch: "Channels should include the failing check's
// output tail in terminal notifications"). Generous for a local terminal —
// unlike Slack's 3000-char text limit, there's no hard ceiling here, just a
// courtesy cap so one runaway check doesn't flood the log.
const (
	failureTailMaxLines = 10
	failureTailMaxBytes = 1024
)

// formatFailureBlock renders the first failing check's output tail as an
// indented, greppable block (each line prefixed with four spaces + "| "),
// visually grouped under the summary line it follows. Returns "" when rec
// has no failing check or the failing check's tail is empty (e.g. a
// CheckSkipped-only rejection, or a check that failed with no output).
func formatFailureBlock(rec *core.RunRecord) string {
	res := rec.FirstFailure()
	if res == nil {
		return ""
	}
	tail := core.FailureTail(res, failureTailMaxLines, failureTailMaxBytes)
	if tail == "" {
		return ""
	}
	lines := strings.Split(tail, "\n")
	for i, ln := range lines {
		lines[i] = "    | " + ln
	}
	return strings.Join(lines, "\n")
}

// Commands returns a channel that never yields. It is a real channel value
// created once in NewLogChannel and closed over here — not a nil channel —
// so LogChannel's zero-command behavior is an explicit, well-defined value
// (safe to hold, compare, or select on) rather than an accident of an
// uninitialized field. Nothing ever sends on it and it is never closed, so a
// select against it always falls through to its other cases, exactly like a
// nil channel would; the difference is that this is a deliberate choice
// recorded in one place, not a nil chan that happened to fall out of a
// struct's zero value.
func (c *LogChannel) Commands() <-chan core.Command {
	return c.cmds
}

func formatEvent(ev core.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s kind=%s target=%s", ev.At.Format(timeFormat), eventKindString(ev.Kind), ev.Target)
	if ev.Candidate.Ref != "" {
		fmt.Fprintf(&b, " ref=%s", ev.Candidate.Ref)
	}
	if ev.Candidate.SHA != "" {
		fmt.Fprintf(&b, " sha=%s", shortSHA(ev.Candidate.SHA))
	}
	if ev.RunID != "" {
		fmt.Fprintf(&b, " run=%s", ev.RunID)
	}
	if ev.CheckName != "" {
		fmt.Fprintf(&b, " check=%s", ev.CheckName)
	}
	if ev.Detail != "" {
		fmt.Fprintf(&b, " detail=%q", ev.Detail)
	}
	return b.String()
}

func formatRunRecord(rec *core.RunRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  run=%s outcome=%s target=%s checks=[", rec.RunID, outcomeString(rec.Outcome), rec.Target)
	for i, cr := range rec.Checks {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%s(%s)", cr.Name, checkStatusString(cr.Status), cr.Duration)
	}
	fmt.Fprintf(&b, "] wall=%s", rec.EndedAt.Sub(rec.StartedAt))
	if rec.Detail != "" {
		fmt.Fprintf(&b, " detail=%q", rec.Detail)
	}
	return b.String()
}

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// shortSHA truncates a full SHA to a short, human-scannable prefix, matching
// git's usual abbreviation length. Shorter inputs (e.g. already-truncated
// test fixtures) pass through unchanged.
func shortSHA(sha string) string {
	const n = 8
	if len(sha) > n {
		return sha[:n]
	}
	return sha
}

func eventKindString(k core.EventKind) string {
	switch k {
	case core.EventQueued:
		return "queued"
	case core.EventTrialClean:
		return "trial_clean"
	case core.EventTrialConflict:
		return "trial_conflict"
	case core.EventCheckStarted:
		return "check_started"
	case core.EventCheckFinished:
		return "check_finished"
	case core.EventLanded:
		return "landed"
	case core.EventRejected:
		return "rejected"
	case core.EventSkipped:
		return "skipped"
	case core.EventError:
		return "error"
	case core.EventIgnoredRef:
		return "ignored_ref"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

func checkStatusString(s core.CheckStatus) string {
	switch s {
	case core.CheckPassed:
		return "passed"
	case core.CheckFailed:
		return "failed"
	case core.CheckSkipped:
		return "skipped"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

func outcomeString(o core.Outcome) string {
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
