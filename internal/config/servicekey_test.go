package config

import (
	"testing"
	"time"
)

// baseService is the reference Service the reorder/whitespace/field-change
// tests below all vary from — deliberately populated in every field so a
// missing field in ServiceKey's encoding would show up as a false collision
// somewhere in this file, not just a diff-from-zero-value blind spot.
func baseService() Service {
	return Service{
		Name:         "pg",
		Image:        "postgres:16-alpine",
		Port:         5432,
		Env:          []EnvVar{{Name: "POSTGRES_PASSWORD", Value: "scratch"}, {Name: "POSTGRES_USER", Value: "sa"}},
		ReadyCommand: []string{"pg_isready", "-h", "localhost"},
		ReadyTimeout: defaultReadyTimeout,
		IdleTTL:      defaultIdleTTL,
		Memory:       "2g",
		CPUs:         "1.5",
	}
}

func TestServiceKey_EnvReorder(t *testing.T) {
	a := baseService()
	b := baseService()
	b.Env = []EnvVar{{Name: "POSTGRES_USER", Value: "sa"}, {Name: "POSTGRES_PASSWORD", Value: "scratch"}}
	if ServiceKey("https://example.com/repo.git", a) != ServiceKey("https://example.com/repo.git", b) {
		t.Error("swapping env line order changed the key")
	}
}

func TestServiceKey_Whitespace(t *testing.T) {
	// Two KDL documents differing only in indentation/blank lines/comments,
	// parsed through ParseChecks, must hash identically — the key hashes
	// the parsed struct, never raw bytes.
	tight := []byte(`
service "pg" {
    image "postgres:16-alpine"
    port 5432
    env "POSTGRES_PASSWORD" "scratch"
}
check "t" {
    command "true"
    needs "pg"
}
`)
	reflowed := []byte(`
// a comment that must not affect hashing
service "pg" {

        image     "postgres:16-alpine"
        port      5432

        env "POSTGRES_PASSWORD"    "scratch"
}


check "t" {
    command "true"

    needs "pg"
}
`)
	csA, err := ParseChecks(tight)
	if err != nil {
		t.Fatalf("ParseChecks(tight): %v", err)
	}
	csB, err := ParseChecks(reflowed)
	if err != nil {
		t.Fatalf("ParseChecks(reflowed): %v", err)
	}
	remote := "https://example.com/repo.git"
	if ServiceKey(remote, csA.Services[0]) != ServiceKey(remote, csB.Services[0]) {
		t.Error("reflowing whitespace/comments changed the key")
	}
}

func TestServiceKey_AnyFieldChange(t *testing.T) {
	remote := "https://example.com/repo.git"
	baseKey := ServiceKey(remote, baseService())

	variants := map[string]Service{}

	v := baseService()
	v.Name = "pg2"
	variants["name"] = v

	v = baseService()
	v.Image = "postgres:17-alpine"
	variants["image"] = v

	v = baseService()
	v.Port = 5433
	variants["port"] = v

	v = baseService()
	v.Env = append([]EnvVar(nil), v.Env...)
	v.Env[0].Value = "different"
	variants["env value"] = v

	v = baseService()
	v.Env = append([]EnvVar(nil), v.Env...)
	v.Env[0].Name = "POSTGRES_PW"
	variants["env name"] = v

	v = baseService()
	v.ReadyCommand = []string{"pg_isready"}
	variants["ready-command"] = v

	v = baseService()
	v.ReadyTimeout = v.ReadyTimeout + time.Second
	variants["ready-timeout"] = v

	v = baseService()
	v.IdleTTL = v.IdleTTL + time.Minute
	variants["idle-ttl"] = v

	v = baseService()
	v.Memory = "4g"
	variants["memory"] = v

	v = baseService()
	v.CPUs = "2"
	variants["cpus"] = v

	for name, sv := range variants {
		if ServiceKey(remote, sv) == baseKey {
			t.Errorf("changing %s did not change the key", name)
		}
	}
}

func TestServiceKey_RemoteChange(t *testing.T) {
	svc := baseService()
	if ServiceKey("https://example.com/a.git", svc) == ServiceKey("https://example.com/b.git", svc) {
		t.Error("different remote produced the same key (M5 boundary)")
	}
}

func TestServiceKey_DefaultApplied(t *testing.T) {
	// Omitting idle-ttl (and letting applyServiceDefaults fill it) must
	// hash identically to writing the default explicitly.
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
	explicit := Service{Name: "pg", Image: "postgres:16-alpine", Port: 5432, ReadyTimeout: defaultReadyTimeout, IdleTTL: defaultIdleTTL}
	remote := "https://example.com/repo.git"
	if ServiceKey(remote, cs.Services[0]) != ServiceKey(remote, explicit) {
		t.Error("omitting idle-ttl/ready-timeout did not hash the same as writing the defaults explicitly")
	}
}
