// `gauntlet status`, `gauntlet retry`, `gauntlet cancel`, and
// `gauntlet hooks-cancel` are client-side porcelain, like `gauntlet land`
// (cmd/gauntlet/land.go): thin HTTP clients over the dashboard's JSON API
// (internal/dashboard/api.go, work chunk E4 plus Feature 1's cancel/
// hooks-cancel routes), for agents, scripts, and humans who don't want to
// open a browser. None talk to git, config, or the queue directly —
// everything they know comes from the API response. Kept intentionally thin:
// net/http + encoding/json only, no shared internal/dashboard types (so a
// change to the API's Go structs can't silently break the CLI's JSON
// decoding — they're coupled only through the wire format, which is the
// point of a JSON API).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultDashboardURL = "http://localhost:8080"

// --- gauntlet status -----------------------------------------------------

// statusAPIResponse mirrors dashboard.statusResponse's JSON shape
// (internal/dashboard/api.go) — duplicated here deliberately; see the file
// doc.
type statusAPIResponse struct {
	SnapshotAt string            `json:"snapshotAt"`
	Targets    []statusAPITarget `json:"targets"`

	// IgnoredRefs mirrors dashboard.statusResponse's own TOP-LEVEL,
	// daemon-wide field (S7c, internal/dashboard/api.go): recently pushed
	// refs whose target segment names NO configured target — which is why
	// they can't be scoped to any target object.
	IgnoredRefs []statusAPIIgnoredRef `json:"ignoredRefs"`

	// IdleSince mirrors dashboard.statusResponse's own TOP-LEVEL field: the
	// RFC3339 instant since which the whole daemon — every target's queue
	// and post-land hooks — has been idle, "" when the daemon is busy right
	// now.
	IdleSince string `json:"idleSince"`
}

type statusAPITarget struct {
	Name     string              `json:"name"`
	Branch   string              `json:"branch"`
	Tip      string              `json:"tip"`
	InFlight *statusAPIInFlight  `json:"inFlight"`
	Pipeline []statusAPIPipeline `json:"pipeline"`
	Waiting  []statusAPIWaiting  `json:"waiting"`
	Parked   []statusAPIParked   `json:"parked"`

	// LiveHook and HookRuns mirror dashboard.targetStatus's own additions
	// field-for-field (S5-surface, internal/dashboard/api.go) — see that
	// file's doc on why this is a separate type rather than a shared import.
	LiveHook *statusAPILiveHook `json:"liveHook"`
	HookRuns []statusAPIHookRun `json:"hookRuns"`
}

type statusAPILiveHook struct {
	Running      bool   `json:"running"`
	CurrentHook  string `json:"currentHook"`
	HookIndex    int    `json:"hookIndex"`
	HookCount    int    `json:"hookCount"`
	StartedAt    string `json:"startedAt"`
	BacklogDepth int    `json:"backlogDepth"`
}

type statusAPIHookRun struct {
	RunID      string `json:"runID"`
	OwedCount  int    `json:"owedCount"`
	DoneCount  int    `json:"doneCount"`
	StartedAt  string `json:"startedAt"`
	Skipped    bool   `json:"skipped"`
	SkipReason string `json:"skipReason"`
	Incomplete bool   `json:"incomplete"`
}

type statusAPIIgnoredRef struct {
	At     string `json:"at"`
	Target string `json:"target"` // the UNCONFIGURED name the ref's segment named
	Ref    string `json:"ref"`
	Detail string `json:"detail"`
}

type statusAPIInFlight struct {
	Ref          string   `json:"ref"`
	SHA          string   `json:"sha"`
	RunID        string   `json:"runID"`
	CurrentCheck string   `json:"currentCheck"`
	StartedAt    string   `json:"startedAt"`
	ChecksDone   []string `json:"checksDone"`
}

