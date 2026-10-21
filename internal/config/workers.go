package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	kdl "github.com/sblinch/kdl-go"
)

type Workers struct {
	ScaleInterval  time.Duration `kdl:"scale-interval,format:units"`
	Pools          []WorkerPool  `kdl:"pool,multiple"`
	Listen         string        `kdl:"listen"`
	TokenEnv       string        `kdl:"token-env"`
	TLSCert        string        `kdl:"tls-cert"`
	TLSKey         string        `kdl:"tls-key"`
	Lease          time.Duration `kdl:"lease,format:units"`
	ReconnectGrace time.Duration `kdl:"reconnect-grace,format:units"`
}

func (w *Workers) defaults() {
	if w.ScaleInterval == 0 {
		w.ScaleInterval = 30 * time.Second
	}
	for i := range w.Pools {
		p := &w.Pools[i]
		if p.IdleTimeout == 0 {
			p.IdleTimeout = 5 * time.Minute
		}
		if p.StartupTimeout == 0 {
			p.StartupTimeout = 10 * time.Minute
		}
	}
	if w.Listen == "" {
		w.Listen = "127.0.0.1:8081"
	}
	if w.TokenEnv == "" {
		w.TokenEnv = "GAUNTLET_WORKER_TOKEN"
	}
	if w.Lease == 0 {
		w.Lease = time.Minute
	}
	if w.ReconnectGrace == 0 {
		w.ReconnectGrace = 2 * time.Minute
	}
}
func (w *Workers) validate() error {
	if w.ScaleInterval < time.Second || w.ScaleInterval > time.Hour {
		return fmt.Errorf("workers: scale-interval must be 1s..1h")
	}
	seen := map[string]bool{}
	for _, p := range w.Pools {
		if seen[p.Name] {
			return fmt.Errorf("workers: duplicate pool %q", p.Name)
		}
		seen[p.Name] = true
		if err := p.validate(); err != nil {
			return fmt.Errorf("worker pool %q: %w", p.Name, err)
		}
	}
	host, _, err := net.SplitHostPort(w.Listen)
	if err != nil {
		return fmt.Errorf("workers: invalid listen address")
	}
	if (w.TLSCert == "") != (w.TLSKey == "") {
		return fmt.Errorf("workers: tls-cert and tls-key must be set together")
	}
	ip := net.ParseIP(host)
	if w.TLSCert == "" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("workers: a non-loopback listener requires TLS")
	}
	if w.TokenEnv == "" || strings.ContainsAny(w.TokenEnv, "=\x00") {
		return fmt.Errorf("workers: token-env must name a variable")
	}
	if w.Lease < 3*time.Second || w.Lease > time.Hour || w.ReconnectGrace < w.Lease || w.ReconnectGrace > time.Hour {
		return fmt.Errorf("workers: lease must be 3s..1h and reconnect-grace must cover one lease, up to 1h")
	}
	return nil
}

// ResourceValues uses millicores and bytes for scheduler arithmetic. Empty
// requests reserve one core and 128 MiB; explicit zero requests are rejected.
func ResourceValues(cpu, memory string) (int64, int64, error) {
	if cpu != "" && !cpusPattern.MatchString(cpu) {
		return 0, 0, fmt.Errorf("invalid CPU quantity")
	}
	cores := 1.0
	var err error
	if cpu != "" {
		cores, err = strconv.ParseFloat(cpu, 64)
	}
	if err != nil || math.IsNaN(cores) || math.IsInf(cores, 0) || cores <= 0 || cores > 1000000 {
		return 0, 0, fmt.Errorf("cpus must be positive and at most 1000000")
	}
	bytes := int64(128 << 20)
	if memory != "" {
		if !memoryPattern.MatchString(memory) {
			return 0, 0, fmt.Errorf("invalid memory quantity")
		}
		multiplier := int64(1)
		number := strings.ToLower(memory)
		switch number[len(number)-1] {
		case 'b':
			number = number[:len(number)-1]
		case 'k':
			number = number[:len(number)-1]
			multiplier = 1 << 10
		case 'm':
			number = number[:len(number)-1]
			multiplier = 1 << 20
		case 'g':
			number = number[:len(number)-1]
			multiplier = 1 << 30
		}
		n, err := strconv.ParseInt(number, 10, 64)
		if err != nil || n <= 0 || n > math.MaxInt64/multiplier {
			return 0, 0, fmt.Errorf("memory must be a positive bounded quantity")
		}
		bytes = n * multiplier
	}
	return max(1, int64(math.Ceil(cores*1000))), bytes, nil
}

