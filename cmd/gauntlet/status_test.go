package main

// Tests for gauntlet status/retry's pure pieces: flag parsing and
// renderStatus against canned API JSON. No network, no exec — matching
// land_test.go's "argv construction only" style (docs/plans/phase23.md §6
// chunk D8 / E4).

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseStatusFlags_Defaults(t *testing.T) {
	f, err := parseStatusFlags(nil)
	if err != nil {
		t.Fatalf("parseStatusFlags(nil): %v", err)
	}
	if f.url != defaultDashboardURL {
		t.Errorf("url = %q, want %q", f.url, defaultDashboardURL)
	}
	if f.target != "" {
		t.Errorf("target = %q, want empty", f.target)
	}
	if f.json {
		t.Errorf("json = true, want false")
	}
}

func TestParseStatusFlags_Overrides(t *testing.T) {
	f, err := parseStatusFlags([]string{"-url", "http://example:1234", "-target", "main", "-json"})
	if err != nil {
		t.Fatalf("parseStatusFlags: %v", err)
	}
	if f.url != "http://example:1234" {
		t.Errorf("url = %q", f.url)
	}
	if f.target != "main" {
		t.Errorf("target = %q", f.target)
	}
	if !f.json {
		t.Errorf("json = false, want true")
	}
}

func TestParseStatusFlags_UnknownFlagErrors(t *testing.T) {
	if _, err := parseStatusFlags([]string{"-bogus"}); err == nil {
		t.Errorf("expected error for unknown flag")
	}
}

func TestParseRetryFlags_RequiresTargetAndRef(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"neither", nil},
		{"target only", []string{"-target", "main"}},
		{"ref only", []string{"-ref", "refs/heads/for/main/alice/topic"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseRetryFlags(c.args); err == nil {
				t.Errorf("expected error, got none")
			}
		})
	}
}

func TestParseRetryFlags_Valid(t *testing.T) {
	f, err := parseRetryFlags([]string{
		"-url", "http://example:1234",
		"-target", "main",
		"-ref", "refs/heads/for/main/alice/topic",
	})
	if err != nil {
		t.Fatalf("parseRetryFlags: %v", err)
	}
	if f.url != "http://example:1234" || f.target != "main" || f.ref != "refs/heads/for/main/alice/topic" {
		t.Errorf("f = %+v", f)
	}
}

func TestParseRetryFlags_DefaultURL(t *testing.T) {
	f, err := parseRetryFlags([]string{"-target", "main", "-ref", "refs/heads/for/main/alice/topic"})
	if err != nil {
		t.Fatalf("parseRetryFlags: %v", err)
	}
	if f.url != defaultDashboardURL {
		t.Errorf("url = %q, want %q", f.url, defaultDashboardURL)
	}
}

// --- gauntlet cancel / hooks-cancel flag parsing (Feature 1) ---------------
// Mirrors the retry flag tests above exactly: same shape, same defaults.

func TestParseCancelFlags_RequiresTargetAndRef(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"neither", nil},
		{"target only", []string{"-target", "main"}},
		{"ref only", []string{"-ref", "refs/heads/for/main/alice/topic"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseCancelFlags(c.args); err == nil {
				t.Errorf("expected error, got none")
			}
		})
	}
}

func TestParseCancelFlags_Valid(t *testing.T) {
	f, err := parseCancelFlags([]string{
		"-url", "http://example:1234",
		"-target", "main",
		"-ref", "refs/heads/for/main/alice/topic",
	})
	if err != nil {
		t.Fatalf("parseCancelFlags: %v", err)
	}
	if f.url != "http://example:1234" || f.target != "main" || f.ref != "refs/heads/for/main/alice/topic" {
		t.Errorf("f = %+v", f)
	}
}

func TestParseCancelFlags_DefaultURL(t *testing.T) {
	f, err := parseCancelFlags([]string{"-target", "main", "-ref", "refs/heads/for/main/alice/topic"})
	if err != nil {
		t.Fatalf("parseCancelFlags: %v", err)
	}
	if f.url != defaultDashboardURL {
		t.Errorf("url = %q, want %q", f.url, defaultDashboardURL)
	}
}

func TestParseHooksCancelFlags_RequiresTarget(t *testing.T) {
	if _, err := parseHooksCancelFlags(nil); err == nil {
		t.Errorf("expected error, got none")
	}
}

func TestParseHooksCancelFlags_Valid(t *testing.T) {
	f, err := parseHooksCancelFlags([]string{"-url", "http://example:1234", "-target", "main"})
	if err != nil {
		t.Fatalf("parseHooksCancelFlags: %v", err)
	}
	if f.url != "http://example:1234" || f.target != "main" {
		t.Errorf("f = %+v", f)
	}
}

