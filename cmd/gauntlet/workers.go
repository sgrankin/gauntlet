package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/services"
	"github.com/sgrankin/gauntlet/internal/workers"
)

type workerRuntime struct {
	Broker   *workers.Broker
	Executor *workers.Executor
	server   *http.Server
	stop     context.CancelFunc
	done     sync.WaitGroup
}

func startWorkers(ctx context.Context, cancel context.CancelFunc, cfg *config.Daemon, state string, repo *gitx.Repo, logs string) (*workerRuntime, error) {
	if cfg.Workers == nil {
		return nil, nil
	}
	settings := cfg.Workers
	token := os.Getenv(settings.TokenEnv)
	if len(token) < 32 {
		return nil, fmt.Errorf("workers: %s must contain at least 32 bytes", settings.TokenEnv)
	}
	broker, err := workers.Open(filepath.Join(state, "workers.db"), settings.Lease, settings.ReconnectGrace, nil)
	if err != nil {
		return nil, err
	}
	profiles := map[string]workers.Requirements{}
	for _, profile := range cfg.Profiles {
		if profile.Kind == "remote" {
			cpu, memory, err := config.ResourceValues(profile.CPUs, profile.Memory)
			if err != nil {
				broker.Close()
				return nil, err
			}
			profiles[profile.Name] = workers.Requirements{Labels: profile.Labels, Resources: workers.Resources{Slots: 1, CPU: cpu, Memory: memory}}
		}
	}
	runtime := &workerRuntime{Broker: broker, Executor: &workers.Executor{Broker: broker, Root: ctx, Profiles: profiles}}
	listener, err := net.Listen("tcp", settings.Listen)
	if err != nil {
		broker.Close()
		return nil, err
	}
	if settings.TLSCert != "" {
		certificate, err := tls.LoadX509KeyPair(settings.TLSCert, settings.TLSKey)
		if err != nil {
			listener.Close()
			broker.Close()
			return nil, err
		}
		listener = tls.NewListener(listener, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
	}
	handler := (&workers.Server{Broker: broker, Objects: repo, Token: token, LogDir: logs}).Handler()
	runtime.server = &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16384}
	maintenanceCtx, stop := context.WithCancel(ctx)
	runtime.stop = stop
	runtime.done.Add(2)
	go func() {
		defer runtime.done.Done()
		if err := runtime.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "gauntlet workers: %v\n", err)
			cancel()
		}
	}()
	go func() {
		defer runtime.done.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-maintenanceCtx.Done():
				return
			case <-ticker.C:
				if err := broker.Maintain(maintenanceCtx, 2*time.Minute, 24*time.Hour); err != nil && maintenanceCtx.Err() == nil {
					fmt.Fprintf(os.Stderr, "gauntlet worker maintenance: %v\n", err)
				}
			}
		}
	}()
	return runtime, nil
}

func (w *workerRuntime) Close() {
	if w == nil {
		return
	}
	w.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.server.Shutdown(ctx); err != nil {
		w.server.Close()
	}
	w.done.Wait()
	w.Broker.Close()
}

func runWorker(args []string) error {
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	path := flags.String("config", "gauntlet-worker.kdl", "worker configuration")
	state := flags.String("state", ".gauntlet-worker", "worker state directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) > 0 {
		return fmt.Errorf("worker takes no positional arguments")
	}
	cfg, err := config.LoadWorker(*path)
	if err != nil {
		return err
	}
	token := os.Getenv(cfg.TokenEnv)
	if len(token) < 32 {
		return fmt.Errorf("worker: %s must contain at least 32 bytes", cfg.TokenEnv)
	}
	absolute, err := filepath.Abs(*state)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(absolute, 0700); err != nil {
		return err
	}
	lock, err := AcquireLock(absolute)
	if err != nil {
		return err
	}
	defer lock.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	gitDir := filepath.Join(absolute, "objects.git")
	objects, err := gitx.OpenObjects(ctx, gitDir)
	if err != nil {
		return err
	}
	scratch := filepath.Join(absolute, "scratch")
	if err = os.MkdirAll(scratch, 0700); err != nil {
		return err
	}
	namespace, err := stateToken(absolute)
	if err != nil {
		return err
	}
	executorCfg := &config.Daemon{Executor: cfg.Executor, Profiles: cfg.Profiles}
	backend, err := buildExecutor(executorCfg, scratch, namespace, gitDir, []string{cfg.TokenEnv})
	if err != nil {
		return err
	}
	pools := map[string]*services.Pool{}
	byRuntime := map[string]*services.Pool{}
	if len(cfg.Services.Allow) > 0 {
		all := append([]config.Executor{cfg.Executor}, cfg.Profiles...)
		for _, profile := range all {
			runtime := cfg.Services.Runtime
			mode := services.ModePublish
			if profile.Kind == "container" {
				runtime = profile.Runtime
				mode = services.ModeNetwork
			}
			if runtime != "docker" && runtime != "podman" {
				continue
			}
			key := runtime + "-" + mode.String()
			pool := byRuntime[key]
			if pool == nil {
				serviceDir := filepath.Join(absolute, "services", key)
				if err = os.MkdirAll(serviceDir, 0700); err != nil {
					return err
				}
				pool, err = services.New(services.Config{Remote: cfg.Scheduler, Token: namespace + "-" + key, Mode: mode, Runtime: runtime, StateDir: serviceDir, MaxInstances: cfg.Services.MaxInstances})
				if err != nil {
					return err
				}
				if err = pool.Adopt(ctx); err != nil {
					fmt.Fprintf(os.Stderr, "worker service adoption: %v\n", err)
				}
				pool.ArmReaper()
				byRuntime[key] = pool
			}
			pools[profile.Name] = pool
		}
	}
	profiles := []string{""}
	var imageProfiles []string
	if cfg.Executor.Kind == "container" {
		imageProfiles = append(imageProfiles, "")
	}
	for _, profile := range cfg.Profiles {
		profiles = append(profiles, profile.Name)
		if profile.Kind == "container" {
			imageProfiles = append(imageProfiles, profile.Name)
		}
	}
	cpu, memory, err := config.ResourceValues(cfg.CPUs, cfg.Memory)
	if err != nil {
		return err
	}
	agent := &workers.Agent{ShutdownTimeout: cfg.ShutdownTimeout, Client: workers.Client{URL: cfg.Scheduler, Token: token}, Worker: workers.Worker{ID: cfg.Name, Profiles: profiles, ImageProfiles: imageProfiles, Labels: cfg.Labels, Capacity: workers.Resources{Slots: cfg.MaxExecutions, CPU: cpu, Memory: memory}, UrgentReserve: cfg.UrgentReserve}, StateDir: absolute, GitDir: gitDir, Objects: objects, Executor: backend, Services: pools}
	return agent.Run(ctx)
}

var _ core.Executor = (*workers.Executor)(nil)
