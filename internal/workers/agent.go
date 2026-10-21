package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/services"
	"github.com/sgrankin/gauntlet/internal/statefile"
)

type Agent struct {
	running          sync.Map
	lastGC           time.Time
	ShutdownTimeout  time.Duration
	Client           Client
	Worker           Worker
	StateDir, GitDir string
	Objects          *gitx.Repo
	Executor         core.Executor
	Services         map[string]*services.Pool
	Poll             time.Duration
}

type retainedResult struct {
	Attempt Attempt
	Result  Result
	At      time.Time
}

// Run stops claiming on shutdown and drains active commands. Transport failures
// do not stop executions: results are fsynced locally and retried on reconnect.
func (a *Agent) Run(ctx context.Context) error {
	if a.Objects == nil || a.Executor == nil || a.Worker.Capacity.Slots < 1 {
		return fmt.Errorf("worker requires objects, executor, and positive capacity")
	}
	if a.Poll <= 0 {
		a.Poll = time.Second
	}
	jobsDir := filepath.Join(a.StateDir, "jobs")
	if err := os.MkdirAll(jobsDir, 0700); err != nil {
		return err
	}
	a.Worker.Session = randomID()
	for profile := range a.Services {
		a.Worker.ServiceProfiles = append(a.Worker.ServiceProfiles, profile)
	}
	sort.Strings(a.Worker.ServiceProfiles)
	var tasks sync.WaitGroup
	active := make(chan struct{}, a.Worker.Capacity.Slots)
	finished := make(chan error, a.Worker.Capacity.Slots)
	ticker := time.NewTicker(a.Poll)
	defer ticker.Stop()
	executionCtx, stopExecutions := context.WithCancel(context.Background())
	defer func() {
		defer stopExecutions()
		drained := make(chan struct{})
		go func() { tasks.Wait(); close(drained) }()
		timeout := a.ShutdownTimeout
		if timeout <= 0 {
			timeout = 5 * time.Minute
		}
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-drained:
		case <-timer.C:
			stopExecutions()
			<-drained
		}
		uploadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = a.flush(uploadCtx, jobsDir)
	}()
	nextMaintenance := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		uploadErr := a.flush(ctx, jobsDir)
		if time.Now().After(nextMaintenance) {
			if err := a.maintain(ctx, jobsDir, len(active) == 0); err != nil && ctx.Err() == nil {
				return err
			}
			nextMaintenance = time.Now().Add(time.Minute)
		}
		if ctx.Err() != nil {
			return nil
		}
		if uploadErr == nil && len(active) < cap(active) {
			var attempt *Attempt
			requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			advertisement := a.Worker
			a.running.Range(func(_, value any) bool {
				used := value.(Resources)
				advertisement.Usage.Slots += used.Slots
				advertisement.Usage.CPU += used.CPU
				advertisement.Usage.Memory += used.Memory
				return true
			})
			err := a.Client.post(requestCtx, "/v1/claim", advertisement, &attempt)
			cancel()
			if err == nil && attempt != nil {
				if !safeAttempt(*attempt) {
					return fmt.Errorf("scheduler returned invalid attempt identity")
				}
				active <- struct{}{}
				activeDir := filepath.Join(jobsDir, attempt.ID+"-"+attempt.Token)
				a.running.Store(activeDir, attempt.Requirements.Resources)
				tasks.Add(1)
				go func(attempt Attempt) {
					defer tasks.Done()
					defer func() { <-active }()
					defer a.running.Delete(activeDir)
					finished <- a.execute(executionCtx, attempt, jobsDir)
				}(*attempt)
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-finished:
			if err != nil {
				return err
			}
		case <-ticker.C:
		}
	}
}

func safeAttempt(attempt Attempt) bool {
	if len(attempt.ID) != 64 || len(attempt.Token) != 48 {
		return false
	}
	for _, value := range []string{attempt.ID, attempt.Token} {
		for _, r := range value {
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
	}
	return true
}

func (a *Agent) execute(parent context.Context, attempt Attempt, jobsDir string) error {
	dir := filepath.Join(jobsDir, attempt.ID+"-"+attempt.Token)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		delay := max(time.Second, time.Duration(attempt.LeaseMillis)*time.Millisecond/3)
		ticker := time.NewTicker(delay)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				requestCtx, stop := context.WithTimeout(ctx, delay)
				err := a.Client.post(requestCtx, "/v1/attempt/"+attempt.ID+"/renew", map[string]string{"token": attempt.Token}, nil)
				stop()
				if errors.Is(err, ErrStale) {
					cancel()
					return
				}
			}
		}
	}()
	result := a.runJob(ctx, attempt, dir)
	cancel()
	<-heartbeatDone
	retained := retainedResult{Attempt: attempt, Result: EncodeResult(result), At: time.Now()}
	data, err := json.Marshal(retained)
	if err != nil {
		return err
	}
	// Persist before any upload: a lost acknowledgement or scheduler outage must
	// not turn a completed command into another execution.
	return statefile.Write(filepath.Join(dir, "result.json"), data)
}

