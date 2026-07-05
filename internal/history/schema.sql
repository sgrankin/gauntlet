-- schema.sql: gauntlet history store schema (user_version = 2).
--
-- Applied fresh (user_version == 0) via the migrate() stepwise switch in
-- store.go, which stamps a new database straight to the current version. An
-- existing v1 database is migrated in place instead (ALTER TABLE checks ADD
-- COLUMN output ...) rather than re-run against this file, so this file
-- always reflects the *current* schema, not the upgrade path — see
-- migrate()'s doc comment for the version-by-version steps.

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
  duration_ms  INTEGER NOT NULL
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,              -- passed|failed|skipped
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',   -- captured output, verbatim (executor tail-caps at 64KiB) (v2+)
  PRIMARY KEY (run_id, seq)
);
CREATE INDEX idx_checks_name ON checks(name);

CREATE TABLE queue_depth (
  at        INTEGER NOT NULL,
  target    TEXT NOT NULL,
  waiting   INTEGER NOT NULL,
  in_flight INTEGER NOT NULL,
  parked    INTEGER NOT NULL,
  PRIMARY KEY (at, target)
);