// statusAPIPipeline mirrors dashboard.pipelineStatus's JSON shape
// (internal/dashboard/api.go) — see the file doc on why this is a separate
// type rather than a shared import. One entry per run currently in a
// target's pipeline: head first, additive alongside inFlight (which stays
// the head run, for back-compat) — a target running more than one
// speculative lane, or a batch of more than one member, only shows up
// here.
type statusAPIPipeline struct {
	Members      []statusAPIPipelineMember `json:"members"`
	ChainTip     string                    `json:"chainTip"`
	Predicted    bool                      `json:"predicted"`
	BatchID      string                    `json:"batchId"`
	ChecksDone   []string                  `json:"checksDone"`
	CurrentCheck string                    `json:"currentCheck"`
	StartedAt    string                    `json:"startedAt"`
}

type statusAPIPipelineMember struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type statusAPIWaiting struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
	Seq int64  `json:"seq"`
}

// statusAPIServicesResponse mirrors dashboard.servicesResponse's JSON shape
// (internal/dashboard/api.go) — duplicated here deliberately, like every
// other statusAPI* type in this file. Unlike everything above, this is
// fetched from a SEPARATE endpoint (GET /api/v1/services, not embedded in
// /api/v1/status): the shared-services pool is its own daemon-global
// resource (design §10's tuning instrument), not something the queue
// snapshot carries.
type statusAPIServicesResponse struct {
	MaxInstances int                    `json:"maxInstances"`
	Pending      int                    `json:"pending"`
	Instances    []statusAPIServiceInst `json:"instances"`
}

type statusAPIServiceInst struct {
	Service   string `json:"service"`
	Image     string `json:"image"`
	Key       string `json:"key"`
	KeyHash12 string `json:"keyHash12"`
	Mode      string `json:"mode"`
	Host      string `json:"host"`
	Port      string `json:"port"`
	CreatedAt string `json:"createdAt"`
	LastUsed  string `json:"lastUsed"`
	Refcount  int    `json:"refcount"`
	Hits      int    `json:"hits"`
}

