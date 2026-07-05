-- schema.sql: gauntlet history store schema (user_version = 5).
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
  batch_size   INTEGER NOT NULL DEFAULT 1
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