func TestParseHooksCancelFlags_DefaultURL(t *testing.T) {
	f, err := parseHooksCancelFlags([]string{"-target", "main"})
	if err != nil {
		t.Fatalf("parseHooksCancelFlags: %v", err)
	}
	if f.url != defaultDashboardURL {
		t.Errorf("url = %q, want %q", f.url, defaultDashboardURL)
	}
}

func TestShortSHA(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "-"},
		{"abcd", "abcd"},
		{"1234567890abcdef", "12345678"},
	}
	for _, c := range cases {
		if got := shortSHA(c.in); got != c.want {
			t.Errorf("shortSHA(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOrDash(t *testing.T) {
	if got := orDash(""); got != "-" {
		t.Errorf("orDash(\"\") = %q", got)
	}
	if got := orDash("test"); got != "test" {
		t.Errorf("orDash(\"test\") = %q", got)
	}
}

// canned is a hand-built API response covering both an in-flight target
// and an idle one, mirroring the shape internal/dashboard/api_test.go's
// testSnapshot produces.
const canned = `{
	"snapshotAt": "2026-07-05T12:00:00Z",
	"targets": [
		{
			"name": "main",
			"branch": "main",
			"tip": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"inFlight": {
				"ref": "refs/heads/for/main/alice/feat-a",
				"sha": "1111111111111111111111111111111111111111",
				"runID": "run-inflight-1",
				"currentCheck": "test",
				"startedAt": "2026-07-05T11:59:30Z",
				"checksDone": ["lint"]
			},
			"waiting": [
				{"ref": "refs/heads/for/main/carol/first", "sha": "3333333333333333333333333333333333333333", "seq": 2},
				{"ref": "refs/heads/for/main/bob/second", "sha": "2222222222222222222222222222222222222222", "seq": 5}
			],
			"parked": [
				{"ref": "refs/heads/for/main/mallory/evil", "sha": "4444444444444444444444444444444444444444", "outcome": "rejected", "reason": "build failed", "at": "2026-07-05T11:00:00Z", "runId": "run-mallory-1"},
				{"ref": "refs/heads/for/main/dave/legacy", "sha": "5555555555555555555555555555555555555555", "outcome": "conflict", "reason": "stale seed", "at": "2026-07-05T10:00:00Z"}
			]
		},
		{
			"name": "release",
			"branch": "release/v2",
			"tip": "",
			"inFlight": null,
			"waiting": [],
			"parked": []
		}
	]
}`

func decodeCanned(t *testing.T) statusAPIResponse {
	t.Helper()
	var resp statusAPIResponse
	if err := json.Unmarshal([]byte(canned), &resp); err != nil {
		t.Fatalf("decode canned JSON: %v", err)
	}
	return resp
}

func TestRenderStatus_CompactSummary(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"main (main) tip=aaaaaaaa",
		"in-flight: refs/heads/for/main/alice/feat-a check=test",
		"waiting: 2",
		// mallory's park carries a runId: both run= and at= render.
		"refs/heads/for/main/mallory/evil [rejected]: build failed (run=run-mallory-1 at=2026-07-05T11:00:00Z)",
		// dave's park has no runId (a boot-seeded park predating that
		// field): the run= token is omitted entirely, not printed empty —
		// at= still renders on its own.
		"refs/heads/for/main/dave/legacy [conflict]: stale seed (at=2026-07-05T10:00:00Z)",
		"release (release/v2) tip=-",
		"in-flight: -",
		"waiting: 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, out)
		}
	}
	if strings.Contains(out, "run=)") || strings.Contains(out, "()") {
		t.Errorf("output contains an empty run=/at= parenthetical:\n%s", out)
	}
}

func TestRenderStatus_FiltersByTarget(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "release", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, "main (main)") {
		t.Errorf("expected main filtered out:\n%s", out)
	}
	if !strings.Contains(out, "release (release/v2)") {
		t.Errorf("expected release target present:\n%s", out)
	}
}

func TestRenderStatus_UnknownTargetNotice(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "does-not-exist", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "no such target: does-not-exist") {
		t.Errorf("output = %q", buf.String())
	}
}

