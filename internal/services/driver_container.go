package services

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// serviceKeyLabel is the container label containerDriver stamps at Create
// and reads back at InspectKey — names carry only a 12-hex truncation, so
// the label, never the name, is the source of truth for adoption matching
// (services.md §2 review m2, m6).
const serviceKeyLabel = "dev.gauntlet.service-key"

// containerDriver is the phase-A Driver: docker or podman via their CLI,
// reusing the operator's existing images and runtime as-is (services.md
// §6.1). It deliberately does not reuse internal/executor's
// runtimeSpec/probeRuntime — the two share only the binary name and a
// reachability probe, while this driver's whole argv surface (run -d,
// network create, inspect, exec, rm -v, logs, port) is disjoint from the
// check executor's run --rm (docs/plans/services-impl.md §1 "Driver reuse
// decision"). internal/executor/container.go is untouched by this package.
type containerDriver struct {
	bin   string // "docker" | "podman"
	token string
}

func newContainerDriver(runtime, token string) *containerDriver {
	return &containerDriver{bin: runtime, token: token}
}

var _ Driver = (*containerDriver)(nil)

// Create implements Driver.
func (d *containerDriver) Create(ctx context.Context, is InstanceSpec) (Instance, error) {
	if is.Mode == ModeNetwork {
		if err := d.ensureNetwork(ctx, is.Net); err != nil {
			return Instance{}, err
		}
	}

	args := createArgs(is)

	out, err := exec.CommandContext(ctx, d.bin, args...).CombinedOutput()
	if err != nil {
		return Instance{}, fmt.Errorf("%s run -d: %w (%s)", d.bin, err, strings.TrimSpace(string(out)))
	}
	containerID := strings.TrimSpace(string(out))

	inst := Instance{
		Name:        is.Name,
		Key:         is.Key,
		ContainerID: containerID,
		Mode:        is.Mode,
		Spec:        is.Spec, // A1
	}

	switch is.Mode {
	case ModeNetwork:
		inst.Host = is.Alias
		inst.Port = strconv.Itoa(is.Spec.Port)
	case ModePublish:
		host, port, err := d.readPublishedPort(ctx, is.Name, is.Spec.Port)
		if err != nil {
			_ = d.Destroy(ctx, Instance{Name: is.Name})
			return Instance{}, fmt.Errorf("%s port: %w", d.bin, err)
		}
		inst.Host, inst.Port = host, port
	}
	return inst, nil
}

// createArgs builds the `run -d` argv for is — split out of Create as a pure
// function so its shape (mode-specific flags, resource limits, env, image)
// is unit-testable without a real docker/podman daemon (the rest of Create
// is dockerlive-only).
func createArgs(is InstanceSpec) []string {
	args := []string{"run", "-d",
		"--name", is.Name,
		"--label", serviceKeyLabel + "=" + is.Key,
		"--restart", "no",
	}
	switch is.Mode {
	case ModeNetwork:
		args = append(args, "--network", is.Net, "--network-alias", is.Alias)
	case ModePublish:
		args = append(args, "-p", fmt.Sprintf("127.0.0.1:0:%d", is.Spec.Port))
	}
	// Verbatim passthrough, omitted entirely when unset (services.md §7
	// "Resource honesty" phase-B landing) — no gauntlet-chosen default fills
	// in for an author who left these unset.
	if is.Spec.Memory != "" {
		args = append(args, "--memory", is.Spec.Memory)
	}
	if is.Spec.CPUs != "" {
		args = append(args, "--cpus", is.Spec.CPUs)
	}
	for _, e := range is.Spec.Env {
		args = append(args, "-e", e.Name+"="+e.Value)
	}
	args = append(args, is.Spec.Image)
	return args
}

