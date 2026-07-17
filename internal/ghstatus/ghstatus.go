// Package ghstatus implements a core.Channel that posts one rollup GitHub
// commit status per target to the candidate SHA, via the plain REST
// "statuses" API:
//
//	POST {api}/repos/{owner}/{repo}/statuses/{candidate_sha}
//
// It is output-only: Commands never yields — commit-status posting has
// no inbound command surface, and never will.
package ghstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

var _ core.Channel = (*Channel)(nil)

// TokenSource supplies the credential for each status POST. Implemented
// by internal/ghauth (App installation tokens and static PATs); declared
// here so the dependency points the right way, mirroring gitx.TokenSource.
type TokenSource interface {
	// Token returns a currently valid token; called once per POST — the
	// provider owns caching and refresh.
	Token(ctx context.Context) (string, error)
	// Invalidate reports that token was rejected (401), so the provider
	// drops it before the caller's single retry.
	Invalidate(token string)
}

// staticToken adapts Params.Token, the fixed-PAT shorthand.
type staticToken string

func (s staticToken) Token(ctx context.Context) (string, error) { return string(s), nil }
func (staticToken) Invalidate(string)                           {}

// Params configures a Channel. It is a package-local struct: mapping
// from parsed config lives only in cmd, so this package never imports
// internal/config.
type Params struct {
	// Owner and Repo identify the GitHub repository (e.g. "acme" and
	// "widgets" for github.com/acme/widgets).
	Owner string
	Repo  string

	// Token is a fixed PAT sent as "Authorization: token <Token>" —
	// static-mode shorthand for Tokens.
	Token string

	// Tokens, when non-nil, overrides Token: every POST requests a
	// current credential from it, and a 401 invalidates + retries once
	// with a fresh one. This is how App installation tokens (which
	// expire hourly) flow in; see internal/ghauth.
	Tokens TokenSource

	// TrialRefs selects merge-SHA VERIFICATION statuses (issue #7) over
	// today's candidate-SHA statuses. When set, the rollup describes
	// verification of the exact synthetic merge: pending when the merge is
	// published (EventTrialMerged), success when its required graph is
	// green (EventVerified), failure on a source rejection, error on
	// infrastructure — all posted to the merge SHA, never repainted by the
	// subsequent landing. A trial conflict, which has no synthetic commit,
	// still reports against the candidate SHA. Off ⇒ today's exact
	// behavior (candidate SHA, EventTrialClean/EventLanded lifecycle).
	TrialRefs bool

	// APIURL is the GitHub REST API base URL. Defaults to
	// "https://api.github.com" when empty.
	APIURL string

	// DashboardURL, when non-empty, is the dashboard's public base URL.
	// target_url is built as "<DashboardURL>/run/<runID>"; omitted from the
	// posted status when DashboardURL is empty.
	DashboardURL string

	// Log receives one line per dropped Emit error. Defaults to os.Stderr
	// when nil, matching LogChannel's spirit (internal/channel/log.go).
	Log io.Writer
}

// requestTimeout bounds every status POST so Emit never blocks the
// reconcile loop on a slow or unreachable GitHub.
const requestTimeout = 5 * time.Second

// descriptionCap is the length (in runes) GitHub's commit-status
// "description" field is truncated to.
const descriptionCap = 140

// Channel is a core.Channel that posts one rollup commit status,
// "gauntlet/<target>", to the candidate SHA for events that map to a status
// (see statusFor). It is output-only.
type Channel struct {
	owner, repo  string
	tokens       TokenSource
	apiURL       string
	dashboardURL string
	trialRefs    bool

	client *http.Client
	cmds   chan core.Command

	logMu sync.Mutex
	log   io.Writer
}

// New returns a Channel configured by p.
func New(p Params) *Channel {
	apiURL := p.APIURL
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	logw := p.Log
	if logw == nil {
		logw = os.Stderr
	}
	tokens := p.Tokens
	if tokens == nil {
		tokens = staticToken(p.Token)
	}
	return &Channel{
		owner:        p.Owner,
		repo:         p.Repo,
		tokens:       tokens,
		apiURL:       apiURL,
		dashboardURL: p.DashboardURL,
		trialRefs:    p.TrialRefs,
		client:       &http.Client{Timeout: requestTimeout},
		cmds:         make(chan core.Command),
		log:          logw,
	}
}

