package flaky

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/sgrankin/gauntlet/internal/core"
)

type Classifier interface {
	Classify(context.Context, core.CheckJob, core.CheckResult) (Decision, error)
}

// Retrier stays inside one check worker, retaining the tested revision,
// workspace, services, and execution slot until a final result is available.
// It must only be configured for commands whose effects are safe to repeat.
type Retrier struct {
	History       *History
	MaxRunRetries int
	Classifier    Classifier
	Model         string
	Checks        []string
	MaxRetries    int
	MinConfidence float64
	Timeout       time.Duration
}

func (r *Retrier) Run(ctx context.Context, job core.CheckJob, run func(context.Context, core.CheckJob) core.CheckResult) core.CheckResult {
	start := time.Now()
	res := run(ctx, job)
	if r == nil || r.Classifier == nil || job.OperatorOwned || job.ImageBuild || job.ReceiptCapture || !slices.Contains(r.Checks, job.Name) || res.Err != nil || res.Status != core.CheckFailed || ctx.Err() != nil {
		return res
	}
	var audit strings.Builder
	var suspects []string
	var kind string
	canonical := res.LogPath
	totalCPUUser, totalCPUSys, peak := res.UserCPU, res.SysCPU, res.PeakRSS
	for attempt := 1; ; attempt++ {
		fmt.Fprintf(&audit, "\n--- attempt %d: %s ---\n%s\n", attempt, resultWord(res), tail(res.Output, 12000))
		if res.Err != nil || res.Status != core.CheckFailed || ctx.Err() != nil {
			break
		}
		if attempt > r.MaxRetries {
			audit.WriteString("gauntlet failure review: retry budget exhausted; keeping failure.\n")
			break
		}
		cctx, cancel := context.WithTimeout(ctx, r.Timeout)
		decision, err := r.Classifier.Classify(cctx, job, res)
		if cctx.Err() != nil {
			err = cctx.Err()
		}
		cancel()
		if err == nil {
			err = decision.Validate()
		}
		if err != nil {
			if e := r.History.Record(ctx, job, res.Output, decision, err, false, false); e != nil {
				log.Printf("failure history: %v", e)
			}
			note := fmt.Sprintf("gauntlet failure review: decision unavailable or invalid (%v); keeping failure.\n", err)
			audit.WriteString(note)
			appendNote(canonical, note)
			break
		}
		kind = decision.Kind
		for _, ref := range decision.Suspects {
			for _, candidate := range job.Candidates {
				if candidate.Ref == ref && !slices.Contains(suspects, ref) {
					suspects = append(suspects, ref)
				}
			}
		}
		if len(suspects) > 0 {
			fmt.Fprintf(&audit, "gauntlet investigation: UNCONFIRMED suspects=%q evidence=%q\n", suspects, decision.Evidence)
		}
		retry := decision.Action == "retry" && decision.Confidence >= r.MinConfidence && ctx.Err() == nil
		note := fmt.Sprintf("gauntlet failure review: model=%q %s confidence=%.3f threshold=%.3f; %s; retry=%t\n", r.Model, decision.Action, decision.Confidence, r.MinConfidence, strings.Join(strings.Fields(decision.Reason), " "), retry)
		audit.WriteString(note)
		appendNote(canonical, note)
		if !retry {
			if err := r.History.Record(ctx, job, res.Output, decision, nil, false, false); err != nil {
				log.Printf("failure history: %v", err)
			}
			break
		}
		if r.MaxRunRetries > 0 {
			allowed, err := r.History.Reserve(ctx, job.RunID, r.MaxRunRetries)
			if err != nil || !allowed {
				audit.WriteString("gauntlet failure review: run-wide retry budget exhausted or unavailable; keeping failure.\n")
				break
			}
		}
		failedOutput := res.Output
		nextJob := job
		if job.LogPath != "" {
			nextJob.LogPath = strings.TrimSuffix(job.LogPath, ".log.zst") + fmt.Sprintf(".attempt-%d.log.zst", attempt+1)
		}
		res = run(ctx, nextJob)
		if res.Err == nil && res.Status == core.CheckSkipped {
			res.Status = core.CheckFailed
			res.Output += "\ngauntlet: a skipped retry cannot clear the original failure."
		}
		if err := r.History.Record(ctx, job, failedOutput, decision, nil, true, res.Err == nil && res.Status == core.CheckPassed); err != nil {
			log.Printf("failure history: %v", err)
		}
		totalCPUUser += res.UserCPU
		totalCPUSys += res.SysCPU
		peak = max(peak, res.PeakRSS)
		if canonical == "" {
			canonical = res.LogPath
		} else if res.LogPath != "" {
			appendNote(canonical, fmt.Sprintf("\n--- retry attempt %d ---\n", attempt+1))
			appendLog(canonical, res.LogPath)
		}
	}
	res.SuspectedRefs = suspects
	res.FailureKind = kind
	res.Output = tail(audit.String(), 65536)
	res.LogPath = canonical
	res.Duration = time.Since(start)
	res.UserCPU, res.SysCPU, res.PeakRSS = totalCPUUser, totalCPUSys, peak
	return res
}

// Concatenated zstd frames keep the original log URL valid across retries.
// Individual attempt files remain available if appending fails; all files
// live under the run directory and follow its existing retention policy.
func appendNote(path, note string) {
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		log.Printf("failure review: append log note: %v", err)
		return
	}
	defer file.Close()
	enc, err := zstd.NewWriter(file, zstd.WithEncoderConcurrency(1))
	if err != nil {
		log.Printf("failure review: encode log note: %v", err)
		return
	}
	_, writeErr := io.WriteString(enc, note)
	closeErr := enc.Close()
	if writeErr != nil || closeErr != nil {
		log.Printf("failure review: log note write failed")
	}
}

func appendLog(dst, src string) {
	if dst == src {
		return
	}
	source, err := os.Open(src)
	if err != nil {
		log.Printf("failure review: read attempt log: %v", err)
		return
	}
	defer source.Close()
	file, err := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		log.Printf("failure review: append attempt log: %v", err)
		return
	}
	defer file.Close()
	if _, err := io.Copy(file, source); err != nil {
		log.Printf("failure review: append attempt log: %v", err)
	}
}

func resultWord(res core.CheckResult) string {
	if res.Err != nil {
		return "error"
	}
	switch res.Status {
	case core.CheckPassed:
		return "passed"
	case core.CheckSkipped:
		return "skipped"
	case core.CheckWaived:
		return "waived"
	case core.CheckBlocked:
		return "blocked"
	default:
		return "failed"
	}
}
