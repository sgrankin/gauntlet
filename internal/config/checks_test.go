package config

import (
	"reflect"
	"testing"
	"time"
)

// TestParseChecks_Services confirms service/needs/env/ready-command nodes
// populate the structs above via the SAME tag shapes checks.go already used
// for Check.Command (child-node argv) and Check.Name (positional arg) —
// confirmed against the real kdl-go dependency, not just the design doc's
// illustrative snippet.
func TestParseChecks_Services(t *testing.T) {
	data := []byte(`
service "pg" {
    image "postgres:16-alpine"
    port 5432
    env "POSTGRES_PASSWORD" "scratch"
    env "POSTGRES_USER" "sa"
    ready-command "pg_isready" "-h" "localhost"
    ready-timeout "90s"
    idle-ttl "2h"
    memory "2g"
    cpus "1.5"
}

check "svc" {
    command "sh" "-c" "nc -z $GAUNTLET_SVC_PG_HOST $GAUNTLET_SVC_PG_PORT"
    needs "pg"
}
`)
	cs, err := ParseChecks(data)
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	if len(cs.Services) != 1 {
		t.Fatalf("Services = %+v, want 1 entry", cs.Services)
	}
	want := Service{
		Name:         "pg",
		Image:        "postgres:16-alpine",
		Port:         5432,
		Env:          []EnvVar{{Name: "POSTGRES_PASSWORD", Value: "scratch"}, {Name: "POSTGRES_USER", Value: "sa"}},
		ReadyCommand: []string{"pg_isready", "-h", "localhost"},
		ReadyTimeout: 90 * time.Second,
		IdleTTL:      2 * time.Hour,
		Memory:       "2g",
		CPUs:         "1.5",
	}
	if !reflect.DeepEqual(cs.Services[0], want) {
		t.Errorf("Services[0] = %+v, want %+v", cs.Services[0], want)
	}
	if len(cs.Checks) != 1 || len(cs.Checks[0].Needs) != 1 || cs.Checks[0].Needs[0] != "pg" {
		t.Fatalf("Checks[0].Needs = %+v, want [\"pg\"]", cs.Checks)
	}
}

// TestParseChecks_ServiceDefaults confirms applyServiceDefaults fills
// ReadyTimeout/IdleTTL from defaultReadyTimeout/defaultIdleTTL when the KDL
// omits them, and runs before validate() (an omitted, defaulted value must
// not trip the ">0" check).
func TestParseChecks_ServiceDefaults(t *testing.T) {
	data := []byte(`
service "pg" {
    image "postgres:16-alpine"
    port 5432
}
check "t" {
    command "true"
    needs "pg"
}
`)
	cs, err := ParseChecks(data)
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	svc := cs.Services[0]
	if svc.ReadyTimeout != defaultReadyTimeout {
		t.Errorf("ReadyTimeout = %v, want default %v", svc.ReadyTimeout, defaultReadyTimeout)
	}
	if svc.IdleTTL != defaultIdleTTL {
		t.Errorf("IdleTTL = %v, want default %v", svc.IdleTTL, defaultIdleTTL)
	}
}

// TestParseChecks_ServiceNameWithHyphenStaysLegal is the positive
// counterpart to the "service names collide under env-var name transform"
// case in TestParseChecks_Invalid (config_test.go): a hyphenated name with
// no other service colliding under envSafeName must parse cleanly — the new
// check rejects a COLLISION, not hyphens (or any other punctuation) in a
// service name per se.
func TestParseChecks_ServiceNameWithHyphenStaysLegal(t *testing.T) {
	data := []byte(`
service "my-db" {
    image "img"
    port 1433
}
check "t" {
    command "true"
    needs "my-db"
}
`)
	if _, err := ParseChecks(data); err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
}

// TestParseChecks_AfterGraph confirms the happy-path dependency grammar: a
// diamond (two roots, one join), forward references (an edge to a check
// declared later), and max-parallel all parse; absent max-parallel stays 0
// (the queue treats 0 as 1, the serial default).
func TestParseChecks_AfterGraph(t *testing.T) {
	data := []byte(`
max-parallel 4
check "package" {
    command "./ci/package"
    after "unit" "lint"
}
check "unit" {
    command "./ci/unit"
}
check "lint" {
    command "./ci/lint"
}
`)
	cs, err := ParseChecks(data)
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	if cs.MaxParallel != 4 {
		t.Errorf("MaxParallel = %d, want 4", cs.MaxParallel)
	}
	if got := cs.Checks[0].After; len(got) != 2 || got[0] != "unit" || got[1] != "lint" {
		t.Errorf("Checks[0].After = %v, want [unit lint]", got)
	}
	if cs.Checks[1].After != nil || cs.Checks[2].After != nil {
		t.Errorf("rootless checks should have nil After, got %v / %v", cs.Checks[1].After, cs.Checks[2].After)
	}

	noParallel, err := ParseChecks([]byte("check \"t\" {\n    command \"true\"\n}\n"))
	if err != nil {
		t.Fatalf("ParseChecks (no max-parallel): %v", err)
	}
	if noParallel.MaxParallel != 0 {
		t.Errorf("absent max-parallel = %d, want 0 (serial default)", noParallel.MaxParallel)
	}
}

