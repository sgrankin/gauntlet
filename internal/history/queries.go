package history

import (
	"fmt"
	"sort"
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
	batch_id, position, batch_size, speculated, recovered,
	receipt_ref, receipt_blob, receipt_published
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	insertCheckSQL = `
INSERT OR REPLACE INTO checks (run_id, seq, name, status, duration_ms, err, output, log_path, command, blocked_by, waited_ms, image, materialize_ms, peak_rss_bytes, user_cpu_ms, sys_cpu_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	insertHookSQL = `
INSERT OR REPLACE INTO hooks (run_id, seq, name, status, duration_ms, err, output, log_path)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`

	insertDepthSQL = `
INSERT OR REPLACE INTO queue_depth (at, target, waiting, in_flight, parked)
VALUES (?, ?, ?, ?, ?)`

	// upsertRetryIntentSQL, insertIgnoredRefSQL, upsertHookRunStartedSQL, and
	// upsertHookRunSkippedSQL back the v6 durability rows — see
	// Store.writeRetryIntent/writeIgnoredRef/writeHookStarted/
	// writeHookSkipped for the upsert-vs-append reasoning behind each.
	upsertRetryIntentSQL = `
INSERT INTO retry_intents (target, ref, sha, at) VALUES (?, ?, ?, ?)
ON CONFLICT(target, ref) DO UPDATE SET sha = excluded.sha, at = excluded.at`

	insertIgnoredRefSQL = `
INSERT OR REPLACE INTO ignored_refs (at, target, ref, detail) VALUES (?, ?, ?, ?)`

	upsertHookRunStartedSQL = `
INSERT INTO hook_runs (run_id, target, owed_count, started_at, skipped, skip_reason)
VALUES (?, ?, ?, ?, 0, '')
ON CONFLICT(run_id) DO NOTHING`

	upsertHookRunSkippedSQL = `
INSERT INTO hook_runs (run_id, target, owed_count, started_at, skipped, skip_reason)
VALUES (?, ?, ?, ?, 1, ?)
ON CONFLICT(run_id) DO UPDATE SET skipped = 1, skip_reason = excluded.skip_reason`
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
	// core.RunRecord.BatchID verbatim). Position is this member's 0-based
	// index within its batch (0 for
	// serial/speculate). BatchSize is the batch's member count, normalized
	// to at least 1 on write (batchSizeOrDefault) so a serial run's row
	// always reads BatchSize == 1 rather than 0.
	BatchID   string
	Position  int
	BatchSize int

	// Speculated and Recovered are core.RunRecord.Speculated/Recovered
	// verbatim (v7+): Speculated marks a run tested on a predicted
	// (non-head) base rather than the live target tip; Recovered marks a
	// record synthesized by crash recovery (no actual trial+check run
	// happened). Both are purely informational — see RunRecord's own field
	// docs — and are surfaced only on the run-detail dashboard/API/MCP
	// view, not the runs listing.
	Speculated bool
	Recovered  bool

	// ReceiptRef, ReceiptBlob, and ReceiptPublished are core.RunRecord's
	// own fields of the same names verbatim (v12+): the receipt-notes
	// publication provenance of a run whose note was confirmed published
	// (issue #13) — the notes ref, the published note's blob SHA, and
	// "published"/"already-present". All three are "" when receipt-notes
	// policy is disabled, the spec declared no receipt, or the row
	// predates v12 — NOT necessarily "" for a non-landed run: a run that
	// publishes and then loses the target race (stale CAS, crash) still
	// carries these three, by design (see schema.sql's column comment
	// and reconcile.go's stampReceiptRecords).
	ReceiptRef       string
	ReceiptBlob      string
	ReceiptPublished string

	// ChecksTotal and ChecksPassed are this run's own check pass/total
	// counts, populated only by RecentRuns (via correlated scalar
	// subqueries — see its doc comment) — dashboard/server.go turns
	// them into the compact "✓ n/m" ratio next to the outcome chip on
	// target.html's Recent runs table. Run/BatchMembers/LatestTerminalPerRef
	// never select these columns, so both fields stay at their Go zero
	// value (0) on a RunRow returned from any of those; ChecksTotal == 0
	// there is not a claim "this run had no checks", just "not queried".
	ChecksTotal  int
	ChecksPassed int
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
	// Command is the check's argv, shell-joined into one display string at
	// write time (store.go's joinCommand, core.CheckResult.Command) — run.html
	// renders it as an accent-colored command echo above the check's output.
	// Empty for rows written before v8; run.html renders no echo line then.
	Command string
	// BlockedBy is the comma-joined prerequisite names that blocked a
	// 'blocked'-status check (core.CheckResult.BlockedBy); "" for every
	// other status and for rows written before v9.
	BlockedBy string
	// Waited is how long the check sat ready but slotless under the
	// daemon-wide max-executions cap before starting
	// (core.CheckResult.Waited); zero for immediate starts and pre-v9 rows.
	Waited time.Duration
	// Image is the immutable candidate-built image identity this row is
	// about (core.CheckResult.Image): a build node's captured result, or
	// the identity a consumer check ran in. "" otherwise and pre-v10.
	Image string
	// Materialized is how long this node's private isolated workspace took
	// to materialize before its command ran (core.CheckResult.Materialized,
	// issue #9); zero in shared mode and for pre-v11 rows.
	Materialized time.Duration
	// PeakRSS is the peak resident-set size the check's process (and reaped
	// descendants) touched, in bytes (core.CheckResult.PeakRSS, v13+, issue
	// #14). Zero means "not measured" — the container executor (v1) never
	// measures it, and neither does a pre-v13 row — never "measured zero".
	PeakRSS int64
	// UserCPU and SysCPU are the check's process (and reaped-descendant)
	// user-space/kernel CPU time (core.CheckResult.UserCPU/SysCPU, v13+,
	// issue #14). Same zero-means-unmeasured contract as PeakRSS.
	UserCPU time.Duration
	SysCPU  time.Duration
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
// often it failed (red rate), how long it took, and (issue #14) its
// resource-usage envelope.
type CheckStat struct {
	Name        string
	Total       int
	Failed      int
	RedRate     float64 // Failed / Total; 0 when Total == 0
	AvgDuration time.Duration
	MaxDuration time.Duration

	// PeakRSSMax/PeakRSSMedian summarize checks.peak_rss_bytes across this
	// window's rows — but ONLY rows where it was actually measured (> 0;
	// core.CheckResult's zero-means-unmeasured contract). PeakRSSMeasured is
	// false, and both values are zero, when not a single row in the window
	// measured it (e.g. every run used the container executor, which never
	// does, or the window predates v13) — callers must gate on
	// PeakRSSMeasured rather than treat zero as "measured zero", exactly
	// like AvgDuration would if duration itself could go unmeasured (it
	// can't, which is why no such gate exists on it).
	PeakRSSMax      int64
	PeakRSSMedian   int64
	PeakRSSMeasured bool

	// UserCPUMax/UserCPUMedian/SysCPUMax/SysCPUMedian mirror PeakRSSMax/
	// PeakRSSMedian for checks.user_cpu_ms/sys_cpu_ms, independently gated
	// by their own Measured flags: a check's PeakRSS and CPU times are
	// captured together in practice (same executor rusage read), but a
	// measurement failure or a mixed-version window could in principle
	// measure one without the other, so each metric carries its own
	// presence flag rather than sharing PeakRSSMeasured.
	UserCPUMax      time.Duration
	UserCPUMedian   time.Duration
	UserCPUMeasured bool
	SysCPUMax       time.Duration
	SysCPUMedian    time.Duration
	SysCPUMeasured  bool
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
	batch_id, position, batch_size, speculated, recovered,
	receipt_ref, receipt_blob, receipt_published`

// RecentRuns returns target's most recent runs, newest first, capped at
// limit. Each row also carries its own ChecksTotal/ChecksPassed — see
// RunRow's doc for what those feed (target.html's compact "✓ n/m" ratio).
//
// The two counts are correlated scalar subqueries in the SELECT list, not a
// LEFT JOIN against a GROUP-BY-all-of-checks derived table: this query runs
// on every dashboard refresh tick (5s), and runs.target/started_at is
// already indexed (idx_runs_target_started), so WHERE+ORDER BY+LIMIT narrow
// to the ~20 returned rows before either subquery ever runs — each one is
// then a single indexed lookup on checks' own primary key (run_id, seq),
// scoped to just that run_id, rather than an aggregate over the ENTIRE
// checks table computed up front regardless of the LIMIT. COUNT(*) over zero
// matching rows is 0, not NULL, so neither subquery needs the COALESCE a
// LEFT JOIN's aggregate columns would.
//
// Batch-aware only incidentally: unlike CheckStats' target-wide aggregate
// (which must dedupe a batch's duplicated per-member check rows to avoid
// counting one suite N times), this correlates on a single run_id at a time,
// and writeRecord duplicates a batch's checks verbatim onto every member's
// own run_id — so each member's own count here is already exactly that
// member's own (duplicated but internally consistent) check set, with no
// cross-member counting to get wrong.
func (s *Store) RecentRuns(target string, limit int) ([]RunRow, error) {
	rows, err := s.db.Query(`
SELECT `+selectRunColumns+`,
       (SELECT COUNT(*) FROM checks c WHERE c.run_id = runs.run_id),
       (SELECT COUNT(*) FROM checks c WHERE c.run_id = runs.run_id AND c.status = 'passed')
FROM runs
WHERE target = ?
ORDER BY started_at DESC LIMIT ?`,
		target, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("history: recent runs: %w", err)
	}
	defer rows.Close()

	var out []RunRow
	for rows.Next() {
		var total, passed int
		r, err := scanRunRow(rows, &total, &passed)
		if err != nil {
			return nil, fmt.Errorf("history: recent runs: %w", err)
		}
		r.ChecksTotal, r.ChecksPassed = total, passed
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: recent runs: %w", err)
	}
	return out, nil
}

// BatchMembers returns every run row sharing batchID, ordered by position —
// the dashboard's "landed in batch <id> (k of n)" link on /run/{id}. Empty
// batchID (never a real
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
		`SELECT seq, name, status, duration_ms, err, output, log_path, command, blocked_by, waited_ms, image, materialize_ms, peak_rss_bytes, user_cpu_ms, sys_cpu_ms FROM checks WHERE run_id = ? ORDER BY seq`,
		runID,
	)
	if err != nil {
		return RunRow{}, nil, fmt.Errorf("history: run %s checks: %w", runID, err)
	}
	defer rows.Close()

	var checks []CheckRow
	for rows.Next() {
		var c CheckRow
		var durationMS, waitedMS, materializeMS, userCPUms, sysCPUms int64
		if err := rows.Scan(&c.Seq, &c.Name, &c.Status, &durationMS, &c.Err, &c.Output, &c.LogPath, &c.Command, &c.BlockedBy, &waitedMS, &c.Image, &materializeMS, &c.PeakRSS, &userCPUms, &sysCPUms); err != nil {
			return RunRow{}, nil, fmt.Errorf("history: run %s checks: %w", runID, err)
		}
		c.Duration = time.Duration(durationMS) * time.Millisecond
		c.Waited = time.Duration(waitedMS) * time.Millisecond
		c.Materialized = time.Duration(materializeMS) * time.Millisecond
		c.UserCPU = time.Duration(userCPUms) * time.Millisecond
		c.SysCPU = time.Duration(sysCPUms) * time.Millisecond
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
// Total/Failed/AvgDuration/MaxDuration are computed in SQL (GROUP BY
// c.name), unchanged since before v13. The resource-usage aggregates
// (PeakRSS*/UserCPU*/SysCPU*, issue #14) need a per-check-name MEDIAN, which
// SQLite has no builtin aggregate for; rather than hand-roll a
// ROW_NUMBER/PARTITION window query three times over (once per metric) this
// runs one extra query fetching every representative row's raw values (name
// + the three usage columns) and reduces each check name's slice to
// max/median in Go — see medianAndMax, below.
//
// Batch-aware: a batch run's check results are duplicated verbatim onto
// every member's RunRecord, so naively joining checks to runs would count a
// green batch of N suites as N
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
-- blocked rows (v9+) record that a check NEVER RAN — a prerequisite or the
-- run's root failure stopped it. Counting them would dilute red-rate's
-- denominator and drag avg/max duration toward their zero duration_ms, so
-- they're excluded: these stats describe executions, and a blocked check
-- didn't have one.
WHERE c.status != 'blocked'
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
	if len(out) == 0 {
		return out, nil
	}

	// indexByName is built only now that out is done growing: CheckStats
	// below mutates out[idx] in place via this map, and a pointer/index
	// taken mid-append would go stale the moment a later append reallocates
	// out's backing array — this two-pass shape (finish growing, then index)
	// sidesteps that rather than pre-sizing out to a row count learned
	// separately.
	indexByName := make(map[string]int, len(out))
	for i, st := range out {
		indexByName[st.Name] = i
	}

	usageRows, err := s.db.Query(`
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
SELECT c.name, c.peak_rss_bytes, c.user_cpu_ms, c.sys_cpu_ms
FROM checks c
JOIN representative rep ON rep.run_id = c.run_id
WHERE c.status != 'blocked'`,
		target, since.UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("history: check stats usage: %w", err)
	}
	defer usageRows.Close()

	peakRSS := make(map[string][]int64)
	userCPU := make(map[string][]int64)
	sysCPU := make(map[string][]int64)
	for usageRows.Next() {
		var name string
		var peak, user, sys int64
		if err := usageRows.Scan(&name, &peak, &user, &sys); err != nil {
			return nil, fmt.Errorf("history: check stats usage: %w", err)
		}
		// > 0 only: the capture-slice zero-means-unmeasured contract
		// (core.CheckResult's own field docs) applies per field
		// independently, so a row can contribute to one metric's slice and
		// not another's.
		if peak > 0 {
			peakRSS[name] = append(peakRSS[name], peak)
		}
		if user > 0 {
			userCPU[name] = append(userCPU[name], user)
		}
		if sys > 0 {
			sysCPU[name] = append(sysCPU[name], sys)
		}
	}
	if err := usageRows.Err(); err != nil {
		return nil, fmt.Errorf("history: check stats usage: %w", err)
	}

	for name, idx := range indexByName {
		st := &out[idx]
		if median, max, ok := medianAndMax(peakRSS[name]); ok {
			st.PeakRSSMedian, st.PeakRSSMax, st.PeakRSSMeasured = median, max, true
		}
		if median, max, ok := medianAndMax(userCPU[name]); ok {
			st.UserCPUMedian = time.Duration(median) * time.Millisecond
			st.UserCPUMax = time.Duration(max) * time.Millisecond
			st.UserCPUMeasured = true
		}
		if median, max, ok := medianAndMax(sysCPU[name]); ok {
			st.SysCPUMedian = time.Duration(median) * time.Millisecond
			st.SysCPUMax = time.Duration(max) * time.Millisecond
			st.SysCPUMeasured = true
		}
	}
	return out, nil
}

