// Package ghstatus implements a core.Channel that posts one rollup GitHub
// commit status per target to the candidate SHA, via the plain REST
// "statuses" API (see docs/plans/phase23.md §4.3):
//
//	POST {api}/repos/{owner}/{repo}/statuses/{candidate_sha}
//
// It is output-only: Commands never yields (§9.5 — no phase-1 channel or
// this one produces a Command; ghstatus never will, either).
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

// Params configures a Channel. It is a package-local struct (§9.5): mapping
// from parsed config lives only in cmd, so this package never imports
// internal/config.
type Params struct {
	// Owner and Repo identify the GitHub repository (e.g. "acme" and
	// "widgets" for github.com/acme/widgets).
	Owner string
	Repo  string

	// Token is the PAT sent as "Authorization: token <Token>".
	Token string

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
	token        string
	apiURL       string
	dashboardURL string

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
	return &Channel{
		owner:        p.Owner,
		repo:         p.Repo,
		token:        p.Token,
		apiURL:       apiURL,
		dashboardURL: p.DashboardURL,
		client:       &http.Client{Timeout: requestTimeout},
		cmds:         make(chan core.Command),
		log:          logw,
	}
}

// Emit maps ev to a GitHub commit status per the table in
// docs/plans/phase23.md §4.3 and posts it. Events that don't map to a status
// (EventSkipped — transient; re-posts pending on the next trial — and any
// other non-terminal kind) post nothing.
//
// Emit never blocks the reconcile loop and never fails it: POST errors are
// written to the configured log writer and dropped, never returned, since a
// dropped status must never stop the queue. The error return exists only to
// satisfy core.Channel.
func (c *Channel) Emit(ctx context.Context, ev core.Event) error {
	state, description, ok := statusFor(ev)
	if !ok {
		return nil
	}
	if err := c.post(ctx, ev, state, description); err != nil {
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

// statusFor maps ev to the state and description to post, per the table in
// docs/plans/phase23.md §4.3. ok is false for event kinds that post nothing.
func statusFor(ev core.Event) (s state, description string, ok bool) {
	switch ev.Kind {
	case core.EventTrialClean:
		return statePending, "running checks", true
	case core.EventLanded:
		return stateSuccess, "landed", true
	case core.EventRejected:
		return stateFailure, capDescription(detailOf(ev)), true
	case core.EventTrialConflict:
		return stateFailure, "trial merge conflict", true
	case core.EventError:
		return stateError, capDescription(detailOf(ev)), true
	default:
		// core.EventSkipped and every non-terminal kind (Queued,
		// CheckStarted, CheckFinished): no post.
		return "", "", false
	}
}

// detailOf extracts the human-readable detail for a terminal event,
// preferring the carried RunRecord's Detail (the source the §4.3 table
// names) and falling back to Event.Detail so a hand-built Event without a
// Record still produces a description instead of an empty one.
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

// post sends one commit-status POST for ev.
func (c *Channel) post(ctx context.Context, ev core.Event, s state, description string) error {
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

	url := fmt.Sprintf("%s/repos/%s/%s/statuses/%s", c.apiURL, c.owner, c.repo, ev.Candidate.SHA)

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("post %s: unexpected status %s", url, resp.Status)
	}
	return nil
}

// runURL builds target_url = "<base>/run/<runID>", or "" when base is empty
// (§4.3: omitted from the posted status unless a dashboard is configured).
func runURL(base, runID string) string {
	if base == "" {
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
