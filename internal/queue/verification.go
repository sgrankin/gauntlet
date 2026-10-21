package queue

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/verification"
)

// reusableCheck owns the acquired slot and source holds, even after its queue
// waiter detaches. Only immutable run data may cross this boundary.
func (d *Daemon) reusableCheck(ctx context.Context, job core.CheckJob, check config.Check, tree string, svcs []config.Service, deps []string, release func()) core.CheckResult {
	cleanup := func() { release(); d.cfg.Slots.Release() }
	fail := func(err error) core.CheckResult { cleanup(); return core.CheckResult{Name: job.Name, Err: err} }
	if d.cfg.Verification == nil {
		return fail(fmt.Errorf("input-based checks require a verification store"))
	}
	remote := d.cfg.RemoteExecutorProfile != nil && d.cfg.RemoteExecutorProfile(job.Executor)
	var dir string
	var err error
	if !remote {
		dir, _, err = d.materializeNode(ctx, tree, job.MergeSHA)
		if err != nil {
			return fail(err)
		}
	}
	identityJob := job
	identityJob.TestResults, identityJob.RetryCommand, identityJob.RetryTests = "", nil, nil
	identityJob.ServiceNeeds, identityJob.ServiceSpecs = nil, nil
	identityJob.Dir, identityJob.Command, identityJob.InputsCapture = dir, check.Inputs, true
	if job.LogPath != "" {
		identityJob.LogPath = strings.TrimSuffix(job.LogPath, ".log.zst") + ".inputs.log.zst"
	}
	identity := d.exec.RunCheck(ctx, identityJob)
	os.RemoveAll(dir)
	if identity.Err != nil || identity.Status != core.CheckPassed || identity.InputsKey == "" || len(identity.InputsKey) > 4096 {
		cleanup()
		identity.Name = job.Name
		if identity.Err == nil {
			identity.Status = core.CheckFailed
			identity.Output += "\ngauntlet: input identity could not be established"
		}
		return identity
	}
	key := verification.Key("check-v1", d.cfg.VerificationContext, job.Target, check, svcs, deps, sha256.Sum256([]byte(identity.InputsKey)), job.Image)
	job.VerificationID = key
	return d.cfg.Verification.Run(ctx, key, job, func(taskCtx context.Context) core.CheckResult {
		var took time.Duration
		var dir string
		var err error
		if !remote {
			dir, took, err = d.materializeNode(taskCtx, tree, job.MergeSHA)
			if err != nil {
				return core.CheckResult{Name: job.Name, Err: err}
			}
			defer os.RemoveAll(dir)
		}
		job.Dir = dir
		run := func(ctx context.Context, attempt core.CheckJob) core.CheckResult {
			return d.exec.RunCheck(ctx, attempt)
		}
		if !remote && len(check.Needs) > 0 && d.cfg.Services != nil {
			ens, err := d.cfg.Services.EnsureAll(taskCtx, svcs, check.Needs)
			if err != nil {
				return core.CheckResult{Name: job.Name, Err: fmt.Errorf("service ensure: %w", err)}
			}
			defer d.cfg.Services.Release(ens)
			job.ServiceEnv, job.Networks = ens.Env, ens.Networks
			run = func(ctx context.Context, attempt core.CheckJob) core.CheckResult {
				res := d.exec.RunCheck(ctx, attempt)
				if res.Err == nil && res.Status == core.CheckFailed && d.cfg.Services.AnyDead(ctx, ens) {
					res.Err = fmt.Errorf("service died mid-run")
				}
				return res
			}
		}
		res := d.cfg.FailureReview.Run(taskCtx, job, run)
		if res.Err == nil && res.Status == core.CheckSkipped {
			res.Status = core.CheckFailed
			res.Output += "\ngauntlet: input-based verification requires a passing execution; skipped is not evidence"
		}
		res.Command = job.Command
		res.Materialized += took
		stampConsumedImage(&res, job)
		return res
	}, cleanup)
}