// Emit maps ev to a GitHub commit status (see statusFor) and posts it.
// Events that don't map to a status
// (EventSkipped — transient; re-posts pending on the next trial — and any
// other non-terminal kind) post nothing.
//
// Emit never blocks the reconcile loop and never fails it: POST errors are
// written to the configured log writer and dropped, never returned, since a
// dropped status must never stop the queue. The error return exists only to
// satisfy core.Channel.
func (c *Channel) Emit(ctx context.Context, ev core.Event) error {
	state, description, sha, ok := c.statusFor(ev)
	if !ok || sha == "" {
		return nil
	}
	if err := c.post(ctx, ev, state, description, sha); err != nil {
		c.logf("ghstatus: %v", err)
	}
	return nil
}

// Commands returns a channel that never yields. It is a real channel value
// created once in New and closed over here, matching LogChannel's rationale
// (internal/channel/log.go) for why this is preferable to a nil channel.
func (c *Channel) Commands() <-chan core.Command {
	return c.cmds
}

// state is a GitHub commit-status state value.
type state string

const (
	statePending state = "pending"
	stateSuccess state = "success"
	stateFailure state = "failure"
	stateError   state = "error"
)

// statusFor maps ev to the state, description, and target SHA to post. ok
// is false for event kinds that post nothing; the caller also skips an
// empty SHA. Two modes: candidate-SHA (today's default) and merge-SHA
// verification (Config/Params.TrialRefs, issue #7).
func (c *Channel) statusFor(ev core.Event) (s state, description, sha string, ok bool) {
	if c.trialRefs {
		return c.verificationStatusFor(ev)
	}
	return candidateStatusFor(ev)
}

// candidateStatusFor is the pre-#7 behavior: one rollup on the CANDIDATE
// SHA, pending at trial-clean through success at landing.
func candidateStatusFor(ev core.Event) (s state, description, sha string, ok bool) {
	switch ev.Kind {
	case core.EventTrialClean:
		return statePending, "running checks", ev.Candidate.SHA, true
	case core.EventLanded:
		return stateSuccess, "landed", ev.Candidate.SHA, true
	case core.EventRejected:
		return stateFailure, capDescription(detailOf(ev)), ev.Candidate.SHA, true
	case core.EventTrialConflict:
		return stateFailure, "trial merge conflict", ev.Candidate.SHA, true
	case core.EventError:
		return stateError, capDescription(detailOf(ev)), ev.Candidate.SHA, true
	case core.EventHookFinished:
		// Deliberately ignored: the commit status describes the LANDING,
		// and a post-land hook failure must not
		// repaint an already-green landing status red — that's the CD
		// hand-off boundary (DESIGN.md's decision ledger, "Deployments as
		// post-land hooks"). A failed hook is Slack's and the log
		// channel's job to surface, not ghstatus's.
		return "", "", "", false
	default:
		// core.EventSkipped and every non-terminal kind (Queued,
		// CheckStarted, CheckFinished, IgnoredRef): no post.
		return "", "", "", false
	}
}

// verificationStatusFor is the #7 behavior: the rollup describes
// verification of the exact synthetic MERGE, on the merge SHA. Pending
// when published, success when the graph is green (BEFORE landing),
// failure/error on rejection/infra. EventLanded and EventTrialClean post
// nothing here — verification is not landing, and the pending status came
// from EventTrialMerged with the real MergeSHA. EventSkipped posts nothing
// (a superseded/re-tested merge, e.g. a batch falling back to serial: the
// per-member serial re-runs carry the definitive statuses).
func (c *Channel) verificationStatusFor(ev core.Event) (s state, description, sha string, ok bool) {
	switch ev.Kind {
	case core.EventTrialMerged:
		return statePending, "verifying merge", mergeSHAOf(ev), true
	case core.EventVerified:
		return stateSuccess, "merge verified", mergeSHAOf(ev), true
	case core.EventRejected:
		return stateFailure, capDescription(detailOf(ev)), mergeSHAOf(ev), true
	case core.EventError:
		return stateError, capDescription(detailOf(ev)), mergeSHAOf(ev), true
	case core.EventTrialConflict:
		// The documented exception: a conflict produces no synthetic
		// commit, so there is no merge SHA to status — report against the
		// candidate, as candidate mode does.
		return stateFailure, "trial merge conflict", ev.Candidate.SHA, true
	default:
		// EventTrialClean, EventLanded, EventSkipped, EventHookFinished,
		// and every non-terminal kind: no verification post.
		return "", "", "", false
	}
}

