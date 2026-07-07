//go:build dockerlive

package services

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
)

// This file is opt-in only (build tag dockerlive) — excluded from the
// default `go test ./...` run, which is what pool_test.go/record_test.go's
// fake-driver units cover instead. Run explicitly with
// `go test -tags dockerlive ./internal/services/...` against a real
// docker/podman daemon (e.g. `colima start`).

// TestContainerDriverLive exercises containerDriver end-to-end in
// ModePublish: create, the ready-command probe path, probe-alive, list,
// inspect-key, log tailing, and destroy. Skipped cleanly, not failed, when
// no runtime is reachable.
func TestContainerDriverLive(t *testing.T) {
	bin := requireRuntime(t)
	d := newContainerDriver(bin, "livetest")
	ctx := context.Background()

	svc := config.Service{
		Name:         "redis",
		Image:        "docker.io/library/redis:7-alpine",
		Port:         6379,
		ReadyCommand: []string{"redis-cli", "-h", "127.0.0.1", "ping"},
		ReadyTimeout: 30 * time.Second,
		IdleTTL:      time.Minute,
	}
	key := config.ServiceKey("https://example.test/repo.git", svc)
	is := InstanceSpec{
		Key:  key,
		Spec: svc,
		Name: "gauntlet-svc-livetest-" + key[:12],
		Mode: ModePublish,
	}
	t.Cleanup(func() { _ = exec.Command(bin, "rm", "-f", "-v", is.Name).Run() })

	inst, err := d.Create(ctx, is)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.ContainerID == "" {
		t.Fatal("Create: empty ContainerID")
	}
	if inst.Host != "127.0.0.1" || inst.Port == "" {
		t.Fatalf("Create: unresolved publish endpoint %+v", inst)
	}

	deadline := time.Now().Add(30 * time.Second)
	var readyErr error
	for time.Now().Before(deadline) {
		readyErr = d.ProbeReady(ctx, inst)
		if readyErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if readyErr != nil {
		t.Fatalf("ProbeReady: never became ready: %v", readyErr)
	}

	if alive, err := d.ProbeAlive(ctx, inst); err != nil || !alive {
		t.Fatalf("ProbeAlive = %v, %v; want true, nil", alive, err)
	}

	names, err := d.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !containsName(names, is.Name) {
		t.Fatalf("List: %v does not contain %q", names, is.Name)
	}

	gotKey, ok, err := d.InspectKey(ctx, is.Name)
	if err != nil || !ok || gotKey != key {
		t.Fatalf("InspectKey = %q, %v, %v; want %q, true, nil", gotKey, ok, err, key)
	}

	if logs := d.TailLogs(ctx, inst); logs == "" {
		t.Error("TailLogs: empty (expected at least redis's startup banner)")
	}

	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if alive, _ := d.ProbeAlive(ctx, inst); alive {
		t.Error("ProbeAlive after Destroy: still alive")
	}
}

// TestContainerDriverLiveNetworkDefaultProbe exercises ModeNetwork's
// default ready probe (probeListeningInContainer — the ModeNetwork
// fallback this driver uses in place of a TCP dial the daemon has no route
// to perform; see probeDefault's doc) end-to-end: idempotent network
// create, --network-alias, and the /proc/net/tcp listening check against
// redis's own port with no ready-command declared.
func TestContainerDriverLiveNetworkDefaultProbe(t *testing.T) {
	bin := requireRuntime(t)
	d := newContainerDriver(bin, "livetest-net")
	ctx := context.Background()

	svc := config.Service{
		Name:         "redis",
		Image:        "docker.io/library/redis:7-alpine",
		Port:         6379,
		ReadyTimeout: 30 * time.Second,
		IdleTTL:      time.Minute,
	}
	key := config.ServiceKey("https://example.test/repo.git", svc)
	net := "gauntlet-svc-livetest-net"
	is := InstanceSpec{
		Key:   key,
		Spec:  svc,
		Name:  "gauntlet-svc-livetest-net-" + key[:12],
		Alias: key[:12],
		Mode:  ModeNetwork,
		Net:   net,
	}
	t.Cleanup(func() {
		_ = exec.Command(bin, "rm", "-f", "-v", is.Name).Run()
		_ = exec.Command(bin, "network", "rm", net).Run()
	})

	inst, err := d.Create(ctx, is)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.Host != is.Alias {
		t.Fatalf("Create: Host = %q, want alias %q", inst.Host, is.Alias)
	}

	// Idempotent network create: a second call on the same network must
	// not fail on "already exists" (services-impl.md §3.4).
	if err := d.ensureNetwork(ctx, net); err != nil {
		t.Fatalf("ensureNetwork (idempotent re-create): %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	var readyErr error
	for time.Now().Before(deadline) {
		readyErr = d.ProbeReady(ctx, inst)
		if readyErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if readyErr != nil {
		t.Fatalf("ProbeReady (default probe, ModeNetwork): never ready: %v", readyErr)
	}

	if err := d.Destroy(ctx, inst); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

func requireRuntime(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		if err := exec.Command(bin, "ps").Run(); err == nil {
			return bin
		}
	}
	t.Skip("no reachable docker/podman runtime; skipping dockerlive test")
	return ""
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}
