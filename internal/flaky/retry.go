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
	"github.com/sgrankin/gauntlet/internal/policy"
	"github.com/sgrankin/gauntlet/internal/testreport"
)

type Classifier interface {
	Classify(context.Context, core.CheckJob, core.CheckResult) (Decision, error)
}

// Retrier stays inside one check worker, retaining the tested revision,
// workspace, services, and execution slot until a final result is available.
// It must only be configured for commands whose effects are safe to repeat.
type Retrier struct {
	KnownFlakeRetries int
	Policy            *policy.Engine
	History           *History
	MaxRunRetries     int
	Classifier        Classifier
	Model             string
	Checks            []string
	MaxRetries        int
	MinConfidence     float64
	Timeout           time.Duration
}

func (r *Retrier) Run(ctx context.Context, job core.CheckJob, run func(context.Context, core.CheckJob) core.CheckResult) core.CheckResult {
	start := time.Now()
	res := run(ctx, job)
	if r == nil || r.Classifier == nil || job.OperatorOwned || job.ImageBuild || job.ReceiptCapture || job.InputsCapture || !slices.Contains(r.Checks, job.Name) || res.Err != nil || res.Status != core.CheckFailed || ctx.Err() != nil {
		return res
	}
	attempts := []core.TestAttempt{{Number: 1, Command: slices.Clone(job.Command), LogPath: res.LogPath, Tests: slices.Clone(res.Tests)}}
	knownUsed := 0
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
		var decision Decision
		var known bool
		var err error
		if knownUsed < r.KnownFlakeRetries {
			decision, known, err = r.History.KnownFlake(cctx, job, res)
		}
		if err != nil || !known {
			decision, err = r.Classifier.Classify(cctx, job, res)
			known = false
		} else {
			knownUsed++
		}
		if cctx.Err() != nil {
			err = cctx.Err()
		}
		cancel()
		if err == nil {
			err = decision.Validate()
		}
		if err != nil {
			if e := r.History.Record(ctx, job, res.Output, decision, err, false, false, res.Tests); e != nil {
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
		engine := r.Policy
		if engine == nil {
			engine = policy.Default()
		}
		source, model := "codex", r.Model
		if known {
			source, model = "history", "observed-history"
		}
		verdict, policyErr := engine.Decide(ctx, "retry", policy.Input{SchemaVersion: 1, Phase: "retry", Retry: map[string]any{"action": decision.Action, "confidence": decision.Confidence, "min_confidence": r.MinConfidence, "kind": decision.Kind, "check": job.Name, "attempt": attempt, "reason": decision.Reason, "evidence": decision.Evidence, "source": source, "test_ids": testreport.Failed(res.Tests), "verification_id": job.VerificationID}})
		retry := policyErr == nil && verdict.Allow && ctx.Err() == nil
		if policyErr != nil {
			fmt.Fprintf(&audit, "gauntlet retry policy unavailable: %v\n", policyErr)
		} else if !verdict.Allow {
			fmt.Fprintf(&audit, "gauntlet retry policy: %s\n", verdict.Reason())
		}
		note := fmt.Sprintf("gauntlet failure review: model=%q %s confidence=%.3f threshold=%.3f; %s; retry=%t\n", model, decision.Action, decision.Confidence, r.MinConfidence, strings.Join(strings.Fields(decision.Reason), " "), retry)
		audit.WriteString(note)
		appendNote(canonical, note)
		if !retry {
			if err := r.History.Record(ctx, job, res.Output, decision, nil, false, false, res.Tests); err != nil {
				log.Printf("failure history: %v", err)
			}
			break
		}
		if r.MaxRunRetries > 0 {
			var allowed bool
			var err error
			if job.VerificationID != "" {
				allowed, err = r.History.ReserveEvidence(ctx, job.RunID, job.VerificationID, r.MaxRunRetries, r.MaxRetries)
			} else {
				allowed, err = r.History.Reserve(ctx, job.RunID, r.MaxRunRetries)
			}
			if err != nil || !allowed {
				audit.WriteString("gauntlet failure review: run-wide retry budget exhausted or unavailable; keeping failure.\n")
				break
			}
		}
		failedOutput := res.Output
		previousTests := res.Tests
		nextJob := job
		if job.LogPath != "" {
			nextJob.LogPath = strings.TrimSuffix(job.LogPath, ".log.zst") + fmt.Sprintf(".attempt-%d.log.zst", attempt+1)
		}
		requested := testreport.Failed(previousTests)
		if len(job.RetryCommand) > 0 && len(requested) > 0 {
			nextJob.Command = slices.Clone(job.RetryCommand)
			nextJob.RetryTests = slices.Clone(requested)
		} else {
			requested = nil
			for _, test := range previousTests {
				if test.Status != "skipped" {
					requested = append(requested, test.ID)
				}
			}
		}
		res = run(ctx, nextJob)
		attempts = append(attempts, core.TestAttempt{Number: attempt + 1, Command: slices.Clone(nextJob.Command), LogPath: res.LogPath, Tests: slices.Clone(res.Tests)})
		if len(previousTests) > 0 && res.Err == nil {
			merged, coverageErr := testreport.MergeRerun(previousTests, res.Tests, requested)
			res.Tests = merged
			if coverageErr != nil {
				res.Status = core.CheckFailed
				res.Output += "\ngauntlet: " + coverageErr.Error()
			}
		}
		if res.Err == nil && res.Status == core.CheckSkipped {
			res.Status = core.CheckFailed
			res.Output += "\ngauntlet: a skipped retry cannot clear the original failure."
		}
		if err := r.History.Record(ctx, job, failedOutput, decision, nil, true, res.Err == nil && res.Status == core.CheckPassed, previousTests); err != nil {
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
	res.TestAttempts = attempts
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