// mergeSHAOf returns the tested merge SHA to status: the dedicated
// Event.MergeSHA field on the non-terminal EventTrialMerged/EventVerified
// (which carry no Record), falling back to the terminal event's own
// RunRecord.MergeSHA (EventRejected/EventError). "" when neither is
// present (no synthetic commit) — the caller then posts nothing.
func mergeSHAOf(ev core.Event) string {
	if ev.MergeSHA != "" {
		return ev.MergeSHA
	}
	if ev.Record != nil {
		return ev.Record.MergeSHA
	}
	return ""
}

// detailOf extracts the human-readable detail for a terminal event: the
// RunRecord's Detail, falling back to Event.Detail so a hand-built Event
// without a Record still produces a description instead of an empty one.
//
// Deliberately NOT the failing check's output: a live run showed the "first
// line of failing output" heuristic quoting runtime noise and go test's
// package-status lines instead of the assertion. A 140-rune status
// description says WHAT failed; the log/Slack tails, the dashboard run page,
// and the MCP run tool say WHY.
func detailOf(ev core.Event) string {
	if ev.Record != nil && ev.Record.Detail != "" {
		return ev.Record.Detail
	}
	return ev.Detail
}

// capDescription truncates s to at most descriptionCap runes.
func capDescription(s string) string {
	r := []rune(s)
	if len(r) <= descriptionCap {
		return s
	}
	return string(r[:descriptionCap])
}

// statusPayload is the JSON body of a commit-status POST.
type statusPayload struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
	Context     string `json:"context"`
}

// post sends one commit-status POST for ev, requesting a current
// credential per attempt. A 401 — the one response that clearly means
// "this token is expired or revoked" — invalidates the token and retries
// exactly once with a freshly minted one; every other failure (403
// permission, 5xx, network) surfaces immediately, never retried here.
func (c *Channel) post(ctx context.Context, ev core.Event, s state, description, sha string) error {
	payload := statusPayload{
		State:       string(s),
		TargetURL:   runURL(c.dashboardURL, ev.RunID),
		Description: description,
		Context:     "gauntlet/" + ev.Target,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal status: %w", err)
	}

	url := fmt.Sprintf("%s/repos/%s/%s/statuses/%s", c.apiURL, c.owner, c.repo, sha)

	tok, err := c.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("credential: %w", err)
	}
	code, status, err := c.doPost(ctx, url, body, tok)
	if err != nil {
		return err
	}
	if code == http.StatusUnauthorized {
		c.tokens.Invalidate(tok)
		tok, err = c.tokens.Token(ctx)
		if err != nil {
			return fmt.Errorf("credential after 401: %w", err)
		}
		code, status, err = c.doPost(ctx, url, body, tok)
		if err != nil {
			return err
		}
	}
	if code >= 300 {
		return fmt.Errorf("post %s: unexpected status %s", url, status)
	}
	return nil
}

// doPost performs one status POST attempt with the given token, returning
// the HTTP status. The response body is never quoted anywhere: GitHub
// error bodies are not needed for the log-and-drop diagnostic, and never
// reading them is the cheapest way to guarantee nothing secret-adjacent
// leaks into logs.
func (c *Channel) doPost(ctx context.Context, url string, body []byte, token string) (int, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Status, nil
}

// runURL builds target_url = "<base>/run/<runID>", or "" when base or runID
// is empty: omitted from the posted status unless a dashboard is
// configured; an empty runID would otherwise produce a dangling "<base>/run/"
// URL — defense in depth, since the queue always mints a RunID before
// EventTrialClean.
func runURL(base, runID string) string {
	if base == "" || runID == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/run/" + runID
}

// logf writes a log-and-drop line to the configured writer. Like
// LogChannel.Emit, a write failure here is not itself reported anywhere:
// losing a diagnostic line must never do anything worse than lose a
// diagnostic line.
func (c *Channel) logf(format string, args ...any) {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	fmt.Fprintf(c.log, format+"\n", args...)
}
