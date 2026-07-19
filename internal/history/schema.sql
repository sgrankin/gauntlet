-- schema.sql: gauntlet history store schema (user_version = 12).
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
  -- batch_id/position/batch_size (v5+): batch_id groups the per-member
  -- records of one batch run (empty for
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
  recovered    INTEGER NOT NULL DEFAULT 0,
  -- receipt_ref/receipt_blob/receipt_published (v12+): the receipt-notes
  -- publication provenance of a run whose note was actually confirmed
  -- published (issue #13; core.RunRecord.ReceiptRef/ReceiptBlob/
  -- ReceiptPublished verbatim). receipt_ref is the configured notes ref the
  -- receipt was published under; receipt_blob is the published note's blob
  -- SHA; receipt_published is a small vocabulary ("published" for a fresh
  -- note commit, "already-present" for PublishNote's idempotent
  -- AlreadyPublished outcome — still a landing success). All three are '' when
  -- receipt-notes policy is disabled or the spec declares no receipt.
  -- NOT necessarily '' for a non-landed run: landRun stamps these three
  -- fields onto every member's record immediately after a successful
  -- PublishNote, before the target CAS is even attempted — so a run whose
  -- publish succeeded but then lost the target race (stale CAS, crash) still
  -- carries them despite ending Skipped/Error, not Landed. That orphan case
  -- is deliberate: it is exactly the row that most needs this data for
  -- diagnosis (a confirmed-published note with no landing to show for it).
  -- The receipt node's own execution (its command's pass/fail) already
  -- lands as an ordinary row in the checks table below, under
  -- "receipt:<name>" — these three columns are ONLY the publication fact,
  -- not a duplicate of that row.
  receipt_ref       TEXT NOT NULL DEFAULT '',
  receipt_blob      TEXT NOT NULL DEFAULT '',
  receipt_published TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_runs_target_started ON runs(target, started_at DESC);
CREATE INDEX idx_runs_batch_id ON runs(batch_id) WHERE batch_id != '';

CREATE TABLE checks (
  run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL,              -- passed|failed|skipped|blocked (v9+)
  duration_ms INTEGER NOT NULL,
  err         TEXT NOT NULL DEFAULT '',
  output      TEXT NOT NULL DEFAULT '',   -- captured output, verbatim (executor tail-caps at 64KiB) (v2+)
  log_path    TEXT NOT NULL DEFAULT '',   -- full per-check log file path, if one was written (v3+)
  -- command (v8+): the check's argv (core.CheckJob.Command), shell-joined
  -- into one display string by writeRecord — run.html's command echo above
  -- a check's captured output. Empty for rows written before v8, in which
  -- case run.html renders no echo line at all rather than a blank one.
  command     TEXT NOT NULL DEFAULT '',
  -- blocked_by (v9+): comma-joined names of the prerequisites whose
  -- non-green end blocked this check (status 'blocked' rows only) — the
  -- run's explicit failure attribution, never inferred from row order.
  blocked_by  TEXT NOT NULL DEFAULT '',
  -- waited_ms (v9+): how long the check sat ready but slotless under the
  -- daemon-wide max-executions cap before starting — capacity starvation,
  -- as distinct from duration_ms (the command's own cost).
  waited_ms   INTEGER NOT NULL DEFAULT 0,
  -- image (v10+): the immutable candidate-built image identity — a build
  -- node's captured result, or the identity a consumer check ran in.
  image       TEXT NOT NULL DEFAULT '',
  -- materialize_ms (v11+): how long this node's private isolated workspace
  -- took to materialize (git archive + history-mtime pass) before its
  -- command ran — isolated-workspace mode only, zero in shared mode.
  materialize_ms INTEGER NOT NULL DEFAULT 0,
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
-- (target, ref) (core.EventRetryRequested): upserted on every retry, one
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
-- ref whose target segment names no configured target — a misconfiguration):
-- one row per occurrence, so an operator not watching the log/Slack at
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
-- (core.EventHookStarted/EventHookSkipped; a durable owed/skipped marker,
-- no auto-resume): one row per run_id, written the instant the
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
