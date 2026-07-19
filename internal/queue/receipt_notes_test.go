// Receipt-notes gate suite (issue #13, config-surface slice): covers
// SpecRejectReason's four new outcomes (policy enabled + spec missing a
// receipt, policy disabled + spec declares one, an unknown executor
// profile on the receipt node, and a receipt image on a non-container
// profile) directly against the exported function — SpecRejectReason is a
// pure predicate over (*config.CheckSpec, bool, func, func, bool) with no
// queue state, so a direct table test exercises it precisely without
// standing up the reconcile harness, the same way a spec's `after` graph
// is unit-tested in internal/config directly rather than through a run.
package queue

import (
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/config"
)

// mustParseChecks is a small test helper: ParseChecks or fail the test —
// every spec string here is deliberately valid at the config layer
// (SpecRejectReason only runs on specs that already parsed), so a parse
// failure means the fixture itself is wrong.
func mustParseChecks(t *testing.T, kdl string) *config.CheckSpec {
	t.Helper()
	cs, err := config.ParseChecks([]byte(kdl))
	if err != nil {
		t.Fatalf("ParseChecks: %v (fixture is wrong)", err)
	}
	return cs
}

func TestSpecRejectReason_ReceiptPolicy(t *testing.T) {
	const specNoReceipt = `
check "unit" {
    command "true"
}
`
	const specWithReceipt = `
check "unit" {
    command "true"
}
receipt "deployment" {
    command "./ci/write-candidate-receipt"
}
`
	cases := []struct {
		name          string
		kdl           string
		receiptPolicy bool
		wantSub       string // "" = accepted
	}{
		{"policy enabled, spec has no receipt", specNoReceipt, true, "this daemon requires a receipt (receipt-notes is configured) but the check spec declares none"},
		{"policy disabled, spec declares one", specWithReceipt, false, `check spec declares receipt "deployment" but this daemon has no receipt-notes policy`},
		{"policy enabled, spec declares one: accepted", specWithReceipt, true, ""},
		{"policy disabled, spec has none: accepted", specNoReceipt, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := mustParseChecks(t, tc.kdl)
			got := SpecRejectReason(spec, false, nil, nil, tc.receiptPolicy)
			if tc.wantSub == "" {
				if got != "" {
					t.Fatalf("SpecRejectReason = %q, want accepted", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("SpecRejectReason = %q, want it to contain %q", got, tc.wantSub)
			}
		})
	}
}

// TestSpecRejectReason_ReceiptExecutorAndImageGates covers the receipt
// node going through the SAME unknown-executor-profile and
// image-on-incapable-profile gates a check does — reported under the
// "receipt:<name>" node-name prefix (imageOnIncapableProfile/
// unknownExecutorProfile's own doc, mirroring "image:<name>").
func TestSpecRejectReason_ReceiptExecutorAndImageGates(t *testing.T) {
	t.Run("unknown executor profile on the receipt", func(t *testing.T) {
		spec := mustParseChecks(t, `
check "unit" {
    command "true"
}
receipt "deployment" {
    command "./ci/write-candidate-receipt"
    executor "ghost"
}
`)
		known := func(name string) bool { return false } // no named profiles known
		got := SpecRejectReason(spec, false, known, nil, true)
		wantSub := `check spec: check "receipt:deployment" selects unknown executor profile "ghost"`
		if !strings.Contains(got, wantSub) {
			t.Fatalf("SpecRejectReason = %q, want it to contain %q", got, wantSub)
		}
	})

	t.Run("known executor profile on the receipt: accepted", func(t *testing.T) {
		spec := mustParseChecks(t, `
check "unit" {
    command "true"
}
receipt "deployment" {
    command "./ci/write-candidate-receipt"
    executor "ci"
}
`)
		known := func(name string) bool { return name == "ci" }
		imageCapable := func(name string) bool { return name == "ci" }
		if got := SpecRejectReason(spec, false, known, imageCapable, true); got != "" {
			t.Fatalf("SpecRejectReason = %q, want accepted", got)
		}
	})

	t.Run("receipt image on a non-container (incapable) profile", func(t *testing.T) {
		spec := mustParseChecks(t, `
image "go-ci" {
    command "./ci/build"
}
check "unit" {
    command "true"
}
receipt "deployment" {
    command "./ci/write-candidate-receipt"
    image "go-ci"
}
`)
		imageCapable := func(name string) bool { return false } // nothing is container-capable
		got := SpecRejectReason(spec, false, nil, imageCapable, true)
		wantSub := `check spec: check "receipt:deployment" runs candidate-built image "go-ci" but its executor profile is not a container profile`
		if !strings.Contains(got, wantSub) {
			t.Fatalf("SpecRejectReason = %q, want it to contain %q", got, wantSub)
		}
	})

	t.Run("receipt image on a capable profile: accepted", func(t *testing.T) {
		spec := mustParseChecks(t, `
image "go-ci" {
    command "./ci/build"
}
check "unit" {
    command "true"
}
receipt "deployment" {
    command "./ci/write-candidate-receipt"
    executor "ci"
    image "go-ci"
}
`)
		known := func(name string) bool { return name == "ci" }
		imageCapable := func(name string) bool { return name == "ci" }
		if got := SpecRejectReason(spec, false, known, imageCapable, true); got != "" {
			t.Fatalf("SpecRejectReason = %q, want accepted", got)
		}
	})
}

// TestSpecRejectReason_ExistingGatesUnchanged confirms the pre-existing
// services/executor-profile/image gates still fire exactly as before the
// receipt-policy parameter was added — a regression guard for the
// signature change (all three call sites — startRun, finishBatchStart,
// cmd/gauntlet's crossCheck — were updated in the same change).
func TestSpecRejectReason_ExistingGatesUnchanged(t *testing.T) {
	t.Run("services declared with no services block", func(t *testing.T) {
		spec := mustParseChecks(t, `
service "db" {
    image "postgres:16"
    port 5432
}
check "unit" {
    command "true"
    needs "db"
}
`)
		got := SpecRejectReason(spec, false, nil, nil, false)
		want := "check spec declares services but this daemon has no services block"
		if got != want {
			t.Fatalf("SpecRejectReason = %q, want %q", got, want)
		}
	})

	t.Run("services declared and available: accepted", func(t *testing.T) {
		spec := mustParseChecks(t, `
service "db" {
    image "postgres:16"
    port 5432
}
check "unit" {
    command "true"
    needs "db"
}
`)
		if got := SpecRejectReason(spec, true, nil, nil, false); got != "" {
			t.Fatalf("SpecRejectReason = %q, want accepted", got)
		}
	})

	t.Run("unknown executor profile on a check", func(t *testing.T) {
		spec := mustParseChecks(t, `
check "unit" {
    command "true"
    executor "ghost"
}
`)
		got := SpecRejectReason(spec, false, nil, nil, false)
		want := `check spec: check "unit" selects unknown executor profile "ghost"`
		if got != want {
			t.Fatalf("SpecRejectReason = %q, want %q", got, want)
		}
	})

	t.Run("image on an incapable profile", func(t *testing.T) {
		spec := mustParseChecks(t, `
image "app" {
    command "true"
}
check "unit" {
    command "true"
    image "app"
}
`)
		imageCapable := func(name string) bool { return false }
		got := SpecRejectReason(spec, false, nil, imageCapable, false)
		want := `check spec: check "unit" runs candidate-built image "app" but its executor profile is not a container profile`
		if got != want {
			t.Fatalf("SpecRejectReason = %q, want %q", got, want)
		}
	})

	t.Run("plain accepted spec", func(t *testing.T) {
		spec := mustParseChecks(t, `
check "unit" {
    command "true"
}
`)
		if got := SpecRejectReason(spec, false, nil, nil, false); got != "" {
			t.Fatalf("SpecRejectReason = %q, want accepted", got)
		}
	})
}
