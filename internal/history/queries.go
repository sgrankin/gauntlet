package history

import (
	"fmt"
	"time"
)

// Write-side SQL. INSERT OR REPLACE keys on the tables' primary keys (run_id
// for runs; run_id+seq for checks; at+target for queue_depth), making
// re-emission of an identical row idempotent.
const (
	insertRunSQL = `
INSERT OR REPLACE INTO runs (
	run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
	base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms,
	batch_id, position, batch_size
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	insertCheckSQL = `
INSERT OR REPLACE INTO checks (run_id, seq, name, status, duration_ms, err, output, log_path)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	insertHookSQL = `
INSERT OR REPLACE INTO hooks (run_id, seq, name, status, duration_ms, err, output, log_path)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	insertDepthSQL = `
INSERT OR REPLACE INTO queue_depth (at, target, waiting, in_flight, parked)
VALUES (?, ?, ?, ?, ?)`
)

// RunRow is one row of the runs table, as read back for the dashboard.
type RunRow struct {
	RunID          string
	Target         string
	CandidateRef   string
	CandidateUser  string
	CandidateTopic string
	CandidateSHA   string
	BaseOID        string
	MergeSHA       string
	TrialClean     bool
	Outcome        string // landed|rejected|conflict|skipped|error
	Detail         string
	StartedAt      time.Time
	EndedAt        time.Time
	Duration       time.Duration

	// BatchID groups this run with its sibling per-member run rows when it
	// landed as part of a batch (empty for serial and speculate runs;
	// core.RunRecord.BatchID verbatim — docs/plans/phase5.md §10 amendment
	// 1). Position is this member's 0-based index within its batch (0 for
	// serial/speculate). BatchSize is the batch's member count, normalized
	// to at least 1 on write (batchSizeOrDefault) so a serial run's row
	// always reads BatchSize == 1 rather than 0.
	BatchID   string
	Position  int
	BatchSize int
}

// CheckRow is one row of the checks table, as read back for the dashboard.
type CheckRow struct {
	Seq      int
	Name     string
	Status   string // passed|failed|skipped
	Duration time.Duration
	Err      string
	// Output is the check's captured output, stored verbatim for every
	// status: green output is diagnostics too. The executor's 64KiB tail
	// cap is the only bound; history does not re-cap.
	Output string
	// LogPath is the full per-check log file's path (core.CheckResult.
	// LogPath), or "" when no file was written (no Config.LogDir
	// configured, or the file couldn't be created). The dashboard's
	// GET /run/{id}/log/{check} route serves this file, containment-checked
	// under its configured LogRoot; a stored path pointing at a since-pruned
	// or otherwise missing file serves a friendly 404, not an error.
	LogPath string
}

// HookRow is one row of the hooks table, as read back for the dashboard:
// one post-land hook's outcome within a landing (internal/hooks), mirroring
// CheckRow column-for-column.
type HookRow struct {
	Seq      int
	Name     string
	Status   string // passed|failed|skipped
	Duration time.Duration
	Err      string
	// Output is the hook's captured output, stored verbatim regardless of
	// status, same as CheckRow.Output.
	Output string
	// LogPath is the hook's full per-check log file path (core.CheckResult.
	// LogPath, as assigned by internal/hooks' hookLogPath), or "" when no
	// file was written (no Params.LogDir configured, or the file couldn't be
	// created). Served through the same GET /run/{id}/log/{name} route
	// checks use, containment-checked under the same LogRoot — see
	// CheckRow.LogPath's doc.
	LogPath string
}

// CheckStat summarizes one check's outcomes across a window of runs: how
// often it failed (red rate) and how long it took.
type CheckStat struct {
	Name        string
	Total       int
	Failed      int
	RedRate     float64 // Failed / Total; 0 when Total == 0
	AvgDuration time.Duration
	MaxDuration time.Duration
}

// DepthPoint is one sample of the queue_depth series.
type DepthPoint struct {
	At       time.Time
	Target   string
	Waiting  int
	InFlight int
	Parked   int
}

const selectRunColumns = `run_id, target, candidate_ref, candidate_user, candidate_topic, candidate_sha,
	base_oid, merge_sha, trial_clean, outcome, detail, started_at, ended_at, duration_ms,
	batch_id, position, batch_size`

// RecentRuns returns target's most recent runs, newest first, capped at
// limit.
func (s *Store) RecentRuns(target string, limit int) ([]RunRow, error) {
	rows, err := s.db.Query(
		`SELECT `+selectRunColumns+` FROM runs WHERE target = ? ORDER BY started_at DESC LIMIT ?`,
		target, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("history: recent runs: %w", err)
	}
	defer rows.Close()

	var out []RunRow
	for rows.Next() {
		r, err := scanRunRow(rows)
		if err != nil {
			return nil, fmt.Errorf("history: recent runs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: recent runs: %w", err)
	}
	return out, nil
}

// BatchMembers returns every run row sharing batchID, ordered by position —
// the dashboard's "landed in batch <id> (k of n)" link on /run/{id}
// (docs/plans/phase5.md §10 amendment 1). Empty batchID (never a real
// batch_id value; schema.sql's column default) returns an empty result
// rather than every serial/speculate run in the database, since those all
// share the same "" batch_id.
func (s *Store) BatchMembers(batchID string) ([]RunRow, error) {
	if batchID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT `+selectRunColumns+` FROM runs WHERE batch_id = ? ORDER BY position`,
		batchID,
	)
	if err != nil {
		return nil, fmt.Errorf("history: batch members %s: %w", batchID, err)
	}
	defer rows.Close()

	var out []RunRow
	for rows.Next() {
		r, err := scanRunRow(rows)
		if err != nil {
			return nil, fmt.Errorf("history: batch members %s: %w", batchID, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: batch members %s: %w", batchID, err)
	}
	return out, nil
}

// Run returns runID's run row and its check rows in seq order.
func (s *Store) Run(runID string) (RunRow, []CheckRow, error) {
	row := s.db.QueryRow(`SELECT `+selectRunColumns+` FROM runs WHERE run_id = ?`, runID)
	run, err := scanRunRow(row)
	if err != nil {
		return RunRow{}, nil, fmt.Errorf("history: run %s: %w", runID, err)
	}

	rows, err := s.db.Query(
		`SELECT seq, name, status, duration_ms, err, output, log_path FROM checks WHERE run_id = ? ORDER BY seq`,
		runID,
	)
	if err != nil {
		return RunRow{}, nil, fmt.Errorf("history: run %s checks: %w", runID, err)
	}
	defer rows.Close()

	var checks []CheckRow
	for rows.Next() {
		var c CheckRow
		var durationMS int64
		if err := rows.Scan(&c.Seq, &c.Name, &c.Status, &durationMS, &c.Err, &c.Output, &c.LogPath); err != nil {
			return RunRow{}, nil, fmt.Errorf("history: run %s checks: %w", runID, err)
		}
		c.Duration = time.Duration(durationMS) * time.Millisecond
		checks = append(checks, c)
	}
	if err := rows.Err(); err != nil {
		return RunRow{}, nil, fmt.Errorf("history: run %s checks: %w", runID, err)
	}
	return run, checks, nil
}

// Hooks returns runID's post-land hook rows (internal/hooks;
// core.EventHookFinished), in seq order — empty (nil, no error) when the run
// landed no hooks at all (no target hooks configured, or the landing never
// reached hooks, e.g. it wasn't a landing), which the dashboard and MCP run
// tool both treat as "omit the hooks section/field" rather than an error.
func (s *Store) Hooks(runID string) ([]HookRow, error) {
	rows, err := s.db.Query(
		`SELECT seq, name, status, duration_ms, err, output, log_path FROM hooks WHERE run_id = ? ORDER BY seq`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("history: hooks %s: %w", runID, err)
	}
	defer rows.Close()

	var out []HookRow
	for rows.Next() {
		var h HookRow
		var durationMS int64
		if err := rows.Scan(&h.Seq, &h.Name, &h.Status, &durationMS, &h.Err, &h.Output, &h.LogPath); err != nil {
			return nil, fmt.Errorf("history: hooks %s: %w", runID, err)
		}
		h.Duration = time.Duration(durationMS) * time.Millisecond
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: hooks %s: %w", runID, err)
	}
	return out, nil
}

// CheckStats summarizes per-check red-rate and duration across target's
// runs started at or after since.
//
// Batch-aware (docs/plans/phase5.md §10 amendment 1): a batch run's check
// results are duplicated verbatim onto every member's RunRecord (§3.3), so
// naively joining checks to runs would count a green batch of N suites as N
// (deflating red-rate) and a red batch's duplicated failures as N (inflating
// it). Instead this counts one suite per distinct suite key — batch_id when
// set, run_id otherwise (COALESCE(NULLIF(batch_id, empty string), run_id) in
// the query below): the "representative" CTE picks exactly one run_id per
// suite (the lexicographically-smallest run_id sharing a batch_id, or the
// run itself for serial/speculate, whose batch_id is empty), and checks are
// joined only to those representative run_ids.
// Since every member of a batch carries identical duplicated check rows,
// which member is picked as representative doesn't affect the resulting
// counts/durations — only how many times the suite is counted.
func (s *Store) CheckStats(target string, since time.Time) ([]CheckStat, error) {
	rows, err := s.db.Query(`
WITH suite_runs AS (
	SELECT run_id, COALESCE(NULLIF(batch_id, ''), run_id) AS suite_key
	FROM runs
	WHERE target = ? AND started_at >= ?
),
representative AS (
	SELECT MIN(run_id) AS run_id
	FROM suite_runs
	GROUP BY suite_key
)
SELECT c.name,
       COUNT(*) AS total,
       SUM(CASE WHEN c.status = 'failed' THEN 1 ELSE 0 END) AS failed,
       AVG(c.duration_ms) AS avg_ms,
       MAX(c.duration_ms) AS max_ms
FROM checks c
JOIN representative rep ON rep.run_id = c.run_id
GROUP BY c.name
ORDER BY c.name`,
		target, since.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("history: check stats: %w", err)
	}
	defer rows.Close()

	var out []CheckStat
	for rows.Next() {
		var st CheckStat
		var avgMS, maxMS float64
		if err := rows.Scan(&st.Name, &st.Total, &st.Failed, &avgMS, &maxMS); err != nil {
			return nil, fmt.Errorf("history: check stats: %w", err)
		}
		if st.Total > 0 {
			st.RedRate = float64(st.Failed) / float64(st.Total)
		}
		st.AvgDuration = time.Duration(avgMS * float64(time.Millisecond))
		st.MaxDuration = time.Duration(maxMS * float64(time.Millisecond))
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: check stats: %w", err)
	}
	return out, nil
}

// DepthSeries returns target's queue-depth samples at or after since, in
// ascending time order.
func (s *Store) DepthSeries(target string, since time.Time) ([]DepthPoint, error) {
	rows, err := s.db.Query(
		`SELECT at, target, waiting, in_flight, parked FROM queue_depth WHERE target = ? AND at >= ? ORDER BY at ASC`,
		target, since.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("history: depth series: %w", err)
	}
	defer rows.Close()

	var out []DepthPoint
	for rows.Next() {
		var p DepthPoint
		var atMS int64
		if err := rows.Scan(&atMS, &p.Target, &p.Waiting, &p.InFlight, &p.Parked); err != nil {
			return nil, fmt.Errorf("history: depth series: %w", err)
		}
		p.At = time.UnixMilli(atMS)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: depth series: %w", err)
	}
	return out, nil
}

// RefVerdict is one candidate ref's most recent terminal run outcome for a
// target (LatestTerminalPerRef) — the read side of Feature 2's boot-time
// park seeding (queue.Config.SeedParks): Outcome is the raw stored string
// (outcomeString's vocabulary: landed|rejected|conflict|skipped|error), left
// unparsed here since history never imports internal/core (cmd's SeedParks
// closure, the sole caller, maps it back to core.Outcome).
type RefVerdict struct {
	Ref     string
	SHA     string
	Outcome string
	Detail  string
	EndedAt time.Time
}

// LatestTerminalPerRef returns, for every distinct candidate_ref recorded
// against target, that ref's single most recent run row: "most recent" by
// started_at, tie-broken by run_id (both monotonic enough for this purpose —
// newRunID's timestamp+counter scheme, internal/queue/reconcile.go). One row
// per ref, unordered.
//
// Interleaved histories resolve exactly as the window function's
// PARTITION BY/ORDER BY says they should: a ref rejected then later landed
// has its landed row win (no park candidate); a ref landed then later
// rejected again has the rejection win (a park candidate); a ref rejected
// multiple times in a row has only the LATEST rejection win, never an
// earlier one. The caller (queue.Config.SeedParks, wired in cmd/gauntlet)
// is responsible for filtering to red-family outcomes before treating a
// result as a park seed — this method itself returns every ref's latest
// verdict regardless of outcome, landed included.
func (s *Store) LatestTerminalPerRef(target string) ([]RefVerdict, error) {
	rows, err := s.db.Query(`
SELECT candidate_ref, candidate_sha, outcome, detail, ended_at
FROM (
	SELECT candidate_ref, candidate_sha, outcome, detail, ended_at,
	       ROW_NUMBER() OVER (
	           PARTITION BY candidate_ref
	           ORDER BY started_at DESC, run_id DESC
	       ) AS rn
	FROM runs
	WHERE target = ?
)
WHERE rn = 1`, target)
	if err != nil {
		return nil, fmt.Errorf("history: latest terminal per ref %s: %w", target, err)
	}
	defer rows.Close()

	var out []RefVerdict
	for rows.Next() {
		var v RefVerdict
		var endedMS int64
		if err := rows.Scan(&v.Ref, &v.SHA, &v.Outcome, &v.Detail, &endedMS); err != nil {
			return nil, fmt.Errorf("history: latest terminal per ref %s: %w", target, err)
		}
		v.EndedAt = time.UnixMilli(endedMS)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: latest terminal per ref %s: %w", target, err)
	}
	return out, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting
// scanRunRow serve both RecentRuns (multi-row) and Run (single-row).
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRunRow(row rowScanner) (RunRow, error) {
	var r RunRow
	var trialClean int
	var startedMS, endedMS, durationMS int64
	err := row.Scan(
		&r.RunID, &r.Target, &r.CandidateRef, &r.CandidateUser, &r.CandidateTopic, &r.CandidateSHA,
		&r.BaseOID, &r.MergeSHA, &trialClean, &r.Outcome, &r.Detail,
		&startedMS, &endedMS, &durationMS,
		&r.BatchID, &r.Position, &r.BatchSize,
	)
	if err != nil {
		return RunRow{}, err
	}
	r.TrialClean = trialClean != 0
	r.StartedAt = time.UnixMilli(startedMS)
	r.EndedAt = time.UnixMilli(endedMS)
	r.Duration = time.Duration(durationMS) * time.Millisecond
	return r, nil
}
