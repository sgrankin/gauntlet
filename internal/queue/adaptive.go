package queue

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
)

type Governor struct {
	Limit         int
	Clean         int
	Landed        int64
	Retried       int64
	Failed        int64
	Invalidated   int64
	UsefulSeconds float64
	WastedSeconds float64
	Reason        string
	At            time.Time
	Recent        []string
}
type Scheduling struct {
	Admission       int       `json:"admission"`
	CapacityLimited bool      `json:"capacityLimited"`
	Mode            string    `json:"mode"`
	Limit           int       `json:"limit"`
	Ceiling         int       `json:"ceiling"`
	Clean           int       `json:"clean"`
	Landed          int64     `json:"landed"`
	Retried         int64     `json:"retried"`
	Failed          int64     `json:"failed"`
	Invalidated     int64     `json:"invalidated"`
	UsefulSeconds   float64   `json:"usefulSeconds"`
	WastedSeconds   float64   `json:"wastedSeconds"`
	Reason          string    `json:"reason"`
	At              time.Time `json:"at"`
}

func schedulingCeiling(t config.Target) int {
	if t.Mode == "batch" {
		return max(1, t.MaxBatch)
	}
	return max(1, t.Window)
}
func (d *Daemon) governor(t config.Target) Governor {
	g := d.controls.Governors[t.Name]
	if g.Limit == 0 {
		g.Limit = max(1, t.Adaptive.Start)
		g.Reason = "starting conservatively"
	}
	g.Limit = min(schedulingCeiling(t), max(1, g.Limit))
	return g
}
func (d *Daemon) adaptiveLimit(ctx context.Context, t config.Target) (limit int) {
	defer func() {
		if d.admission == nil {
			d.admission = map[string]int{}
		}
		d.admission[t.Name] = limit
	}()
	ceiling := schedulingCeiling(t)
	if t.Adaptive == nil {
		return ceiling
	}
	g := d.governor(t)
	pressure := d.cfg.Slots.Saturated()
	if d.cfg.ExecutionPressure != nil && d.cfg.ExecutionPressure(ctx) {
		pressure = true
	}
	if pressure {
		// Limit only new admission. Never cancel an existing trial to enforce a
		// lower window; a queued remote check still wakes a zero-sized pool.
		return min(g.Limit, max(1, t.Adaptive.Start))
	}
	return g.Limit
}
func (d *Daemon) schedulingSnapshot(t config.Target) *Scheduling {
	if t.Adaptive == nil {
		return nil
	}
	g := d.governor(t)
	admission := d.admission[t.Name]
	if admission == 0 {
		admission = g.Limit
	}
	admission = min(admission, g.Limit)
	return &Scheduling{Admission: admission, CapacityLimited: admission < g.Limit, Mode: t.Mode, Limit: g.Limit, Ceiling: schedulingCeiling(t), Clean: g.Clean, Landed: g.Landed, Retried: g.Retried, Failed: g.Failed, Invalidated: g.Invalidated, UsefulSeconds: g.UsefulSeconds, WastedSeconds: g.WastedSeconds, Reason: g.Reason, At: g.At}
}
func (d *Daemon) governorFeedback(t config.Target, r *run, outcome core.Outcome) {
	if t.Adaptive == nil || r.emergencyReason != "" {
		return
	}
	g := d.governor(t)
	feedbackID := r.runID + "/" + strconv.Itoa(int(outcome))
	if slices.Contains(g.Recent, feedbackID) {
		return
	}
	spent, wasted := 0.0, 0.0
	retried := false
	executed := false
	for _, result := range r.results {
		if result.Reused {
			continue
		}
		if result.Status != core.CheckSkipped {
			executed = true
		}
		cost := result.Duration.Seconds()
		if cpu := result.UserCPU + result.SysCPU; cpu > 0 {
			cost = cpu.Seconds()
		}
		spent += max(0, cost)
		if result.VerificationID == "" {
			wasted += max(0, cost)
		}
		retried = retried || len(result.TestAttempts) > 1
	}
	wasted += r.abandonedSeconds
	if outcome == core.OutcomeSkipped && wasted == 0 {
		return
	}

	switch {
	case outcome == core.OutcomeLanded:
		g.Landed += int64(len(r.members))
		g.UsefulSeconds += spent
		if retried {
			g.Retried++
			g.Clean = 0
			g.Reason = "retry recovered; holding concurrency"
		} else if executed {
			g.Clean++
			g.Reason = "clean landing"
			if g.Clean >= max(1, t.Adaptive.Successes) {
				g.Limit = min(schedulingCeiling(t), g.Limit+1)
				g.Clean = 0
				g.Reason = "clean landings increased concurrency"
			}
		} else {
			g.Reason = "reused evidence; holding concurrency"
		}
	case outcome == core.OutcomeRejected || outcome == core.OutcomeError:
		// Rejections from authorization and policy are not check-health evidence.
		if r.verdict != verdictRejected && r.verdict != verdictErrored {
			return
		}
		g.Failed++
		g.WastedSeconds += wasted
		g.Clean = 0
		g.Limit = max(1, g.Limit/2)
		g.Reason = "failed validation reduced concurrency"
	case outcome == core.OutcomeSkipped:
		g.Invalidated++
		g.WastedSeconds += wasted
		g.Clean = 0
		g.Limit = max(1, g.Limit/2)
		g.Reason = "invalidated work reduced concurrency"
	default:
		return
	}
	g.At = d.now()
	g.Recent = append(slices.Clone(g.Recent), feedbackID)
	if len(g.Recent) > 128 {
		g.Recent = g.Recent[len(g.Recent)-128:]
	}
	next := d.controls.clone()
	if next.Governors == nil {
		next.Governors = map[string]Governor{}
	}
	next.Governors[t.Name] = g
	d.saveControls(next)
}
