package workers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/testreport"
	"github.com/sgrankin/gauntlet/internal/verification"
	_ "modernc.org/sqlite"
)

var ErrCapacity = errors.New("worker backlog is full")

var ErrStale = errors.New("worker attempt was cancelled or reassigned")

type Broker struct {
	pools        []PoolStatus
	epoch        string
	waiters      map[string]int
	closed       bool
	mu           sync.Mutex
	db           *sql.DB
	now          func() time.Time
	born         time.Time
	lease, grace time.Duration
	changed      chan struct{}
}

func Open(path string, lease, reconnectGrace time.Duration, now func() time.Time) (*Broker, error) {
	if lease <= 0 || reconnectGrace < 0 {
		return nil, fmt.Errorf("invalid worker lease or reconnect grace")
	}
	if now == nil {
		now = time.Now
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS worker_jobs(id TEXT PRIMARY KEY, job BLOB NOT NULL, needs BLOB NOT NULL, priority INTEGER NOT NULL, created INTEGER NOT NULL, wanted INTEGER NOT NULL, state TEXT NOT NULL, root TEXT NOT NULL DEFAULT '', parent TEXT NOT NULL DEFAULT '', owner_epoch TEXT NOT NULL DEFAULT '', worker TEXT NOT NULL DEFAULT '', session TEXT NOT NULL DEFAULT '', token TEXT NOT NULL DEFAULT '', lease_until INTEGER NOT NULL DEFAULT 0, result BLOB);
 CREATE INDEX IF NOT EXISTS worker_ready ON worker_jobs(state,priority,created);
 CREATE INDEX IF NOT EXISTS worker_leases ON worker_jobs(worker,session,state);
 CREATE INDEX IF NOT EXISTS worker_runs ON worker_jobs(json_extract(job,'$.RunID'),json_extract(job,'$.MergeSHA'));
 CREATE INDEX IF NOT EXISTS worker_profiles ON worker_jobs(json_extract(job,'$.Executor'),state,priority,created);
 CREATE TABLE IF NOT EXISTS worker_peers(id TEXT PRIMARY KEY, session TEXT NOT NULL, seen INTEGER NOT NULL, data BLOB NOT NULL, draining INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Broker{epoch: randomID(), db: db, now: now, born: now(), lease: lease, grace: reconnectGrace, changed: make(chan struct{}), waiters: map[string]int{}}, nil
}

func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.notify()
	return b.db.Close()
}
func (b *Broker) notify() { close(b.changed); b.changed = make(chan struct{}) }

// Claim chooses work on the scheduler. Workers advertise capacity; they cannot
// choose a job by ID. Reassignment rotates the token atomically with its lease.
func (b *Broker) Claim(ctx context.Context, worker Worker) (*Attempt, error) {
	if worker.ID == "" || worker.Session == "" || worker.Capacity.Slots < 1 || worker.Capacity.Slots > 4096 || worker.Capacity.CPU <= 0 || worker.Capacity.Memory <= 0 || worker.Usage.Slots < 0 || worker.Usage.CPU < 0 || worker.Usage.Memory < 0 || worker.UrgentReserve < 0 || worker.UrgentReserve >= worker.Capacity.Slots {
		return nil, fmt.Errorf("invalid worker advertisement")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var session string
	var seen int64
	var draining bool
	err = tx.QueryRowContext(ctx, `SELECT session,seen,draining FROM worker_peers WHERE id=?`, worker.ID).Scan(&session, &seen, &draining)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil && session != worker.Session && now.Sub(time.UnixMilli(seen)) < b.lease {
		return nil, fmt.Errorf("worker identity is already active")
	}
	data, err := json.Marshal(worker)
	if err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO worker_peers(id,session,seen,data) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET session=excluded.session,seen=excluded.seen,data=excluded.data`, worker.ID, worker.Session, now.UnixMilli(), data); err != nil {
		return nil, err
	}
	available := worker.Capacity
	rows, err := tx.QueryContext(ctx, `SELECT needs FROM worker_jobs WHERE state='leased' AND worker=? AND session=?`, worker.ID, worker.Session)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			break
		}
		var req Requirements
		if err = json.Unmarshal(data, &req); err != nil {
			break
		}
		available = available.subtract(req.Resources)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	available.Slots = min(available.Slots, worker.Capacity.Slots-worker.Usage.Slots)
	available.CPU = min(available.CPU, worker.Capacity.CPU-worker.Usage.CPU)
	available.Memory = min(available.Memory, worker.Capacity.Memory-worker.Usage.Memory)
	if worker.Draining || draining {
		return nil, tx.Commit()
	}
	profiles, _ := json.Marshal(worker.Profiles)
	rows, err = tx.QueryContext(ctx, `SELECT id,job,needs,priority FROM worker_jobs WHERE json_extract(job,'$.Executor') IN (SELECT value FROM json_each(?)) AND ((state='pending' AND owner_epoch=?) OR (state='leased' AND owner_epoch=? AND lease_until<? AND ? >= ?)) ORDER BY priority DESC,created,id`, string(profiles), b.epoch, b.epoch, now.UnixMilli(), now.UnixMilli(), b.born.Add(b.grace).UnixMilli())
	if err != nil {
		return nil, err
	}
	var chosen *Attempt
	for rows.Next() {
		var id string
		var jobData, reqData []byte
		var priority int
		if err = rows.Scan(&id, &jobData, &reqData, &priority); err != nil {
			break
		}
		attempt := Attempt{ID: id, Worker: worker.ID}
		if err = json.Unmarshal(jobData, &attempt.Job); err != nil {
			break
		}
		if err = json.Unmarshal(reqData, &attempt.Requirements); err != nil {
			break
		}
		if !worker.accepts(attempt.Job, attempt.Requirements) || !attempt.Requirements.Resources.fits(available) {
			continue
		}
		if priority < 3 {
			after := available.subtract(attempt.Requirements.Resources)
			if after.Slots < worker.UrgentReserve || after.CPU < reservedShare(worker.Capacity.CPU, worker.Capacity.Slots, worker.UrgentReserve) || after.Memory < reservedShare(worker.Capacity.Memory, worker.Capacity.Slots, worker.UrgentReserve) {
				continue
			}
		}
		chosen = &attempt
		break
	}
	readErr = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	if chosen == nil {
		return nil, tx.Commit()
	}
	chosen.Token = randomID()
	chosen.LeaseUntil = now.Add(b.lease)
	chosen.LeaseMillis = b.lease.Milliseconds()
	if _, err = tx.ExecContext(ctx, `UPDATE worker_jobs SET state='leased',worker=?,session=?,token=?,lease_until=? WHERE id=?`, worker.ID, worker.Session, chosen.Token, chosen.LeaseUntil.UnixMilli(), chosen.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	b.notify()
	return chosen, nil
}

func (b *Broker) Renew(ctx context.Context, id, token string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	result, err := b.db.ExecContext(ctx, `UPDATE worker_jobs SET lease_until=? WHERE id=? AND token=? AND state='leased'`, b.now().Add(b.lease).UnixMilli(), id, token)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 1 {
		_, err = b.db.ExecContext(ctx, `UPDATE worker_peers SET seen=? WHERE (id,session) IN (SELECT worker,session FROM worker_jobs WHERE id=? AND token=?)`, b.now().UnixMilli(), id, token)
	}
	if err == nil && n != 1 {
		return ErrStale
	}
	return err
}

// Complete accepts a late result if the lease was not reassigned. Expiration
// permits replacement, but does not discard useful completed work by itself.
func (b *Broker) Complete(ctx context.Context, id, token string, result Result) error {
	if result.Check.Err != nil {
		return fmt.Errorf("wire result contains an error interface")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var jobData, parentData []byte
	var storedToken, state, worker, parent string
	if err := b.db.QueryRowContext(ctx, `SELECT job,state,token,worker,parent FROM worker_jobs WHERE id=?`, id).Scan(&jobData, &state, &storedToken, &worker, &parent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrStale
		}
		return err
	}
	if token == "" || token != storedToken {
		return ErrStale
	}
	if state == "completed" {
		return nil
	}
	if state != "leased" {
		return ErrStale
	}
	var job core.CheckJob
	if err := json.Unmarshal(jobData, &job); err != nil {
		return err
	}
	if len(result.Check.Output) > 65536 || len(result.Error) > 4096 || len(result.Check.Receipt) > 1<<20 || len(result.Check.InputsKey) > 4096 {
		return fmt.Errorf("worker result exceeds bounds")
	}
	result.Check.Name, result.Check.Command = job.Name, job.Command
	result.Check.ExecutedRun, result.Check.ExecutedSHA = job.RunID, job.MergeSHA
	result.Check.ExecutionID, result.Check.Worker = id, worker
	result.Check.VerificationID = job.VerificationID
	result.Check.Reused = false
	if result.Check.Status != core.CheckPassed && result.Check.Status != core.CheckFailed && result.Check.Status != core.CheckSkipped {
		return fmt.Errorf("invalid worker verdict")
	}
	if job.TestResults != "" && result.Error == "" && result.Check.Status == core.CheckPassed {
		if err := testreport.Validate(result.Check.Tests); err != nil || len(testreport.Failed(result.Check.Tests)) > 0 {
			return fmt.Errorf("passing result lacks passing tests")
		}
	}
	raw := slices.Clone(result.Check.Tests)
	result.Check.TestAttempts = nil
	if parent != "" {
		if err := b.db.QueryRowContext(ctx, `SELECT result FROM worker_jobs WHERE id=? AND state='completed'`, parent).Scan(&parentData); err != nil {
			return fmt.Errorf("retry predecessor unavailable: %w", err)
		}
		var previous Result
		if err := json.Unmarshal(parentData, &previous); err != nil {
			return err
		}
		result.Check.TestAttempts = previous.Check.TestAttempts
		result.Check.Duration += previous.Check.Duration
		result.Check.Output = previous.Check.Output + "\n--- worker retry ---\n" + result.Check.Output
		result.Check.UserCPU += previous.Check.UserCPU
		result.Check.SysCPU += previous.Check.SysCPU
		result.Check.PeakRSS = max(result.Check.PeakRSS, previous.Check.PeakRSS)
		result.Check.Materialized += previous.Check.Materialized
		if len(previous.Check.Tests) > 0 && result.Error == "" {
			requested := job.RetryTests
			if len(requested) == 0 {
				for _, test := range previous.Check.Tests {
					if test.Status != "skipped" {
						requested = append(requested, test.ID)
					}
				}
			}
			merged, err := testreport.MergeRerun(previous.Check.Tests, result.Check.Tests, requested)
			result.Check.Tests = merged
			if err != nil {
				result.Check.Status = core.CheckFailed
				result.Check.Output += "\ngauntlet: " + err.Error()
			}
		}
	}
	attemptLog := result.AttemptLogPath
	if attemptLog == "" {
		attemptLog = result.Check.LogPath
	}
	result.Check.TestAttempts = append(result.Check.TestAttempts, core.TestAttempt{LogDigest: result.LogDigest, ExecutionID: id, Worker: worker, Number: len(result.Check.TestAttempts) + 1, Command: job.Command, LogPath: attemptLog, Tests: raw})
	if len(result.Check.Output) > 65536 {
		result.Check.Output = result.Check.Output[len(result.Check.Output)-65536:]
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = b.db.ExecContext(ctx, `UPDATE worker_jobs SET result=?,state='completed' WHERE id=? AND token=? AND state='leased'`, data, id, token)
	if err == nil {
		b.notify()
	}
	return err
}

func (b *Broker) Cancel(ctx context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.db.ExecContext(ctx, `UPDATE worker_jobs SET state='cancelled' WHERE id=? AND state IN ('pending','leased')`, id)
	if err == nil {
		b.notify()
	}
	return err
}

func (b *Broker) Wait(ctx context.Context, id string) (Result, error) {
	b.mu.Lock()
	b.waiters[id]++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.waiters[id]--
		if b.waiters[id] == 0 {
			delete(b.waiters, id)
		}
		b.mu.Unlock()
	}()
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return Result{}, fmt.Errorf("worker broker closed")
		}
		var data []byte
		var state string
		err := b.db.QueryRowContext(ctx, `SELECT state,result FROM worker_jobs WHERE id=?`, id).Scan(&state, &data)
		changed := b.changed
		b.mu.Unlock()
		if err != nil {
			return Result{}, err
		}
		if state == "cancelled" {
			return Result{}, ErrStale
		}
		if state == "completed" {
			var result Result
			err = json.Unmarshal(data, &result)
			return result, err
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-changed:
		}
	}
}

// Submit recovers the latest authorized attempt in an exact-SHA check's retry
// chain. A resumed retry does not spend another retry budget or run twice.
func (b *Broker) Submit(ctx context.Context, root string, job core.CheckJob, needs Requirements, priority int) (string, error) {
	if root == "" || len(job.Command) == 0 || needs.Resources.Slots < 1 || needs.Resources.CPU < 0 || needs.Resources.Memory < 0 {
		return "", fmt.Errorf("invalid worker job")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	parent := job.PreviousExecution
	if parent != "" {
		var state string
		if err = tx.QueryRowContext(ctx, `SELECT root,state FROM worker_jobs WHERE id=?`, parent).Scan(&root, &state); err != nil {
			return "", err
		}
		if root == "" || state != "completed" {
			return "", fmt.Errorf("retry requires a completed predecessor")
		}
	} else {
		var latest, state string
		err = tx.QueryRowContext(ctx, `SELECT id,state FROM worker_jobs WHERE root=? ORDER BY rowid DESC LIMIT 1`, root).Scan(&latest, &state)
		if err == nil {
			if state == "cancelled" {
				var active int
				if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_jobs WHERE state IN ('pending','leased')`).Scan(&active); err != nil {
					return "", err
				}
				if active >= 10000 {
					return "", ErrCapacity
				}
				if _, err = tx.ExecContext(ctx, `UPDATE worker_jobs SET state='pending',token='',lease_until=0,worker='',session='' WHERE id=?`, latest); err != nil {
					return "", err
				}
			}
			_, err = tx.ExecContext(ctx, `UPDATE worker_jobs SET wanted=?,owner_epoch=? WHERE id=?`, b.now().UnixMilli(), b.epoch, latest)
			if err != nil {
				return "", err
			}
			return latest, tx.Commit()
		}
		if err != sql.ErrNoRows {
			return "", err
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worker_jobs WHERE state IN ('pending','leased')`).Scan(&count); err != nil {
		return "", err
	}
	if count >= 10000 {
		return "", ErrCapacity
	}
	id := verification.Key("remote-attempt-v1", root, parent, job.Command, job.RetryTests)
	data, err := json.Marshal(job)
	if err != nil {
		return "", err
	}
	requirements, err := json.Marshal(needs)
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO worker_jobs(id,job,needs,priority,created,wanted,state,root,parent,owner_epoch) VALUES(?,?,?,?,?,?,'pending',?,?,?) ON CONFLICT(id) DO UPDATE SET wanted=excluded.wanted,priority=excluded.priority,owner_epoch=excluded.owner_epoch`, id, data, requirements, priority, b.now().UnixMilli(), b.now().UnixMilli(), root, parent, b.epoch)
	if err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	b.notify()
	return id, nil
}

func (b *Broker) forgetCompleted(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waiters[id] > 0 {
		return
	}
	b.db.Exec(`DELETE FROM worker_jobs WHERE id=? AND state='completed'`, id)
}