func (a *Agent) runJob(ctx context.Context, attempt Attempt, dir string) core.CheckResult {
	job := attempt.Job
	failure := func(err error) core.CheckResult { return core.CheckResult{Name: job.Name, Err: err} }
	refs, err := a.Objects.ListLocalRefs(ctx, "refs/gauntlet/source/")
	if err != nil {
		return failure(err)
	}
	haves := make([]string, 0, len(refs))
	for _, sha := range refs {
		haves = append(haves, sha)
	}
	sort.Strings(haves)
	haves = slices.Compact(haves)
	haves = haves[:min(128, len(haves))]
	body, _ := json.Marshal(map[string]any{"token": attempt.Token, "haves": haves})
	response, err := a.Client.request(ctx, "POST", "/v1/attempt/"+attempt.ID+"/source", strings.NewReader(string(body)), "")
	if err != nil {
		return failure(err)
	}
	err = a.Objects.ImportPack(ctx, response.Body)
	response.Body.Close()
	if err != nil {
		return failure(err)
	}
	release, err := a.Objects.RetainSources(ctx, sourceCommits(job)...)
	if err != nil {
		return failure(err)
	}
	defer release()
	tree, err := a.Objects.ObjectTree(ctx, job.MergeSHA)
	if err != nil {
		return failure(err)
	}
	materialized := time.Now()
	job.Dir = filepath.Join(dir, "workspace")
	if err = os.MkdirAll(job.Dir, 0700); err != nil {
		return failure(err)
	}
	defer os.RemoveAll(job.Dir)
	if err = a.Objects.ExportTree(ctx, tree, job.Dir); err != nil {
		return failure(err)
	}
	if job.HistoryMtimes {
		if _, err = a.Objects.RestoreMtimes(ctx, job.MergeSHA, job.Dir); err != nil {
			return failure(err)
		}
	}
	took := time.Since(materialized)
	job.GitDir = a.GitDir
	job.LogPath = ""
	if job.RecordLogs {
		job.LogPath = filepath.Join(dir, "check.log.zst")
	}
	job.ServiceEnv, job.Networks = nil, nil
	var ensured services.Ensured
	pool := a.Services[job.Executor]
	if len(job.ServiceNeeds) > 0 {
		if pool == nil {
			return failure(fmt.Errorf("worker services are unavailable"))
		}
		var specs []config.Service
		if err = json.Unmarshal(job.ServiceSpecs, &specs); err != nil {
			return failure(err)
		}
		ensured, err = pool.EnsureAll(ctx, specs, job.ServiceNeeds)
		if err != nil {
			return failure(err)
		}
		defer pool.Release(ensured)
		job.ServiceEnv, job.Networks = ensured.Env, ensured.Networks
	}
	result := a.Executor.RunCheck(ctx, job)
	if pool != nil && len(job.ServiceNeeds) > 0 && result.Err == nil && result.Status == core.CheckFailed && pool.AnyDead(ctx, ensured) {
		result.Err = fmt.Errorf("worker service died mid-run")
	}
	result.Materialized = took
	return result
}

func (a *Agent) flush(ctx context.Context, jobsDir string) error {
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(jobsDir, entry.Name())
		file, err := os.Open(filepath.Join(dir, "result.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(file, maxResultBytes+1))
		file.Close()
		if err != nil {
			return err
		}
		var retained retainedResult
		if len(data) > maxResultBytes || json.Unmarshal(data, &retained) != nil || !safeAttempt(retained.Attempt) {
			return fmt.Errorf("invalid retained worker result")
		}
		if time.Since(retained.At) > 24*time.Hour {
			os.RemoveAll(dir)
			continue
		}
		attempt := retained.Attempt
		if retained.Result.Check.LogPath != "" {
			log, err := os.Open(retained.Result.Check.LogPath)
			if err != nil {
				return err
			}
			stat, err := log.Stat()
			if err != nil {
				log.Close()
				return err
			}
			if stat.Size() > maxLogBytes {
				retained.Result.Check.Output += "\ngauntlet: worker log exceeds upload limit; output tail and test reports retained."
				if len(retained.Result.Check.Output) > 65536 {
					retained.Result.Check.Output = retained.Result.Check.Output[len(retained.Result.Check.Output)-65536:]
				}
			} else {
				response, err := a.Client.request(ctx, "PUT", "/v1/attempt/"+attempt.ID+"/log", log, attempt.Token)
				log.Close()
				if errors.Is(err, ErrStale) {
					os.RemoveAll(dir)
					continue
				}
				if err != nil {
					retained.Result.Check.Output += "\ngauntlet: worker full log could not be uploaded; output tail and reports retained."
					if len(retained.Result.Check.Output) > 65536 {
						retained.Result.Check.Output = retained.Result.Check.Output[len(retained.Result.Check.Output)-65536:]
					}
				} else {
					var uploaded struct {
						Digest string `json:"digest"`
					}
					err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&uploaded)
					response.Body.Close()
					if err != nil {
						return err
					}
					retained.Result.LogDigest = uploaded.Digest
				}
			}
			log.Close()
		}
		request := struct {
			Token  string `json:"token"`
			Result Result `json:"result"`
		}{attempt.Token, retained.Result}
		err = a.Client.post(ctx, "/v1/attempt/"+attempt.ID+"/result", request, nil)
		if err == nil || errors.Is(err, ErrStale) {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return nil
}
