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
)

const defaultDashboardURL = "http://localhost:8899"

// --- gauntlet status -----------------------------------------------------

// statusAPIResponse mirrors dashboard.statusResponse's JSON shape
// (internal/dashboard/api.go) — duplicated here deliberately; see the file
// doc.
type statusAPIResponse struct {
	SnapshotAt string            `json:"snapshotAt"`
	Targets    []statusAPITarget `json:"targets"`
}

type statusAPITarget struct {
	Name     string             `json:"name"`
	Branch   string             `json:"branch"`
	Tip      string             `json:"tip"`
	InFlight *statusAPIInFlight `json:"inFlight"`
	Waiting  []statusAPIWaiting `json:"waiting"`
	Parked   []statusAPIParked  `json:"parked"`
}

type statusAPIInFlight struct {
	Ref          string   `json:"ref"`
	SHA          string   `json:"sha"`
	RunID        string   `json:"runID"`
	CurrentCheck string   `json:"currentCheck"`
	StartedAt    string   `json:"startedAt"`
	ChecksDone   []string `json:"checksDone"`
}

type statusAPIWaiting struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
	Seq int64  `json:"seq"`
}

type statusAPIParked struct {
	Ref     string `json:"ref"`
	SHA     string `json:"sha"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
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
	return renderStatus(os.Stdout, resp, f.target)
}

// renderStatus writes a compact, per-target summary of resp to w: branch
// and short tip SHA, the in-flight ref and check (if any), the waiting
// count, and one line per parked ref with its outcome and reason. Pure
// (no I/O beyond w), so it's testable against canned API JSON without a
// network.
func renderStatus(w io.Writer, resp statusAPIResponse, target string) error {
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

		fmt.Fprintf(w, "  waiting: %d\n", len(t.Waiting))

		if len(t.Parked) > 0 {
			fmt.Fprintf(w, "  parked:\n")
			for _, p := range t.Parked {
				fmt.Fprintf(w, "    %s [%s]: %s\n", p.Ref, p.Outcome, p.Reason)
			}
		}
	}
	if shown == 0 && target != "" {
		fmt.Fprintf(w, "no such target: %s\n", target)
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