type Worker struct {
	ShutdownTimeout time.Duration `kdl:"shutdown-timeout,format:units"`
	Scheduler       string        `kdl:"scheduler"`
	Name            string        `kdl:"name"`
	TokenEnv        string        `kdl:"token-env"`
	Labels          []string      `kdl:"labels"`
	CPUs            string        `kdl:"cpus"`
	Memory          string        `kdl:"memory"`
	MaxExecutions   int           `kdl:"max-executions"`
	UrgentReserve   int           `kdl:"urgent-reserve"`
	Executors       []Executor    `kdl:"executor,multiple"`
	Executor        Executor      `kdl:"-"`
	Profiles        []Executor    `kdl:"-"`
	Services        Services      `kdl:"services"`
}

func LoadWorker(path string) (*Worker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var worker Worker
	if err = kdl.Unmarshal(data, &worker); err != nil {
		return nil, fmt.Errorf("worker config: %w", err)
	}
	if worker.ShutdownTimeout == 0 {
		worker.ShutdownTimeout = 5 * time.Minute
	}
	if worker.ShutdownTimeout < 0 || worker.ShutdownTimeout > 24*time.Hour {
		return nil, fmt.Errorf("worker: shutdown-timeout must be positive and at most 24h")
	}
	if worker.Name == "" {
		worker.Name, err = os.Hostname()
		if err != nil {
			return nil, err
		}
	}
	if worker.TokenEnv == "" {
		worker.TokenEnv = "GAUNTLET_WORKER_TOKEN"
	}
	endpoint, err := url.Parse(worker.Scheduler)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" && endpoint.Path != "/" || endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return nil, fmt.Errorf("worker: scheduler must be an HTTP(S) base URL")
	}
	if endpoint.Scheme == "http" {
		ip := net.ParseIP(endpoint.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("worker: remote scheduler requires HTTPS")
		}
	}
	if worker.TokenEnv == "" || strings.ContainsAny(worker.TokenEnv, "=\x00") {
		return nil, fmt.Errorf("worker: invalid token-env")
	}
	if worker.Name == "" || len(worker.Name) > 128 || strings.ContainsAny(worker.Name, "/\\\x00") {
		return nil, fmt.Errorf("worker: invalid name")
	}
	if worker.MaxExecutions < 1 || worker.MaxExecutions > 4096 || worker.UrgentReserve < 0 || worker.UrgentReserve >= worker.MaxExecutions {
		return nil, fmt.Errorf("worker: invalid execution capacity or urgent reserve")
	}
	if worker.CPUs == "" || worker.Memory == "" {
		return nil, fmt.Errorf("worker: cpus and memory capacity are required")
	}
	if _, _, err = ResourceValues(worker.CPUs, worker.Memory); err != nil {
		return nil, err
	}
	daemon := Daemon{Executors: worker.Executors}
	if err = daemon.resolveExecutors(); err != nil {
		return nil, err
	}
	applyExecutorDefaults(&daemon.Executor)
	if daemon.Executor.Kind == "" {
		daemon.Executor.Kind = "local"
	}
	if err = validateExecutor(&daemon.Executor, "worker executor"); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range daemon.Profiles {
		profile := &daemon.Profiles[i]
		applyExecutorDefaults(profile)
		if profile.Kind == "remote" || seen[profile.Name] {
			return nil, fmt.Errorf("worker profiles must be unique local/container executors")
		}
		seen[profile.Name] = true
		if err = validateExecutor(profile, "worker executor "+profile.Name); err != nil {
			return nil, err
		}
	}
	worker.Executor, worker.Profiles = daemon.Executor, daemon.Profiles
	if len(worker.Services.Allow) > 0 {
		if len(worker.Services.Allow) != 1 || worker.Services.Allow[0] != "container" {
			return nil, fmt.Errorf("worker: only container services are supported")
		}
		if worker.Services.Runtime == "" {
			worker.Services.Runtime = "docker"
		}
		if worker.Services.Runtime != "docker" && worker.Services.Runtime != "podman" {
			return nil, fmt.Errorf("worker: services require docker or podman")
		}
		if worker.Services.MaxInstances == 0 {
			worker.Services.MaxInstances = 8
		}
		if worker.Services.MaxInstances < 1 {
			return nil, fmt.Errorf("worker: max-instances must be positive")
		}
	}
	return &worker, nil
}
