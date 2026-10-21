package flaky

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/sgrankin/gauntlet/internal/core"
	_ "modernc.org/sqlite"
	"slices"
	"strings"
	"time"
)

type History struct{ db *sql.DB }

type Observation struct {
	At                                time.Time `json:"at"`
	Run, Check, Revision, Fingerprint string
	Decision                          Decision
	Error                             string `json:"error,omitempty"`
	Retried, Passed                   bool
	TestIDs                           []string
	Output                            string
}

func OpenHistory(path string) (*History, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS failure_review(id INTEGER PRIMARY KEY, at INTEGER NOT NULL, fingerprint TEXT NOT NULL, check_name TEXT NOT NULL, data TEXT NOT NULL); CREATE INDEX IF NOT EXISTS failure_review_check ON failure_review(check_name,id); CREATE TABLE IF NOT EXISTS retry_budget(run TEXT PRIMARY KEY, used INTEGER NOT NULL, at INTEGER NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &History{db}, nil
}
func (h *History) Close() error { return h.db.Close() }

func fingerprint(check, output string) string {
	// Whitespace changes do not create another cluster. Framework-neutral:
	// callers should not infer a test identity from this fingerprint.
	sum := sha256.Sum256([]byte(check + "\n" + strings.Join(strings.Fields(output), " ")))
	return hex.EncodeToString(sum[:])
}
func (h *History) Record(ctx context.Context, job core.CheckJob, output string, decision Decision, decisionErr error, retried, passed bool, reports ...[]core.TestCase) error {
	if h == nil {
		return nil
	}
	o := Observation{At: time.Now().UTC(), Run: job.RunID, Check: job.Name, Revision: job.MergeSHA, Fingerprint: fingerprint(job.Name, output), Decision: decision, Retried: retried, Passed: passed, Output: tail(output, 4096)}
	if decisionErr != nil {
		o.Error = decisionErr.Error()
	}
	if len(reports) > 0 {
		o.TestIDs = failedIDs(reports[0])
		o.Fingerprint = failureSignature(job.Name, output, reports[0])
	}
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = h.db.ExecContext(ctx, `INSERT INTO failure_review(at,fingerprint,check_name,data) VALUES(?,?,?,?)`, o.At.Unix(), o.Fingerprint, o.Check, string(data))
	if err != nil {
		return err
	}
	_, err = h.db.ExecContext(ctx, `DELETE FROM failure_review WHERE at < ? OR id NOT IN (SELECT id FROM failure_review ORDER BY id DESC LIMIT 10000); DELETE FROM retry_budget WHERE at < ?`, time.Now().Add(-30*24*time.Hour).Unix(), time.Now().Add(-30*24*time.Hour).Unix())
	return err
}
func (h *History) Reserve(ctx context.Context, run string, limit int) (bool, error) {
	if h == nil {
		return false, fmt.Errorf("failure history unavailable")
	}
	result, err := h.db.ExecContext(ctx, `INSERT INTO retry_budget(run,used,at) VALUES(?,1,?) ON CONFLICT(run) DO UPDATE SET used=used+1 WHERE used < ?`, run, time.Now().Unix(), limit)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ReserveEvidence atomically charges both the queue-run budget and the stable
// check-input budget. Reconstructing a run cannot replenish the latter.
func (h *History) ReserveEvidence(ctx context.Context, run, identity string, runLimit, inputLimit int) (bool, error) {
	if h == nil {
		return false, fmt.Errorf("failure history unavailable")
	}
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	for _, budget := range []struct {
		key   string
		limit int
	}{{run, runLimit}, {"inputs:" + identity, inputLimit}} {
		result, err := tx.ExecContext(ctx, `INSERT INTO retry_budget(run,used,at) VALUES(?,1,?) ON CONFLICT(run) DO UPDATE SET used=used+1 WHERE used < ?`, budget.key, time.Now().Unix(), budget.limit)
		if err != nil {
			return false, err
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (h *History) Recent(ctx context.Context, check string) ([]Observation, error) {
	if h == nil {
		return nil, fmt.Errorf("history disabled")
	}
	rows, err := h.db.QueryContext(ctx, `SELECT data FROM failure_review WHERE check_name=? ORDER BY id DESC LIMIT 20`, check)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Observation{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var o Observation
		if err := json.Unmarshal([]byte(data), &o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func failedIDs(tests []core.TestCase) []string {
	var ids []string
	for _, test := range tests {
		if test.Status == "failed" {
			ids = append(ids, test.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func failureSignature(check, output string, tests []core.TestCase) string {
	var failed []core.TestCase
	for _, test := range tests {
		if test.Status == "failed" {
			failed = append(failed, test)
		}
	}
	if len(failed) == 0 {
		return fingerprint(check, output)
	}
	slices.SortFunc(failed, func(a, b core.TestCase) int { return strings.Compare(a.ID, b.ID) })
	data, _ := json.Marshal(failed)
	return fingerprint(check, string(data))
}

// KnownFlake requires two distinct failed-then-passed executions, with no later
// failed retry for this exact signature. Model labels alone are not evidence.
func (h *History) KnownFlake(ctx context.Context, job core.CheckJob, res core.CheckResult) (Decision, bool, error) {
	if h == nil {
		return Decision{}, false, nil
	}
	signature := failureSignature(job.Name, res.Output, res.Tests)
	rows, err := h.db.QueryContext(ctx, `SELECT data FROM failure_review WHERE check_name=? AND fingerprint=? AND at>=? ORDER BY id DESC LIMIT 20`, job.Name, signature, time.Now().Add(-30*24*time.Hour).Unix())
	if err != nil {
		return Decision{}, false, err
	}
	defer rows.Close()
	runs := map[string]bool{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return Decision{}, false, err
		}
		var observation Observation
		if err := json.Unmarshal([]byte(data), &observation); err != nil {
			return Decision{}, false, err
		}
		if observation.Error != "" || !observation.Retried {
			continue
		}
		if !observation.Passed {
			return Decision{}, false, nil
		}
		if observation.Run != "" && observation.Run != job.RunID {
			runs[observation.Run] = true
			if len(runs) >= 2 {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return Decision{}, false, err
	}
	if len(runs) < 2 {
		return Decision{}, false, nil
	}
	return Decision{Action: "retry", Confidence: 1, Kind: "flake", Reason: "This exact failure signature recovered on retry in at least two distinct prior runs.", Evidence: []string{"observed failed-to-passed executions"}}, true, nil
}