// TestParseChecks_Workspace covers the isolated-workspace policy (issue
// #9): absent = shared (default, Isolated() false), "isolated" parses.
func TestParseChecks_Workspace(t *testing.T) {
	shared, err := ParseChecks([]byte("check \"t\" {\n    command \"true\"\n}\n"))
	if err != nil {
		t.Fatalf("ParseChecks (no workspace): %v", err)
	}
	if shared.Workspace != "" || shared.Isolated() {
		t.Errorf("absent workspace = %q / Isolated=%v, want shared default", shared.Workspace, shared.Isolated())
	}

	iso, err := ParseChecks([]byte("workspace \"isolated\"\ncheck \"t\" {\n    command \"true\"\n}\n"))
	if err != nil {
		t.Fatalf("ParseChecks (isolated): %v", err)
	}
	if iso.Workspace != "isolated" || !iso.Isolated() {
		t.Errorf("workspace = %q / Isolated=%v, want isolated", iso.Workspace, iso.Isolated())
	}
}

// TestParseChecks_ReceiptMinimal covers the smallest legal receipt node:
// just a name and a command. Executor/Image/After all stay at their zero
// values, and CheckSpec.Receipt() returns it.
func TestParseChecks_ReceiptMinimal(t *testing.T) {
	data := []byte(`
check "unit" {
    command "./ci/unit"
}
receipt "deployment" {
    command "./ci/write-candidate-receipt"
}
`)
	cs, err := ParseChecks(data)
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	r := cs.Receipt()
	if r == nil {
		t.Fatal("Receipt() = nil, want the declared receipt")
	}
	want := Receipt{Name: "deployment", Command: []string{"./ci/write-candidate-receipt"}}
	if !reflect.DeepEqual(*r, want) {
		t.Errorf("Receipt() = %+v, want %+v", *r, want)
	}
}

// TestParseChecks_ReceiptFullFeatured covers every receipt field at once:
// executor profile selection, a candidate-built image, and `after` edges
// naming checks — the same fields and semantics Check itself has, per the
// issue #13 design (Receipt's doc comment).
func TestParseChecks_ReceiptFullFeatured(t *testing.T) {
	data := []byte(`
image "go-ci" {
    command "./ci/build-image"
}
check "unit" {
    command "./ci/unit"
}
check "lint" {
    command "./ci/lint"
}
receipt "deployment" {
    command "./ci/write-candidate-receipt"
    executor "host"
    image "go-ci"
    after "unit" "lint"
}
`)
	cs, err := ParseChecks(data)
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	r := cs.Receipt()
	if r == nil {
		t.Fatal("Receipt() = nil, want the declared receipt")
	}
	want := Receipt{
		Name:     "deployment",
		Command:  []string{"./ci/write-candidate-receipt"},
		Executor: "host",
		Image:    "go-ci",
		After:    []string{"unit", "lint"},
	}
	if !reflect.DeepEqual(*r, want) {
		t.Errorf("Receipt() = %+v, want %+v", *r, want)
	}
}

// TestParseChecks_ReceiptAbsent confirms Receipt() is nil when no receipt
// node is declared — the common case, and the signal
// queue.SpecRejectReason's new receipt-notes gates consume.
func TestParseChecks_ReceiptAbsent(t *testing.T) {
	cs, err := ParseChecks([]byte("check \"t\" {\n    command \"true\"\n}\n"))
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	if r := cs.Receipt(); r != nil {
		t.Errorf("Receipt() = %+v, want nil", r)
	}
}

func TestCheckSpec_RequiresServices(t *testing.T) {
	cases := []struct {
		name string
		cs   CheckSpec
		want bool
	}{
		{"no services, no needs", CheckSpec{Checks: []Check{{Name: "t", Command: []string{"true"}}}}, false},
		{"has a service, no needs", CheckSpec{
			Checks:   []Check{{Name: "t", Command: []string{"true"}}},
			Services: []Service{{Name: "pg"}},
		}, true},
		{"no services, check has needs", CheckSpec{
			Checks: []Check{{Name: "t", Command: []string{"true"}, Needs: []string{"pg"}}},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cs.RequiresServices(); got != tc.want {
				t.Errorf("RequiresServices() = %v, want %v", got, tc.want)
			}
		})
	}
}
