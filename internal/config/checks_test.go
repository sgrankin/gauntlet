package config

import (
	"reflect"
	"testing"
	"time"
)

// TestParseChecks_Services is the §2.1 kdl-go grammar spike, turned into a
// permanent regression test: service/needs/env/ready-command nodes must
// populate the structs above via the SAME tag shapes checks.go already used
// for Check.Command (child-node argv) and Check.Name (positional arg) —
// confirmed against the real kdl-go dependency, not the design doc's
// illustrative snippet (docs/plans/services-impl.md §2.1).
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