type statusAPIParked struct {
	Ref     string `json:"ref"`
	SHA     string `json:"sha"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
	// RunID is the run that parked this candidate, mirroring
	// dashboard.parkedStatus.RunID (internal/dashboard/api.go) — "" for a
	// boot-seeded park predating this field, in which case renderStatus
	// omits it rather than printing an empty run= token.
	RunID string `json:"runId"`
}

type statusFlags struct {
	url    string
	target string
	json   bool
}

// parseStatusFlags parses "gauntlet status"'s flags. flag.ContinueOnError
// (rather than land.go's ExitOnError) so tests can exercise bad-flag paths
// without exiting the test binary; runStatus turns a parse error into the
// same "print to stderr, exit 1" behavior main's dispatch already gives
// every subcommand.
func parseStatusFlags(args []string) (statusFlags, error) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var f statusFlags
	fs.StringVar(&f.url, "url", defaultDashboardURL, "dashboard base URL")
	fs.StringVar(&f.target, "target", "", "only show this target")
	fs.BoolVar(&f.json, "json", false, "print the raw API response instead of a human summary")
	if err := fs.Parse(args); err != nil {
		return statusFlags{}, err
	}
	return f, nil
}

// runStatus implements "gauntlet status": GET the dashboard's
// /api/v1/status and either print it verbatim (-json) or render a compact
// human summary.
func runStatus(args []string) error {
	f, err := parseStatusFlags(args)
	if err != nil {
		return err
	}

	body, err := httpGetBody(f.url + "/api/v1/status")
	if err != nil {
		return err
	}

	if f.json {
		_, err := os.Stdout.Write(body)
		if err == nil {
			fmt.Fprintln(os.Stdout)
		}
		return err
	}

	var resp statusAPIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode /api/v1/status response: %w", err)
	}

	// The shared-services pool (design §10's tuning instrument) lives at its
	// own endpoint, fetched best-effort: a 503 (no services configured for
	// this daemon) or any other failure just omits the section renderStatus
	// prints below — never fails the whole `gauntlet status` command over an
	// optional, daemon-global extra.
	var svc *statusAPIServicesResponse
	if svcBody, err := httpGetBody(f.url + "/api/v1/services"); err == nil {
		var parsed statusAPIServicesResponse
		if err := json.Unmarshal(svcBody, &parsed); err == nil {
			svc = &parsed
		}
	}

	return renderStatus(os.Stdout, resp, f.target, svc)
}

// renderStatus writes a compact, per-target summary of resp to w: branch
// and short tip SHA, the in-flight ref and check (if any), the pipeline
// (one line per run, position + ref/topic + a "(speculated)" marker on a
// predicted-base run), the waiting count, and one line per parked ref with
// its outcome and reason. Pure (no I/O beyond w), so it's testable against
// canned API JSON without a network.
//
// svc is the shared-services pool's own separately-fetched response (design
// §10's tuning instrument, runStatus's doc) — nil when services aren't
// configured for this daemon or the fetch failed, in which case the section
// below is omitted entirely, same convention as every other optional section
// here.
func renderStatus(w io.Writer, resp statusAPIResponse, target string, svc *statusAPIServicesResponse) error {
	shown := 0
	for _, t := range resp.Targets {
		if target != "" && t.Name != target {
			continue
		}
		shown++

		fmt.Fprintf(w, "%s (%s) tip=%s\n", t.Name, t.Branch, shortSHA(t.Tip))

		if t.InFlight != nil {
			fmt.Fprintf(w, "  in-flight: %s check=%s\n", t.InFlight.Ref, orDash(t.InFlight.CurrentCheck))
		} else {
			fmt.Fprintf(w, "  in-flight: -\n")
		}

		if len(t.Pipeline) > 1 || (len(t.Pipeline) == 1 && len(t.Pipeline[0].Members) > 1) {
			// Only worth a separate section once there's something inFlight
			// alone can't already show: more than one run in flight
			// (speculation), or a single run with more than one member
			// (batch) — matching the dashboard target page's own
			// (len(Pipeline) > 1 || len(Pipeline[0].Members) > 1) gate
			// (internal/dashboard/server.go).
			fmt.Fprintf(w, "  pipeline:\n")
			for i, p := range t.Pipeline {
				refs := make([]string, len(p.Members))
				for j, m := range p.Members {
					refs[j] = m.Ref
				}
				line := strings.Join(refs, ", ")
				if line == "" {
					line = "-"
				}
				if p.Predicted {
					line += " (speculated)"
				}
				fmt.Fprintf(w, "    #%d %s\n", i, line)
			}
		}

		fmt.Fprintf(w, "  waiting: %d\n", len(t.Waiting))

		if len(t.Parked) > 0 {
			fmt.Fprintf(w, "  parked:\n")
			for _, p := range t.Parked {
				fmt.Fprintf(w, "    %s [%s]: %s%s\n", p.Ref, p.Outcome, p.Reason, parkedSuffix(p.RunID, p.At))
			}
		}

		// Post-land hooks (S5-surface): live progress first (only when a
		// hook is actually running right now), then the durable owed/done
		// ledger — a crash-incomplete or recovery-skipped landing is
		// visible here the same way the dashboard/MCP surfaces it.
		if t.LiveHook != nil && t.LiveHook.Running {
			fmt.Fprintf(w, "  hooks: running %s (%d/%d)\n", t.LiveHook.CurrentHook, t.LiveHook.HookIndex, t.LiveHook.HookCount)
		}
		if len(t.HookRuns) > 0 {
			fmt.Fprintf(w, "  hook runs:\n")
			for _, hr := range t.HookRuns {
				status := "complete"
				switch {
				case hr.Skipped:
					status = "skipped: " + hr.SkipReason
				case hr.Incomplete:
					status = "crash-incomplete"
				}
				fmt.Fprintf(w, "    %s owed=%d done=%d [%s]\n", hr.RunID, hr.OwedCount, hr.DoneCount, status)
			}
		}

	}
	if shown == 0 && target != "" {
		fmt.Fprintf(w, "no such target: %s\n", target)
	}

	// Recently ignored refs (S7c): a DAEMON-level section, rendered once at
	// the end — an ignored ref's target segment names no configured target
	// (that's why it was ignored), so it belongs to no target above. Printed
	// regardless of any -target filter: a misconfiguration is exactly the
	// kind of thing a filtered view shouldn't hide.
	if len(resp.IgnoredRefs) > 0 {
		fmt.Fprintf(w, "ignored refs (no configured target):\n")
		for _, ir := range resp.IgnoredRefs {
			fmt.Fprintf(w, "  %s: %s\n", ir.Ref, ir.Detail)
		}
	}

	// Shared-services pool (design §10's tuning instrument): another
	// DAEMON-level section, rendered once at the end regardless of any
	// -target filter, same reasoning as ignored refs above — the pool isn't
	// scoped to any one target either. Omitted entirely when svc is nil
	// (services disabled, or the separate /api/v1/services fetch failed).
	if svc != nil {
		fmt.Fprintf(w, "services (max=%d pending=%d):\n", svc.MaxInstances, svc.Pending)
		if len(svc.Instances) == 0 {
			fmt.Fprintf(w, "  none live\n")
		}
		for _, inst := range svc.Instances {
			fmt.Fprintf(w, "  %s [%s] %s:%s refs=%d hits=%d (created=%s last-used=%s)\n",
				inst.Service, inst.KeyHash12, inst.Host, inst.Port, inst.Refcount, inst.Hits, inst.CreatedAt, inst.LastUsed)
		}
	}

	// Idle signal: a DAEMON-level line, printed once at the very end
	// regardless of any -target filter, same convention as ignored
	// refs/services above — omitted entirely when the daemon isn't idle
	// right now (resp.IdleSince == "").
	if resp.IdleSince != "" {
		fmt.Fprintf(w, "idle since %s\n", resp.IdleSince)
	}
	return nil
}

func shortSHA(sha string) string {
	const n = 8
	if sha == "" {
		return "-"
	}
	if len(sha) > n {
		return sha[:n]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// parkedSuffix builds the trailing "(run=... at=...)" a parked line appends
// (renderStatus), key=value style matching the hook-runs section's
// owed=/done= tokens just below it. Either half can be legitimately absent —
// runID is "" for a boot-seeded park predating that field (see
// statusAPIParked.RunID's doc), at is "" only defensively (a live park
// always records one) — so each token is included only when its value is
// non-empty, and the whole suffix is "" (not "()") when both are, rather
// than printing an empty/misleading parenthetical.
func parkedSuffix(runID, at string) string {
	var parts []string
	if runID != "" {
		parts = append(parts, "run="+runID)
	}
	if at != "" {
		parts = append(parts, "at="+at)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " ") + ")"
}

// --- gauntlet retry -------------------------------------------------------

type retryFlags struct {
	url    string
	target string
	ref    string
}

func parseRetryFlags(args []string) (retryFlags, error) {
	fs := flag.NewFlagSet("retry", flag.ContinueOnError)
	var f retryFlags
	fs.StringVar(&f.url, "url", defaultDashboardURL, "dashboard base URL")
	fs.StringVar(&f.target, "target", "", "target name, matching a `target` in the daemon's gauntlet.kdl [required]")
	fs.StringVar(&f.ref, "ref", "", "candidate ref, e.g. refs/heads/for/main/alice/topic [required]")
	if err := fs.Parse(args); err != nil {
		return retryFlags{}, err
	}
	if f.target == "" {
		return retryFlags{}, fmt.Errorf("-target is required")
	}
	if f.ref == "" {
		return retryFlags{}, fmt.Errorf("-ref is required")
	}
	return f, nil
}

// retryRequestBody mirrors dashboard.retryRequest's JSON shape
// (internal/dashboard/api.go) — see the file doc on why this is a separate
// type rather than a shared import.
type retryRequestBody struct {
	Target string `json:"target"`
	Ref    string `json:"ref"`
}

// runRetry implements "gauntlet retry": POST the dashboard's
// /api/v1/retry with {target, ref}, re-queuing a parked candidate at its
// current SHA without a new push.
func runRetry(args []string) error {
	f, err := parseRetryFlags(args)
	if err != nil {
		return err
	}

	reqBody, err := json.Marshal(retryRequestBody{Target: f.target, Ref: f.ref})
	if err != nil {
		return err
	}

	res, err := http.Post(f.url+"/api/v1/retry", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", res.Status, bytes.TrimSpace(body))
	}
	fmt.Fprintln(os.Stdout, string(bytes.TrimSpace(body)))
	return nil
}

// --- gauntlet cancel ------------------------------------------------------

type cancelFlags struct {
	url    string
	target string
	ref    string
}

func parseCancelFlags(args []string) (cancelFlags, error) {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	var f cancelFlags
	fs.StringVar(&f.url, "url", defaultDashboardURL, "dashboard base URL")
	fs.StringVar(&f.target, "target", "", "target name, matching a `target` in the daemon's gauntlet.kdl [required]")
	fs.StringVar(&f.ref, "ref", "", "candidate ref, e.g. refs/heads/for/main/alice/topic [required]")
	if err := fs.Parse(args); err != nil {
		return cancelFlags{}, err
	}
	if f.target == "" {
		return cancelFlags{}, fmt.Errorf("-target is required")
	}
	if f.ref == "" {
		return cancelFlags{}, fmt.Errorf("-ref is required")
	}
	return f, nil
}

// cancelRequestBody mirrors dashboard.cancelRequest's JSON shape
// (internal/dashboard/api.go) — see retryRequestBody's doc on why this is a
// separate type rather than a shared import.
type cancelRequestBody struct {
	Target string `json:"target"`
	Ref    string `json:"ref"`
}

// runCancel implements "gauntlet cancel": POST the dashboard's
// /api/v1/cancel with {target, ref} — stops whatever is currently
// happening to that candidate and parks it at its current SHA (Feature 1,
// manual operator cancellation; see "Cancellation" in the README).
func runCancel(args []string) error {
	f, err := parseCancelFlags(args)
	if err != nil {
		return err
	}

	reqBody, err := json.Marshal(cancelRequestBody{Target: f.target, Ref: f.ref})
	if err != nil {
		return err
	}

	res, err := http.Post(f.url+"/api/v1/cancel", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", res.Status, bytes.TrimSpace(body))
	}
	fmt.Fprintln(os.Stdout, string(bytes.TrimSpace(body)))
	return nil
}

// --- gauntlet hooks-cancel --------------------------------------------------

type hooksCancelFlags struct {
	url    string
	target string
}

func parseHooksCancelFlags(args []string) (hooksCancelFlags, error) {
	fs := flag.NewFlagSet("hooks-cancel", flag.ContinueOnError)
	var f hooksCancelFlags
	fs.StringVar(&f.url, "url", defaultDashboardURL, "dashboard base URL")
	fs.StringVar(&f.target, "target", "", "target name, matching a `target` in the daemon's gauntlet.kdl [required]")
	if err := fs.Parse(args); err != nil {
		return hooksCancelFlags{}, err
	}
	if f.target == "" {
		return hooksCancelFlags{}, fmt.Errorf("-target is required")
	}
	return f, nil
}

// hooksCancelRequestBody mirrors dashboard.hookCancelRequest's JSON shape
// (internal/dashboard/api.go).
type hooksCancelRequestBody struct {
	Target string `json:"target"`
}

// runHooksCancel implements "gauntlet hooks-cancel": POST the dashboard's
// /api/v1/hooks/cancel with {target} — cancels that target's currently
// running post-land hook execution, if any (Feature 1's hook-cancel
// surface, hooks.Runner.CancelCurrent).
func runHooksCancel(args []string) error {
	f, err := parseHooksCancelFlags(args)
	if err != nil {
		return err
	}

	reqBody, err := json.Marshal(hooksCancelRequestBody{Target: f.target})
	if err != nil {
		return err
	}

	res, err := http.Post(f.url+"/api/v1/hooks/cancel", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", res.Status, bytes.TrimSpace(body))
	}
	fmt.Fprintln(os.Stdout, string(bytes.TrimSpace(body)))
	return nil
}

// --- gauntlet drain ---------------------------------------------------------

type drainFlags struct {
	url      string
	deadline time.Duration
	wait     bool
}

func parseDrainFlags(args []string) (drainFlags, error) {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	var f drainFlags
	fs.StringVar(&f.url, "url", defaultDashboardURL, "dashboard base URL")
	fs.DurationVar(&f.deadline, "deadline", 0, "force the immediate kill this long from now if the drain hasn't finished (0 = no deadline)")
	fs.BoolVar(&f.wait, "wait", false, "block until the daemon reports lifecycle=drained")
	if err := fs.Parse(args); err != nil {
		return drainFlags{}, err
	}
	return f, nil
}

// drainRequestBody mirrors dashboard.drainRequest's JSON shape.
type drainRequestBody struct {
	Deadline string `json:"deadline,omitempty"`
}

// statusLifecycle is the subset of GET /api/v1/status the drain client
// reads to follow the lifecycle transition.
type statusLifecycle struct {
	Lifecycle    string `json:"lifecycle"`
	ActiveRuns   int    `json:"activeRuns"`
	ActiveChecks int    `json:"activeChecks"`
}

// runDrain implements "gauntlet drain": POST /api/v1/drain to begin a
// graceful shutdown drain, optionally waiting until the daemon reports
// lifecycle=drained. A connection failure surfaces as a clear error rather
// than pretending a drain began (the CLI is a thin client; a daemon with
// no dashboard bind simply isn't reachable this way — use a signal).
func runDrain(args []string) error {
	f, err := parseDrainFlags(args)
	if err != nil {
		return err
	}

	body := drainRequestBody{}
	if f.deadline > 0 {
		body.Deadline = time.Now().Add(f.deadline).UTC().Format(time.RFC3339)
	}
	reqBody, err := json.Marshal(body)
	if err != nil {
		return err
	}
	res, err := http.Post(f.url+"/api/v1/drain", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("no reachable admin endpoint at %s (%w); a daemon without a dashboard bind drains by signal only", f.url, err)
	}
	defer res.Body.Close()
	respBody, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", res.Status, bytes.TrimSpace(respBody))
	}
	fmt.Fprintln(os.Stdout, string(bytes.TrimSpace(respBody)))
	if !f.wait {
		return nil
	}

	// Poll the lifecycle until it reaches "drained". Once drained, the
	// daemon exits and the status endpoint stops answering — a connection
	// failure AFTER we have seen draining is itself the completion signal.
	seenDraining := false
	for {
		raw, err := httpGetBody(f.url + "/api/v1/status")
		if err != nil {
			if seenDraining {
				fmt.Fprintln(os.Stdout, "drained")
				return nil
			}
			return err
		}
		var s statusLifecycle
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		switch s.Lifecycle {
		case "drained":
			fmt.Fprintln(os.Stdout, "drained")
			return nil
		case "draining":
			seenDraining = true
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// --- shared HTTP helper ----------------------------------------------------

// httpGetBody GETs url and returns its body, treating any non-2xx status as
// an error (the body — a JSON `{"error": "..."}` from the dashboard API —
// is included in the error message).
func httpGetBody(url string) ([]byte, error) {
	res, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: %s: %s", url, res.Status, bytes.TrimSpace(body))
	}
	return body, nil
}