// hooksCanned is a canned API response with a target carrying a running
// live hook and a durable hook-run ledger (one crash-incomplete, one
// skipped), plus a TOP-LEVEL recently-ignored ref (S5-surface, S7c: ignored
// refs are daemon-wide, not per-target — their target segment names no
// configured target) — the shape dashboard/api.go's statusResponse carries
// (canned here directly, since renderStatus is tested against hand-built
// JSON, not a live dashboard).
const hooksCanned = `{
	"snapshotAt": "2026-07-05T12:00:00Z",
	"targets": [
		{
			"name": "main",
			"branch": "main",
			"tip": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"inFlight": null,
			"waiting": [],
			"parked": [],
			"liveHook": {
				"running": true,
				"currentHook": "deploy",
				"hookIndex": 1,
				"hookCount": 2,
				"startedAt": "2026-07-05T11:59:00Z",
				"backlogDepth": 1
			},
			"hookRuns": [
				{"runID": "run-a", "owedCount": 2, "doneCount": 1, "startedAt": "2026-07-05T11:00:00Z", "skipped": false, "skipReason": "", "incomplete": true},
				{"runID": "run-b", "owedCount": 3, "doneCount": 3, "startedAt": "2026-07-05T10:00:00Z", "skipped": true, "skipReason": "recovered landing; hooks not run", "incomplete": false}
			]
		}
	],
	"ignoredRefs": [
		{"at": "2026-07-05T09:00:00Z", "target": "unknown", "ref": "refs/heads/for/unknown/alice/typo", "detail": "target \"unknown\" is not configured"}
	]
}`

// TestRenderStatus_HookFieldsRendered confirms renderStatus prints the
// live-hook progress line, the durable hook-run ledger (crash-incomplete and
// skipped rows distinguished), and the daemon-level recently-ignored refs
// section at the end (S5-surface, S7c; mirrors
// TestRenderStatus_PipelineRendersPositionRefAndSpeculatedMarker's
// canned-JSON approach for S10).
func TestRenderStatus_HookFieldsRendered(t *testing.T) {
	var resp statusAPIResponse
	if err := json.Unmarshal([]byte(hooksCanned), &resp); err != nil {
		t.Fatalf("decode hooksCanned JSON: %v", err)
	}

	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"hooks: running deploy (1/2)",
		"hook runs:",
		"run-a owed=2 done=1 [crash-incomplete]",
		"run-b owed=3 done=3 [skipped: recovered landing; hooks not run]",
		"ignored refs (no configured target):",
		`refs/heads/for/unknown/alice/typo: target "unknown" is not configured`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, out)
		}
	}
}

// TestRenderStatus_IgnoredRefsSurviveTargetFilter confirms the daemon-level
// ignored-refs section still renders under a -target filter — a
// misconfiguration is exactly what a filtered view shouldn't hide, and the
// refs belong to no configured target anyway.
func TestRenderStatus_IgnoredRefsSurviveTargetFilter(t *testing.T) {
	var resp statusAPIResponse
	if err := json.Unmarshal([]byte(hooksCanned), &resp); err != nil {
		t.Fatalf("decode hooksCanned JSON: %v", err)
	}

	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "main", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "ignored refs (no configured target):") {
		t.Errorf("filtered output missing the daemon-level ignored-refs section:\n%s", buf.String())
	}
}

// TestRenderStatus_NoHookFieldsOmitsSections confirms a target with no
// liveHook/hookRuns/ignoredRefs (the existing "canned" fixture, decoded
// before this chunk added these fields) renders none of the new sections —
// pure additive change, no regression for a target that has nothing to show.
func TestRenderStatus_NoHookFieldsOmitsSections(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "main", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()
	for _, absent := range []string{"hooks: running", "hook runs:", "ignored refs"} {
		if strings.Contains(out, absent) {
			t.Errorf("output unexpectedly contains %q:\n%s", absent, out)
		}
	}
}

func TestRenderStatus_NoParkedOmitsSection(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "release", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if strings.Contains(buf.String(), "parked:") {
		t.Errorf("expected no parked section for release:\n%s", buf.String())
	}
}

// pipelineCanned is a canned API response with a target running a
// multi-lane speculative pipeline (one predicted-base run, one on the live
// tip) — the exact shape statusAPITarget previously dropped entirely
// (S10), since it had no Pipeline field to unmarshal into.
const pipelineCanned = `{
	"snapshotAt": "2026-07-05T12:00:00Z",
	"targets": [
		{
			"name": "main",
			"branch": "main",
			"tip": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"inFlight": {
				"ref": "refs/heads/for/main/alice/feat-a",
				"sha": "1111111111111111111111111111111111111111",
				"runID": "run-inflight-1",
				"currentCheck": "test",
				"startedAt": "2026-07-05T11:59:30Z",
				"checksDone": []
			},
			"pipeline": [
				{
					"members": [{"ref": "refs/heads/for/main/alice/feat-a", "sha": "1111111111111111111111111111111111111111"}],
					"chainTip": "1111111111111111111111111111111111111111",
					"predicted": false,
					"batchId": "",
					"checksDone": [],
					"currentCheck": "test",
					"startedAt": "2026-07-05T11:59:30Z"
				},
				{
					"members": [{"ref": "refs/heads/for/main/bob/feat-b", "sha": "2222222222222222222222222222222222222222"}],
					"chainTip": "2222222222222222222222222222222222222222",
					"predicted": true,
					"batchId": "",
					"checksDone": [],
					"currentCheck": "",
					"startedAt": "2026-07-05T11:59:45Z"
				}
			],
			"waiting": [],
			"parked": []
		}
	]
}`

