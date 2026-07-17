package queue

import (
	"time"
)

// Lifecycle is the daemon's shutdown-lifecycle phase (issue #8), surfaced
// in the Snapshot for status/readiness. Forcing (a second signal or the
// deadline) is cmd's cancellation of the root context, observed by the
// daemon only as its Run returning — so the queue itself distinguishes
// three phases: running, draining (admission stopped, finite set still
// finishing), drained (the set is empty; Run is about to exit).
type Lifecycle string

const (
	LifecycleRunning  Lifecycle = "running"
	LifecycleDraining Lifecycle = "draining"
	LifecycleDrained  Lifecycle = "drained"
)

// Drain requests a graceful drain: stop admitting new candidates and
// extending speculation on the next tick, let the already-admitted set
// finish (checks + one landing CAS each), then exit. Safe to call from any
// goroutine (a signal handler, the HTTP admin surface). Idempotent: a
// repeat request never resumes admission, and only ever SHORTENS the
// deadline (an earlier forcing instant wins) — it can't be pushed out.
// deadline is the zero value for "no queue-level deadline" (cmd still owns
// the actual force via ctx cancellation); a non-zero deadline is advisory
// state for observers here.
func (d *Daemon) Drain(deadline time.Time) {
	d.drainReqMu.Lock()
	defer d.drainReqMu.Unlock()
	d.drainReq = true
	if !deadline.IsZero() && (d.drainReqDL.IsZero() || deadline.Before(d.drainReqDL)) {
		d.drainReqDL = deadline
	}
}

// syncDrainRequest folds any external Drain request into the reconcile-
// goroutine-only live state, stamping drainSince from the injected clock
// on the transition. Called at the top of every ReconcileOnce so admission
// gating and the Snapshot see a consistent view for the whole tick.
func (d *Daemon) syncDrainRequest() {
	d.drainReqMu.Lock()
	req, dl := d.drainReq, d.drainReqDL
	d.drainReqMu.Unlock()
	if !req {
		return
	}
	if !d.draining {
		d.draining = true
		d.drainSince = d.now()
	}
	if !dl.IsZero() && (d.drainDeadline.IsZero() || dl.Before(d.drainDeadline)) {
		d.drainDeadline = dl
	}
}

// activeRuns and activeChecks count the in-flight drain set — runs still in
// some lane, and checks still executing within them. Reconcile-goroutine-
// only (reads d.lanes). Used both for the Snapshot's status fields and for
// the drained-complete test.
func (d *Daemon) activeRuns() int {
	n := 0
	for _, l := range d.lanes {
		n += len(l.runs)
	}
	return n
}

func (d *Daemon) activeChecks() int {
	n := 0
	for _, l := range d.lanes {
		for _, r := range l.runs {
			n += len(r.inflight)
		}
	}
	return n
}

// drainComplete reports that a requested drain has emptied its finite set —
// every admitted run has reached a terminal outcome and left its lane — so
// the Run loop may exit cleanly. Only meaningful while draining.
func (d *Daemon) drainComplete() bool {
	return d.draining && d.activeRuns() == 0
}

// lifecycle derives the current phase for the Snapshot.
func (d *Daemon) lifecycle() Lifecycle {
	switch {
	case !d.draining:
		return LifecycleRunning
	case d.activeRuns() == 0:
		return LifecycleDrained
	default:
		return LifecycleDraining
	}
}