// ensureNetwork creates name if it doesn't already exist. "already exists"
// on stderr is treated as success (services-impl.md §3.4's idempotent
// network create) — docker/podman both fail `network create` on a name
// collision instead of no-op'ing, so this driver swallows exactly that one
// error text itself.
func (d *containerDriver) ensureNetwork(ctx context.Context, name string) error {
	out, err := exec.CommandContext(ctx, d.bin, "network", "create", name).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already exists") {
		return fmt.Errorf("%s network create %s: %w (%s)", d.bin, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// readPublishedPort reads back the kernel-assigned host port for
// containerPort published on name (ModePublish's `-p 127.0.0.1:0:<port>`) —
// gauntlet never picks a port itself, so it can never pick a duplicate
// (services.md §5 "Ports: who allocates, who cares").
func (d *containerDriver) readPublishedPort(ctx context.Context, name string, containerPort int) (host, port string, err error) {
	out, err := exec.CommandContext(ctx, d.bin, "port", name, strconv.Itoa(containerPort)).Output()
	if err != nil {
		return "", "", fmt.Errorf("port %d: %w", containerPort, err)
	}
	// docker/podman both print one "IP:PORT" binding per line (IPv4/IPv6);
	// since we always publish on 127.0.0.1 explicitly, the first line is
	// the one that matters and only its trailing port is ever used.
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	idx := strings.LastIndex(line, ":")
	if idx < 0 {
		return "", "", fmt.Errorf("port %d: unparseable output %q", containerPort, line)
	}
	return "127.0.0.1", line[idx+1:], nil
}

// ProbeAlive implements Driver: existence + running state via inspect, NOT
// the ready command (services.md §6). A failed inspect (typically: gone)
// reports false with no error — "not alive" is the ordinary outcome here,
// not an infra failure worth surfacing.
func (d *containerDriver) ProbeAlive(ctx context.Context, in Instance) (bool, error) {
	out, err := exec.CommandContext(ctx, d.bin, "inspect", "-f", "{{.State.Running}}", in.Name).Output()
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// ProbeReady implements Driver: in.Spec.ReadyCommand executed inside the
// instance via exec (services.md §6 review q2), or — when ReadyCommand is
// empty — a default probe (Service.ReadyCommand's doc in internal/config).
func (d *containerDriver) ProbeReady(ctx context.Context, in Instance) error {
	if len(in.Spec.ReadyCommand) > 0 {
		args := append([]string{"exec", in.Name}, in.Spec.ReadyCommand...)
		out, err := exec.CommandContext(ctx, d.bin, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("ready-command: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return d.probeDefault(ctx, in)
}

// probeDefault is the default ready probe when a Service declares no
// ReadyCommand (services.md §6): a TCP dial of the resolved endpoint by the
// daemon. That works as stated in ModePublish, where the endpoint is a
// host-published 127.0.0.1 port the daemon can dial directly.
//
// In ModeNetwork, though, the daemon process itself is never attached to
// the shared per-service network (services.md §5: only service and check
// containers join it) — it has no route to the alias at all, so a literal
// "TCP dial of the endpoint by the daemon" is impossible to perform from
// outside a container. This is the judgment call the plan flagged
// (services-impl.md's "ModeNetwork can't reach the alias" note): rather
// than require every image to bundle a specific probe tool, this execs into
// the instance itself and reads its own /proc/net/tcp for a LISTEN entry on
// the declared port — equivalent in spirit to ProbeAlive-plus-port-open,
// chosen over "just reuse ProbeAlive" because a running-but-not-yet
// listening service (e.g. still initializing its data directory) must
// still park as not-ready.
func (d *containerDriver) probeDefault(ctx context.Context, in Instance) error {
	if in.Mode == ModePublish {
		host, port := d.Endpoint(in)
		return dialTCP(ctx, host, port)
	}
	return d.probeListeningInContainer(ctx, in)
}

func dialTCP(ctx context.Context, host, port string) error {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("tcp dial %s:%s: %w", host, port, err)
	}
	conn.Close()
	return nil
}

// probeListeningInContainer checks whether in.Spec.Port has a LISTEN entry
// inside the instance by reading /proc/net/tcp[6] via exec — see
// probeDefault's doc for why this is the ModeNetwork fallback instead of a
// host-side dial.
func (d *containerDriver) probeListeningInContainer(ctx context.Context, in Instance) error {
	hexPort := strings.ToUpper(strconv.FormatInt(int64(in.Spec.Port), 16))
	if len(hexPort) < 4 {
		hexPort = strings.Repeat("0", 4-len(hexPort)) + hexPort
	}
	out, err := exec.CommandContext(ctx, d.bin, "exec", in.Name, "cat", "/proc/net/tcp", "/proc/net/tcp6").Output()
	if err != nil {
		return fmt.Errorf("probe listening port %d: exec cat /proc/net/tcp: %w", in.Spec.Port, err)
	}
	// Each /proc/net/tcp line is "sl local_address rem_address st ...";
	// local_address is "HEXIP:HEXPORT" and st==0A means LISTEN.
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		addr := strings.SplitN(fields[1], ":", 2)
		if len(addr) == 2 && addr[1] == hexPort && fields[3] == "0A" {
			return nil
		}
	}
	return fmt.Errorf("probe listening port %d: not listening", in.Spec.Port)
}

// Destroy implements Driver: `rm -f -v`, always — anonymous volumes are
// removed with the container (review m4); specs cannot declare named
// volumes so there is nothing else to leak.
func (d *containerDriver) Destroy(ctx context.Context, in Instance) error {
	if in.Name == "" {
		return nil
	}
	if err := exec.CommandContext(ctx, d.bin, "rm", "-f", "-v", in.Name).Run(); err != nil {
		return fmt.Errorf("%s rm -f -v %s: %w", d.bin, in.Name, err)
	}
	return nil
}

// Endpoint implements Driver.
func (d *containerDriver) Endpoint(in Instance) (host, port string) {
	return in.Host, in.Port
}

// List implements Driver, reusing cmd/gauntlet/sweep.go's listing pattern
// (`ps -a --format {{.Names}}`, prefix-filtered client-side in Go) but not
// that file's code — this package does not import cmd/gauntlet
// (docs/plans/services-impl.md §3.4).
func (d *containerDriver) List(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, d.bin, "ps", "-a", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil, fmt.Errorf("%s ps: %w", d.bin, err)
	}
	return strings.Fields(string(out)), nil
}

// InspectKey implements Driver: reads name's serviceKeyLabel via inspect's
// Go-template formatting. ok is false (not an error) when name doesn't
// exist or carries no such label — both are "can't adopt this by key", not
// "adoption failed".
func (d *containerDriver) InspectKey(ctx context.Context, name string) (string, bool, error) {
	out, err := exec.CommandContext(ctx, d.bin,
		"inspect", "-f", fmt.Sprintf("{{index .Config.Labels %q}}", serviceKeyLabel), name).Output()
	if err != nil {
		return "", false, nil
	}
	key := strings.TrimSpace(string(out))
	if key == "" || key == "<no value>" {
		return "", false, nil
	}
	return key, true, nil
}

// TailLogs implements Driver: last ~50 lines, failure-path diagnostics only
// (review m5). Best-effort — an error is folded into the returned string
// rather than propagated, since TailLogs is always called right before
// Destroy on an already-failing path and must never itself block cleanup.
func (d *containerDriver) TailLogs(ctx context.Context, in Instance) string {
	if in.Name == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, d.bin, "logs", "--tail", "50", in.Name).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("(logs unavailable: %v)", err)
	}
	return string(out)
}