// TestRenderStatus_PipelineRendersPositionRefAndSpeculatedMarker is S10's
// regression test: gauntlet status's human renderer must not silently drop
// the API's pipeline array — a target running more than one speculative
// lane must render every lane (position, ref, and a "(speculated)" marker
// on the predicted-base one), not just the single head-run inFlight line.
func TestRenderStatus_PipelineRendersPositionRefAndSpeculatedMarker(t *testing.T) {
	var resp statusAPIResponse
	if err := json.Unmarshal([]byte(pipelineCanned), &resp); err != nil {
		t.Fatalf("decode pipelineCanned JSON: %v", err)
	}

	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"pipeline:",
		"#0 refs/heads/for/main/alice/feat-a",
		"#1 refs/heads/for/main/bob/feat-b (speculated)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, out)
		}
	}
	if strings.Contains(out, "#0 refs/heads/for/main/alice/feat-a (speculated)") {
		t.Errorf("head run must not carry the speculated marker:\n%s", out)
	}
}

// TestRenderStatus_SingleMemberPipelineOmitsSection confirms the ordinary
// (non-speculative, non-batch) case — one pipeline entry with exactly one
// member, mirroring InFlight — doesn't grow a redundant pipeline section:
// renderStatus's gate matches the dashboard target page's own
// (len(Pipeline) > 1 || len(Pipeline[0].Members) > 1) condition.
func TestRenderStatus_SingleMemberPipelineOmitsSection(t *testing.T) {
	resp := decodeCanned(t) // canned's "main" target has no "pipeline" key at all
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "main", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if strings.Contains(buf.String(), "pipeline:") {
		t.Errorf("expected no pipeline section without a multi-run/multi-member pipeline:\n%s", buf.String())
	}
}

// --- services section (design §10's tuning instrument) -----------------------

// testServicesResponse builds a small statusAPIServicesResponse fixture for
// the services-section renderStatus tests below.
func testServicesResponse() *statusAPIServicesResponse {
	return &statusAPIServicesResponse{
		MaxInstances: 4,
		Pending:      1,
		Instances: []statusAPIServiceInst{
			{
				Service: "pg", Image: "postgres:16",
				Key: "abcdef0123456789fullkey", KeyHash12: "abcdef012345",
				Mode: "network", Host: "abcdef012345", Port: "5432",
				CreatedAt: "2026-07-05T10:00:00Z", LastUsed: "2026-07-05T11:55:00Z",
				Refcount: 2, Hits: 7,
			},
		},
	}
}

// TestRenderStatus_ServicesSectionRendered confirms renderStatus prints the
// shared-services pool's tuning knobs and one line per live instance
// (mirroring hook-runs' rendering) when svc is non-nil.
func TestRenderStatus_ServicesSectionRendered(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "", testServicesResponse()); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"services (max=4 pending=1):",
		"pg [abcdef012345] abcdef012345:5432 refs=2 hits=7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, out)
		}
	}
}

// TestRenderStatus_ServicesSectionOmittedWhenNil confirms renderStatus omits
// the services section entirely when svc is nil — services aren't
// configured for this daemon, or the CLI's separate /api/v1/services fetch
// failed (runStatus's best-effort doc).
func TestRenderStatus_ServicesSectionOmittedWhenNil(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "", nil); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if strings.Contains(buf.String(), "services (") {
		t.Errorf("expected no services section when svc is nil:\n%s", buf.String())
	}
}

// TestRenderStatus_ServicesSectionEmptyPool confirms an empty (but
// non-nil, e.g. wired-up-with-nothing-live) pool still prints its
// max/pending line with a "none live" placeholder, rather than an empty
// table or omitting the section.
func TestRenderStatus_ServicesSectionEmptyPool(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	svc := &statusAPIServicesResponse{MaxInstances: 8}
	if err := renderStatus(&buf, resp, "", svc); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "services (max=8 pending=0):") {
		t.Errorf("output missing empty-pool summary line:\n%s", out)
	}
	if !strings.Contains(out, "none live") {
		t.Errorf("output missing \"none live\" placeholder:\n%s", out)
	}
}
