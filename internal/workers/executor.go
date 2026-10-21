package workers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/verification"
)

type Executor struct {
	Broker   *Broker
	Root     context.Context
	Context  string
	Profiles map[string]Requirements
}

func (e *Executor) RunCheck(ctx context.Context, job core.CheckJob) core.CheckResult {
	failure := func(err error) core.CheckResult { return core.CheckResult{Name: job.Name, Err: err} }
	if job.OperatorOwned {
		return failure(fmt.Errorf("operator commands require a local executor"))
	}
	if job.Image != "" && !strings.Contains(job.Image, "@sha256:") {
		return failure(fmt.Errorf("remote image consumers require a registry digest reference"))
	}
	needs, ok := e.Profiles[job.Executor]
	if !ok {
		return failure(fmt.Errorf("remote profile is not configured"))
	}
	root := verification.Key("remote-root-v1", e.Context, job.RunID, job.MergeSHA, job.Name, job.Executor, job.Image, job.InputsCapture, job.ImageBuild, job.ReceiptCapture)
	if job.InputsCapture {
		root = verification.Key(root, randomID())
	}
	priority := 1
	if job.Priority == "low" {
		priority = 0
	}
	if job.Priority == "high" {
		priority = 2
	}
	if job.Priority == "urgent" {
		priority = 3
	}
	wire := job
	wire.Dir, wire.GitDir, wire.LogPath = "", "", ""
	wire.RecordLogs = job.RecordLogs || job.LogPath != ""
	wire.Candidate.Message = ""
	wire.Candidates = slices.Clone(job.Candidates)
	for i := range wire.Candidates {
		wire.Candidates[i].Message = ""
	}
	id, err := e.Broker.Submit(ctx, root, wire, needs, priority)
	for errors.Is(err, ErrCapacity) {
		select {
		case <-ctx.Done():
			return failure(ctx.Err())
		case <-time.After(time.Second):
		}
		id, err = e.Broker.Submit(ctx, root, wire, needs, priority)
	}
	if err != nil {
		return failure(err)
	}
	result, err := e.Broker.Wait(ctx, id)
	if err != nil {
		if ctx.Err() != nil && (e.Root == nil || e.Root.Err() == nil) {
			cancelCtx, cancel := context.WithTimeout(context.Background(), e.Broker.lease)
			defer cancel()
			_ = e.Broker.cancelUnwatched(cancelCtx, id)
		}
		return failure(err)
	}
	if job.InputsCapture {
		e.Broker.forgetCompleted(id)
	}
	check := result.CheckResult()
	check.Reused = check.ExecutedRun != job.RunID || check.ExecutedSHA != job.MergeSHA
	return check
}