// medianAndMax returns vals' median and max. ok is false (median and max
// both 0) for an empty vals — the "nothing measured" case CheckStats uses to
// leave a CheckStat's *Measured flag false rather than report a misleading
// zero. vals is sorted in place; callers here always pass a freshly built
// per-name slice, never a slice another caller still holds a reference to.
func medianAndMax(vals []int64) (median, max int64, ok bool) {
	if len(vals) == 0 {
		return 0, 0, false
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	max = vals[len(vals)-1]
	mid := len(vals) / 2
	if len(vals)%2 == 1 {
		median = vals[mid]
	} else {
		median = (vals[mid-1] + vals[mid]) / 2
	}
	return median, max, true
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
// target (LatestTerminalPerRef) — the read side of boot-time park seeding
// (queue.Config.SeedParks): Outcome is the raw stored string
// (outcomeString's vocabulary: landed|rejected|conflict|skipped|error), left
// unparsed here since history never imports internal/core (cmd's SeedParks
// closure, the sole caller, maps it back to core.Outcome).
type RefVerdict struct {
	Ref     string
	SHA     string
	Outcome string
	Detail  string
	EndedAt time.Time

	// RunID is the winning row's own run_id — plumbed through so
	// cmd/gauntlet's SeedParks closure can carry it into queue.ParkSeed,
	// letting a boot-seeded park still link to the run that produced its
	// verdict (queue.ParkedEntry.RunID) rather than rendering unlinked
	// until the ref's next real terminal event.
	RunID string
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
//
// Retry-intent read-side suppression: a ref's latest terminal row is
// additionally suppressed — omitted from the result entirely — when a
// retry_intents row for the same (target, ref) is newer than that row's
// ended_at (LEFT JOIN retry_intents ... WHERE ri.at IS NULL OR ri.at <=
// ended_at). Net effect: an operator's retry (core.EventRetryRequested,
// internal/queue/command.go's applyRetry) that hasn't yet been superseded by
// a fresh terminal outcome means "don't re-seed a park from the stale
// pre-retry verdict" — a daemon crash between the retry and the retried
// run's own terminal event no longer silently re-parks the ref at its old
// rejection on restart. If the retried run later produces its OWN newer
// terminal row (e.g. it rejects again), that row's ended_at is newer than
// the retry, the join condition is satisfied again, and the ref re-parks
// correctly with the new reason.
//
// Accepted millisecond-tie: both ri.at and ended_at are
// millisecond-truncated (.UnixMilli()), and the join uses <=, so a retry
// landing in the SAME millisecond as the terminal it's retrying
// away from (an automated immediate-retry, or a coarse/fixed test clock)
// still satisfies ri.at <= ended_at — the park is kept, and the operator's
// retry is silently discarded on restart, exactly the narrow-window bug
// this method otherwise closes. This can't be fixed by flipping to a strict
// <: the retried run's own newer terminal, landing at ended_at == ri.at
// (its natural case when the retry and its own outcome are timestamped
// identically at millisecond granularity), must still satisfy the
// comparison to re-park with the fresh reason — flipping the operator would
// just break re-park-on-new-terminal instead. The tie is genuinely
// unresolvable by a timestamp compare alone; disambiguating it would need a
// monotonic sequence number or run-identity ordering, not a threshold
// change. Accepted as low severity: a human operator's retry normally trails
// the rejection it's responding to by seconds, not sub-millisecond, so
// ri.at >> ended_at in practice — the seedparks/retryintent tests only pass
// because their clock advances between reject and retry; the equal-`at`
// boundary itself is untested.
func (s *Store) LatestTerminalPerRef(target string) ([]RefVerdict, error) {
	rows, err := s.db.Query(`
SELECT t.candidate_ref, t.candidate_sha, t.outcome, t.detail, t.ended_at, t.run_id
FROM (
	SELECT candidate_ref, candidate_sha, outcome, detail, ended_at, run_id,
	       ROW_NUMBER() OVER (
	           PARTITION BY candidate_ref
	           ORDER BY started_at DESC, run_id DESC
	       ) AS rn
	FROM runs
	WHERE target = ?
) t
LEFT JOIN retry_intents ri ON ri.target = ? AND ri.ref = t.candidate_ref
WHERE t.rn = 1 AND (ri.at IS NULL OR ri.at <= t.ended_at)`, target, target)
	if err != nil {
		return nil, fmt.Errorf("history: latest terminal per ref %s: %w", target, err)
	}
	defer rows.Close()

	var out []RefVerdict
	for rows.Next() {
		var v RefVerdict
		var endedMS int64
		if err := rows.Scan(&v.Ref, &v.SHA, &v.Outcome, &v.Detail, &endedMS, &v.RunID); err != nil {
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

// IgnoredRef is one recorded occurrence of core.EventIgnoredRef: a
// well-formed candidate ref whose target segment named no configured
// target, as read back for the dashboard/MCP "recently ignored refs"
// section (internal/queue/reconcile.go's checkIgnoredRefs is the sole
// writer, via Store.Emit).
type IgnoredRef struct {
	At     time.Time
	Target string
	Ref    string
	Detail string
}

// IgnoredRefs returns the most recently ignored refs across the whole
// daemon, newest first, capped at limit — the read side of the durable
// misconfig capture: an operator not watching the log/Slack at the instant
// a misnamed/misconfigured ref was pushed can still discover it here after
// the fact.
//
// Deliberately NOT filtered by target: an ignored ref's defining property
// is that its target segment names NO configured target — checkIgnoredRefs
// emits it under that
// unconfigured name — so a per-configured-target query would never match
// anything. The IgnoredRef.Target field carries the unconfigured name for
// display ("for/nope/… — target \"nope\" is not configured").
func (s *Store) IgnoredRefs(limit int) ([]IgnoredRef, error) {
	rows, err := s.db.Query(
		`SELECT at, target, ref, detail FROM ignored_refs ORDER BY at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("history: ignored refs: %w", err)
	}
	defer rows.Close()

	var out []IgnoredRef
	for rows.Next() {
		var r IgnoredRef
		var atMS int64
		if err := rows.Scan(&atMS, &r.Target, &r.Ref, &r.Detail); err != nil {
			return nil, fmt.Errorf("history: ignored refs: %w", err)
		}
		r.At = time.UnixMilli(atMS)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: ignored refs: %w", err)
	}
	return out, nil
}

// HookRunSummary is one landing's durable post-land hook "owed" state
// (core.EventHookStarted/EventHookSkipped -> hook_runs), as read back
// for the dashboard/MCP: OwedCount is the target's configured hook count at
// the moment the first hook of this landing started (or, for a skipped
// landing, at skip time); DoneCount is derived from COUNT(*) of this run's
// rows in the `hooks` table (never stored redundantly in hook_runs itself).
//
// A caller (e.g. the dashboard) reads OwedCount > DoneCount && !Skipped,
// combined with enough wall-clock time having passed since StartedAt (a
// "stale" threshold the caller chooses — this method carries no opinion on
// it, since it has no notion of "now"), as "this landing's hook chain
// crashed mid-way and will never finish on its own" — the crash-incomplete
// signal this exists to surface, discoverable without gauntlet ever
// auto-resuming a hook.
type HookRunSummary struct {
	RunID      string
	Target     string
	OwedCount  int
	DoneCount  int
	StartedAt  time.Time
	Skipped    bool
	SkipReason string
}

// HookRunSummaries returns target's most recent hook_runs entries, newest
// (by StartedAt) first, capped at limit, each joined against the `hooks`
// table to derive DoneCount. A landing with no hook_runs row at all (i.e.
// hooks.Runner never emitted EventHookStarted/EventHookSkipped for it —
// either the target has no hooks configured, or it predates the v6 schema)
// simply has no entry here; this is not an error.
func (s *Store) HookRunSummaries(target string, limit int) ([]HookRunSummary, error) {
	rows, err := s.db.Query(`
SELECT hr.run_id, hr.target, hr.owed_count, hr.started_at, hr.skipped, hr.skip_reason,
       COUNT(h.run_id) AS done_count
FROM hook_runs hr
LEFT JOIN hooks h ON h.run_id = hr.run_id
WHERE hr.target = ?
GROUP BY hr.run_id
ORDER BY hr.started_at DESC
LIMIT ?`, target, limit)
	if err != nil {
		return nil, fmt.Errorf("history: hook run summaries %s: %w", target, err)
	}
	defer rows.Close()

	var out []HookRunSummary
	for rows.Next() {
		var h HookRunSummary
		var startedMS int64
		var skipped int
		if err := rows.Scan(&h.RunID, &h.Target, &h.OwedCount, &startedMS, &skipped, &h.SkipReason, &h.DoneCount); err != nil {
			return nil, fmt.Errorf("history: hook run summaries %s: %w", target, err)
		}
		h.StartedAt = time.UnixMilli(startedMS)
		h.Skipped = skipped != 0
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: hook run summaries %s: %w", target, err)
	}
	return out, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, letting
// scanRunRow serve both RecentRuns (multi-row) and Run (single-row).
type rowScanner interface {
	Scan(dest ...any) error
}

// scanRunRow scans one selectRunColumns-shaped row into a RunRow. extra, when
// given, is appended to the Scan destination list after the fixed 22 —
// RecentRuns' query below additionally selects a check-count aggregate that
// no other RunRow-returning method needs, so its caller passes pointers for
// those trailing columns rather than this function knowing about them itself.
func scanRunRow(row rowScanner, extra ...any) (RunRow, error) {
	var r RunRow
	var trialClean int
	var speculated, recovered int
	var startedMS, endedMS, durationMS int64
	dest := []any{
		&r.RunID, &r.Target, &r.CandidateRef, &r.CandidateUser, &r.CandidateTopic, &r.CandidateSHA,
		&r.BaseOID, &r.MergeSHA, &trialClean, &r.Outcome, &r.Detail,
		&startedMS, &endedMS, &durationMS,
		&r.BatchID, &r.Position, &r.BatchSize, &speculated, &recovered,
		&r.ReceiptRef, &r.ReceiptBlob, &r.ReceiptPublished,
	}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return RunRow{}, err
	}
	r.TrialClean = trialClean != 0
	r.StartedAt = time.UnixMilli(startedMS)
	r.EndedAt = time.UnixMilli(endedMS)
	r.Duration = time.Duration(durationMS) * time.Millisecond
	r.Speculated = speculated != 0
	r.Recovered = recovered != 0
	return r, nil
}
