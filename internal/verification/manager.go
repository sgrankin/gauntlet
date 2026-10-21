// Package verification owns check executions independently of queue attempts.
package verification

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
	_ "modernc.org/sqlite"
)

const Retention = 24 * time.Hour
const OrphanGrace = 2 * time.Minute

// Key includes the caller's execution environment and repository-declared inputs.
func Key(values ...any) string {
	data, err := json.Marshal(values)
	if err != nil {
		panic(err)
	} // Callers supply only concrete JSON data.
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

type execution struct {
	done     chan struct{}
	cancel   context.CancelFunc
	result   core.CheckResult
	waiters  int
	orphaned time.Time
}

type Manager struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	db     *sql.DB
	active map[string]*execution
	closed bool
}

// Open recovers passing evidence. Unfinished local executions run again after
// restarting the scheduler.
func Open(ctx context.Context, path string) (*Manager, error) {
	if path == "" {
		path = ":memory:"
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS evidence (identity TEXT PRIMARY KEY, at INTEGER NOT NULL, result BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS evidence_at ON evidence(at);`); err != nil {
		db.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, db: db, active: map[string]*execution{}}, nil
}

// Run either attaches to matching work or starts it. Ownership of cleanup passes
// to Run unconditionally; it covers resources reserved before computing the key.
// Cancelling a waiter detaches it, without cancelling a useful shared execution.
func (m *Manager) Run(ctx context.Context, key string, job core.CheckJob, run func(context.Context) core.CheckResult, cleanup func()) core.CheckResult {
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil || ctx.Err() != nil {
		m.mu.Unlock()
		cleanup()
		return core.CheckResult{Name: job.Name, Err: context.Canceled}
	}
	e := m.active[key]
	if e == nil {
		var data []byte
		err := m.db.QueryRowContext(ctx, `SELECT result FROM evidence WHERE identity=? AND at>=?`, key, time.Now().Add(-Retention).UnixMilli()).Scan(&data)
		if err == nil {
			var result core.CheckResult
			if err = json.Unmarshal(data, &result); err == nil && result.Status == core.CheckPassed && result.VerificationID == key {
				m.mu.Unlock()
				cleanup()
				result.Reused = true
				return result
			}
			if err == nil {
				err = errors.New("invalid stored passing evidence")
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			m.mu.Unlock()
			cleanup()
			return core.CheckResult{Name: job.Name, Err: fmt.Errorf("verification evidence: %w", err)}
		}
		taskCtx, cancel := context.WithCancel(m.ctx)
		e = &execution{done: make(chan struct{}), cancel: cancel}
		m.active[key] = e
		m.wg.Add(1)
		go m.execute(taskCtx, key, e, job, run, cleanup)
	} else {
		cleanup()
	}
	e.waiters++
	e.orphaned = time.Time{}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		m.mu.Lock()
		e.waiters--
		if e.waiters == 0 {
			e.orphaned = time.Now()
		}
		m.mu.Unlock()
		return core.CheckResult{Name: job.Name, Err: ctx.Err()}
	case <-e.done:
		m.mu.Lock()
		e.waiters--
		result := e.result
		m.mu.Unlock()
		result.Reused = result.ExecutedRun != job.RunID || result.ExecutedSHA != job.MergeSHA
		return result
	}
}

func (m *Manager) execute(ctx context.Context, key string, e *execution, job core.CheckJob, run func(context.Context) core.CheckResult, cleanup func()) {
	defer m.wg.Done()
	defer e.cancel()
	result := run(ctx)
	if ctx.Err() != nil {
		result.Err = ctx.Err()
	}
	cleanup()
	result.VerificationID, result.ExecutedRun, result.ExecutedSHA = key, job.RunID, job.MergeSHA
	// Errors are not evidence; encoding an interface-valued error would also make
	// the persisted record impossible to decode reliably.
	m.mu.Lock()
	if ctx.Err() == nil && result.Err == nil && result.Status == core.CheckPassed {
		data, err := json.Marshal(result)
		if err == nil {
			_, err = m.db.ExecContext(ctx, `INSERT OR REPLACE INTO evidence(identity,at,result) VALUES(?,?,?)`, key, time.Now().UnixMilli(), data)
		}
		if err != nil {
			result.Err = fmt.Errorf("persist verification evidence: %w", err)
		}
	}
	e.result = result
	if m.active[key] == e {
		delete(m.active, key)
	}
	close(e.done)
	m.mu.Unlock()
}

// Maintain bounds retained evidence and cancels work that no reconstructed run
// adopted within the grace period. Call from the scheduler's reconcile loop.
func (m *Manager) Maintain(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, e := range m.active {
		if e.waiters == 0 && !e.orphaned.IsZero() && now.Sub(e.orphaned) >= OrphanGrace {
			e.cancel()
			delete(m.active, key)
		}
	}
	_, err := m.db.Exec(`DELETE FROM evidence WHERE at<? OR identity IN (SELECT identity FROM evidence ORDER BY at DESC LIMIT -1 OFFSET 10000)`, now.Add(-Retention).UnixMilli())
	return err
}

func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
	return m.db.Close()
}
