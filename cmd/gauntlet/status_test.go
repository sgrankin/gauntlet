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
				{"ref": "refs/heads/for/main/mallory/evil", "sha": "4444444444444444444444444444444444444444", "outcome": "rejected", "reason": "build failed", "at": "2026-07-05T11:00:00Z"}
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
	if err := renderStatus(&buf, resp, ""); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"main (main) tip=aaaaaaaa",
		"in-flight: refs/heads/for/main/alice/feat-a check=test",
		"waiting: 2",
		"refs/heads/for/main/mallory/evil [rejected]: build failed",
		"release (release/v2) tip=-",
		"in-flight: -",
		"waiting: 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\noutput:\n%s", want, out)
		}
	}
}

func TestRenderStatus_FiltersByTarget(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "release"); err != nil {
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
	if err := renderStatus(&buf, resp, "does-not-exist"); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "no such target: does-not-exist") {
		t.Errorf("output = %q", buf.String())
	}
}

func TestRenderStatus_NoParkedOmitsSection(t *testing.T) {
	resp := decodeCanned(t)
	var buf bytes.Buffer
	if err := renderStatus(&buf, resp, "release"); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if strings.Contains(buf.String(), "parked:") {
		t.Errorf("expected no parked section for release:\n%s", buf.String())
	}
}
