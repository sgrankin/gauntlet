-- schema.sql: gauntlet history store schema (user_version = 7).
--
-- Applied fresh (user_version == 0) via the migrate() stepwise switch in
-- store.go, which stamps a new database straight to the current version. An
-- existing older database is migrated in place instead (ALTER TABLE checks
-- ADD COLUMN ..., CREATE TABLE hooks ...) rather than re-run against this
-- file, so this file always reflects the *current* schema, not the upgrade
-- path — see migrate()'s doc comment for the version-by-version steps.

CREATE TABLE runs (
  run_id       TEXT PRIMARY KEY,
  target       TEXT NOT NULL,
  candidate_ref   TEXT NOT NULL,
  candidate_user  TEXT NOT NULL,
  candidate_topic TEXT NOT NULL,
  candidate_sha   TEXT NOT NULL,
  base_oid     TEXT NOT NULL,
  merge_sha    TEXT NOT NULL,
  trial_clean  INTEGER NOT NULL,
  outcome      TEXT NOT NULL,             -- landed|rejected|conflict|skipped|error
  detail       TEXT NOT NULL,
  started_at   INTEGER NOT NULL,          -- unix millis
  ended_at     INTEGER NOT NULL,
  duration_ms  INTEGER NOT NULL,
  -- batch_id/position/batch_size (v5+, docs/plans/phase5.md §10 amendment 1):
  -- batch_id groups the per-member records of one batch run (empty for
  -- serial and speculate; core.RunRecord.BatchID verbatim). position is this
  -- member's 0-based index within its batch (0 for serial/speculate).
  -- batch_size is the batch's member count (1 otherwise). Together these
  -- let CheckStats count one suite per distinct
  -- COALESCE(NULLIF(batch_id,''), run_id) instead of once per duplicated
  -- member record.
  batch_id     TEXT NOT NULL DEFAULT '',
  position     INTEGER NOT NULL DEFAULT 0,
  batch_size   INTEGER NOT NULL DEFAULT 1,
  -- speculated/recovered (v7+): core.RunRecord.Speculated/Recovered
  -- verbatim. Speculated marks a run tested on a predicted (non-head) base
  -- rather than the live target tip; recovered marks a record synthesized
  -- by crash recovery (no actual trial+check run happened). Both are
  -- purely informational (see RunRecord's own field docs) — surfaced only
  -- on the run-detail dashboard/API/MCP view, never read by queue logic.
  speculated   INTEGER NOT NULL DEFAULT 0,
  recovered    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);
CREATE INDEX idx_runs_batch_id ON runs(batch_id) WHERE batch_id != '';

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,              -- passed|failed|skipped
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',   -- captured output, verbatim (executor tail-caps at 64KiB) (v2+)
  log_path    TEXT NOT NULL DEFAULT '',   -- full per-check log file path, if one was written (v3+)
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

-- hooks mirrors checks column-for-column (log/history parity for post-land
-- hooks, internal/hooks): one row per finished hook (core.EventHookFinished),
-- keyed by (run_id, seq) exactly like checks. Unlike checks, rows arrive one
-- at a time (there is no RunRecord.Hooks slice to write in bulk from) — see
-- Store.Emit's EventHookFinished branch for how seq is derived without one
-- being carried on the event (v4+).
CREATE TABLE hooks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,              -- passed|failed|skipped
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',   -- captured output, verbatim (executor tail-caps at 64KiB)
  log_path    TEXT NOT NULL DEFAULT '',   -- full per-hook log file path, if one was written
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_hooks_name ON hooks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);

-- retry_intents durably records an operator's most recent retry of a parked
-- (target, ref) (core.EventRetryRequested, S3): upserted on every retry, one
-- row per (target, ref) — the latest retry always wins. Read by
-- LatestTerminalPerRef's seed-park query to suppress re-parking a ref whose
-- last recorded terminal outcome predates a later retry: without this, a
-- daemon crash between an operator's retry and the retried run's own
-- terminal event would silently re-park the ref at its old, now-superseded
-- rejection on restart (v6+).
CREATE TABLE retry_intents (
  target TEXT NOT NULL,
  ref    TEXT NOT NULL,
  sha    TEXT NOT NULL,
  at     INTEGER NOT NULL,
  PRIMARY KEY (target, ref)
);

-- ignored_refs durably records core.EventIgnoredRef (a well-formed candidate
-- ref whose target segment names no configured target — a misconfiguration,
-- S7c): one row per occurrence, so an operator not watching the log/Slack at
-- the instant it happened can still discover it after the fact. Keyed by
-- (at, target, ref) rather than just (target, ref) since the daemon reports
-- the same (ref, SHA) at most once per SHA (see checkIgnoredRefs,
-- internal/queue/reconcile.go) but a ref pushed multiple times over its
-- lifetime legitimately produces multiple distinct rows (v6+).
CREATE TABLE ignored_refs (
  at     INTEGER NOT NULL,
  target TEXT NOT NULL,
  ref    TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (at, target, ref)
);

-- hook_runs durably records one landing's post-land hook "owed" state
-- (core.EventHookStarted/EventHookSkipped; S1-C's durable owed/skipped
-- marker, no auto-resume): one row per run_id, written the instant the
-- FIRST EventHookStarted for that landing reaches history — synchronously,
-- before any hook subprocess even starts — or, for a recovery-synthesized
-- landing whose hooks were never run at all, by EventHookSkipped instead.
-- owed_count is the target's configured hook count at that moment
-- (EventHookStarted/Skipped's HookCount); the number of hooks actually
-- finished is derived separately by counting this run_id's rows in the
-- existing `hooks` table (never stored redundantly here) — see
-- Store.HookRunSummaries. owed_count > (that count), when skipped = 0, means
-- the daemon crashed mid-chain: later hooks in this landing's order were
-- never reached. skipped = 1 (skip_reason set) means hooks were
-- deliberately never attempted (a recovery landing with no merge SHA to
-- export), which must never be confused with a crash (v6+).
CREATE TABLE hook_runs (
  run_id      TEXT PRIMARY KEY REFERENCES runs(run_id) ON DELETE CASCADE,
  target      TEXT NOT NULL,
  owed_count  INTEGER NOT NULL,
  started_at  INTEGER NOT NULL,
  skipped     INTEGER NOT NULL DEFAULT 0,
  skip_reason TEXT NOT NULL DEFAULT ''
);
