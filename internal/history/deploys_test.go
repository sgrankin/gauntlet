package history

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/core"
)

// Deploy history (v14+, docs/design/deployment.md's "Logs and history"):
// the deploys/deploy_nodes tables, their one writer (the terminal
// core.EventDeployFinished), and the two read methods the dashboard/API/MCP
// surfaces query.
//
// Every timestamp here is seeded as an offset from time.Now(), never a fixed
// calendar date: these rows are read back by ordering and by relative age,
// and a test that pins an absolute date starts failing for reasons that have
// nothing to do with the code under test.

// deployRecord builds a well-shaped core.DeployRecord: a green two-node
// graph unless the caller mutates it.
func deployRecord(runID, env, sha string, started time.Time) *core.DeployRecord {
	return &core.DeployRecord{
		RunID:       runID,
		Env:         env,
		DeploySHA:   sha,
		DeployedSHA: "prev0000000000000000000000000000000000000",
		Nodes: []core.CheckResult{
			{Name: "migrate", Seq: 1, Status: core.CheckPassed, Duration: 22 * time.Second, Output: "migrated", Command: []string{"./scripts/migrate"}},
			{Name: "app1", Seq: 2, Status: core.CheckPassed, Duration: 58 * time.Second, Output: "deployed", Command: []string{"./scripts/deploy", "app1"}},
		},
		Outcome:   core.OutcomeLanded,
		StartedAt: started,
		EndedAt:   started.Add(80 * time.Second),
	}
}

// deployFinished wraps rec in the terminal event that carries it, shaped the
// way core.ValidateEvent demands (lane coordinates on the event agreeing
// with the record's own).
func deployFinished(rec *core.DeployRecord) core.Event {
	return core.Event{
		Kind:        core.EventDeployFinished,
		At:          rec.EndedAt,
		RunID:       rec.RunID,
		DeployEnv:   rec.Env,
		DeploySHA:   rec.DeploySHA,
		DeployedSHA: rec.DeployedSHA,
		Deploy:      rec,
	}
}

// TestDeploySchema_FreshOpenStampsCurrentVersion: a brand-new database gets
// schema.sql applied whole and stamped straight at schemaVersion — the
// deploy tables included, queryable with no migration step at all.
func TestDeploySchema_FreshOpenStampsCurrentVersion(t *testing.T) {
	s := openTestStore(t)

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("fresh user_version = %d, want %d", version, schemaVersion)
	}
	if schemaVersion != 14 {
		t.Fatalf("schemaVersion = %d, want 14 (the deploy-tables version)", schemaVersion)
	}
	for _, table := range []string{"deploys", "deploy_nodes"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Errorf("fresh schema has no usable %s table: %v", table, err)
		}
	}
}

// TestMigrate_V13ToV14 seeds a genuine v13 database — the current schema
// with the two deploy tables removed and user_version stamped back — and
// confirms Open migrates it IN PLACE: the version lands on schemaVersion,
// the pre-existing run/check rows are still there afterwards (this is an
// additive migration, it must not rewrite anything), and a deploy record
// emitted after the migration round-trips.
func TestMigrate_V13ToV14(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v13.db")

	started := time.Now().Add(-3 * time.Hour)
	s13, err := Open(path)
	if err != nil {
		t.Fatalf("Open (seed): %v", err)
	}
	if err := s13.Emit(context.Background(), core.Event{
		Kind: core.EventLanded, Target: "main", RunID: "run-old",
		Record: sampleRecord("run-old", "main", started),
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Roll the database back to exactly what v13 was: no deploy tables (and
	// with them, no deploy indexes — SQLite drops a table's indexes with it).
	for _, stmt := range []string{
		`DROP TABLE deploy_nodes`,
		`DROP TABLE deploys`,
		`PRAGMA user_version = 13`,
	} {
		if _, err := s13.db.Exec(stmt); err != nil {
			t.Fatalf("roll back to v13: %q: %v", stmt, err)
		}
	}
	if err := s13.Close(); err != nil {
		t.Fatalf("close v13 handle: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != schemaVersion {
		t.Errorf("user_version after migrate = %d, want %d", version, schemaVersion)
	}

	// The v13 rows are untouched: an additive migration adds tables, it
	// never rewrites the ones already there.
	oldRun, oldChecks, err := s.Run("run-old")
	if err != nil {
		t.Fatalf("Run(run-old) after migrate: %v", err)
	}
	if oldRun.Target != "main" || len(oldChecks) != 2 {
		t.Errorf("pre-v14 run after migrate = %+v with %d checks, want target=main with 2", oldRun, len(oldChecks))
	}

	rec := deployRecord("deploy-1", "prod", "sha1", started)
	if err := s.Emit(context.Background(), deployFinished(rec)); err != nil {
		t.Fatalf("Emit deploy after migrate: %v", err)
	}
	got, nodes, err := s.Deploy("deploy-1")
	if err != nil {
		t.Fatalf("Deploy after migrate: %v", err)
	}
	if got.Env != "prod" || len(nodes) != 2 {
		t.Errorf("deploy after migrate = %+v with %d nodes, want env=prod with 2", got, len(nodes))
	}
}

// TestEmit_DeployFinished_WritesRunAndNodes is the write contract: one
// deploys row plus one deploy_nodes row per DECLARED node — including the
// blocked rows, whose blocked_by names the failed `after` edges that stopped
// them (the run's explicit failure attribution, never inferred from order).
func TestEmit_DeployFinished_WritesRunAndNodes(t *testing.T) {
	s := openTestStore(t)
	started := time.Now().Add(-30 * time.Minute)

	rec := deployRecord("deploy-red", "prod2", "sha9", started)
	rec.Outcome = core.OutcomeRejected
	rec.Culprit = "migrate"
	rec.Detail = "migrate failed"
	rec.Nodes = []core.CheckResult{
		{
			Name: "migrate", Seq: 1, Status: core.CheckFailed,
			Duration: 24 * time.Second, Output: "lock wait timeout",
			Command: []string{"./scripts/migrate"}, LogPath: "/logs/deploy-red/1-migrate.log.zst",
			Waited: 2 * time.Second, PeakRSS: 123456, UserCPU: 900 * time.Millisecond, SysCPU: 100 * time.Millisecond,
		},
		{Name: "app1", Seq: 2, Status: core.CheckBlocked, BlockedBy: []string{"migrate"}},
		{Name: "app2", Seq: 3, Status: core.CheckBlocked, BlockedBy: []string{"migrate"}},
	}
	if err := s.Emit(context.Background(), deployFinished(rec)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	row, nodes, err := s.Deploy("deploy-red")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if row.Env != "prod2" || row.DeploySHA != "sha9" || row.Outcome != "rejected" || row.Culprit != "migrate" || row.Detail != "migrate failed" {
		t.Errorf("deploy row = %+v, want the rejected prod2 run at sha9 with culprit migrate", row)
	}
	if row.Duration != 80*time.Second {
		t.Errorf("deploy duration = %s, want 80s (ended - started)", row.Duration)
	}
	if !row.StartedAt.Equal(started.Truncate(time.Millisecond)) {
		t.Errorf("deploy startedAt = %s, want %s", row.StartedAt, started.Truncate(time.Millisecond))
	}

	if len(nodes) != 3 {
		t.Fatalf("nodes = %d, want 3 (one per declared node, blocked rows included)", len(nodes))
	}
	if nodes[0].Name != "migrate" || nodes[0].Status != "failed" || nodes[0].Duration != 24*time.Second {
		t.Errorf("culprit node = %+v", nodes[0])
	}
	if nodes[0].Command != "./scripts/migrate" || nodes[0].LogPath == "" || nodes[0].Output != "lock wait timeout" {
		t.Errorf("culprit node provenance = %+v, want command/logPath/output all recorded", nodes[0])
	}
	if nodes[0].Waited != 2*time.Second || nodes[0].PeakRSS != 123456 || nodes[0].UserCPU != 900*time.Millisecond || nodes[0].SysCPU != 100*time.Millisecond {
		t.Errorf("culprit node usage = %+v, want the waited/usage columns round-tripped", nodes[0])
	}
	for _, n := range nodes[1:] {
		if n.Status != "blocked" || n.BlockedBy != "migrate" {
			t.Errorf("blocked node %s = status %q blockedBy %q, want blocked/migrate", n.Name, n.Status, n.BlockedBy)
		}
	}
}

// TestEmit_DeployFinished_ReEmitIsIdempotent: both writes are INSERT OR
// REPLACE keyed on the deploy's own primary keys, so re-delivering the same
// terminal event (a channel re-drain, a duplicated fan-out) costs redundant
// writes and nothing else — never a duplicate row, never a constraint error.
func TestEmit_DeployFinished_ReEmitIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rec := deployRecord("deploy-dup", "dev", "sha1", time.Now().Add(-10*time.Minute))

	for i := 0; i < 3; i++ {
		if err := s.Emit(ctx, deployFinished(rec)); err != nil {
			t.Fatalf("Emit #%d: %v", i+1, err)
		}
	}

	rows, err := s.RecentDeploys("", 10)
	if err != nil {
		t.Fatalf("RecentDeploys: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("deploys rows after 3 identical emits = %d, want 1", len(rows))
	}
	_, nodes, err := s.Deploy("deploy-dup")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(nodes) != 2 {
		t.Errorf("deploy_nodes rows after 3 identical emits = %d, want 2", len(nodes))
	}
}

// TestEmit_NonTerminalDeployEvents_WriteNothing: only the terminal event
// carries the record, and only the terminal event writes. A started or
// node-finished event reaching history is a no-op, not a partial row — the
// same stance writeRecord takes on per-check events.
func TestEmit_NonTerminalDeployEvents_WriteNothing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	events := []core.Event{
		{Kind: core.EventDeployStarted, At: time.Now(), RunID: "deploy-x", DeployEnv: "prod", DeploySHA: "sha1"},
		{
			Kind: core.EventDeployNodeFinished, At: time.Now(), RunID: "deploy-x",
			DeployEnv: "prod", DeploySHA: "sha1", CheckName: "migrate",
			Check: &core.CheckResult{Name: "migrate", Status: core.CheckPassed, Duration: time.Second},
		},
	}
	for _, ev := range events {
		if err := core.ValidateEvent(ev); err != nil {
			t.Fatalf("test fixture is not a well-shaped event: %v", err)
		}
		if err := s.Emit(ctx, ev); err != nil {
			t.Fatalf("Emit(kind %d): %v", ev.Kind, err)
		}
	}

	rows, err := s.RecentDeploys("", 10)
	if err != nil {
		t.Fatalf("RecentDeploys: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("deploys rows after non-terminal events = %d, want 0", len(rows))
	}
	var nodes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM deploy_nodes`).Scan(&nodes); err != nil {
		t.Fatalf("count deploy_nodes: %v", err)
	}
	if nodes != 0 {
		t.Errorf("deploy_nodes rows after non-terminal events = %d, want 0", nodes)
	}
}

// TestDeploy_SeqOrderAndUnknownID: nodes come back in spec order (seq, not
// insertion or name order), and an unknown deploy ID is sql.ErrNoRows —
// the sentinel every surface keys its 404 off, exactly as for Run.
func TestDeploy_SeqOrderAndUnknownID(t *testing.T) {
	s := openTestStore(t)
	rec := deployRecord("deploy-order", "prod", "sha1", time.Now().Add(-time.Hour))
	// Deliberately delivered out of spec order — a parallel graph concludes
	// its nodes in whatever order they finish, and Seq is what restores the
	// declaration order the page renders in.
	rec.Nodes = []core.CheckResult{
		{Name: "app2", Seq: 3, Status: core.CheckPassed},
		{Name: "migrate", Seq: 1, Status: core.CheckPassed},
		{Name: "app1", Seq: 2, Status: core.CheckSkipped},
	}
	if err := s.Emit(context.Background(), deployFinished(rec)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	_, nodes, err := s.Deploy("deploy-order")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	var got []string
	for _, n := range nodes {
		got = append(got, n.Name)
	}
	if len(got) != 3 || got[0] != "migrate" || got[1] != "app1" || got[2] != "app2" {
		t.Errorf("node order = %v, want spec order [migrate app1 app2]", got)
	}
	if nodes[0].Seq != 0 || nodes[2].Seq != 2 {
		t.Errorf("stored seq = %d..%d, want the 0-based positions the log filenames key on", nodes[0].Seq, nodes[2].Seq)
	}

	if _, _, err := s.Deploy("nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Deploy(unknown) = %v, want sql.ErrNoRows", err)
	}
}

// TestRecentDeploys_OrderingAndEnvFilter: newest first, and an empty env
// means every environment while a named one filters to that lane.
func TestRecentDeploys_OrderingAndEnvFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now()

	seeds := []struct {
		runID, env string
		ago        time.Duration
	}{
		{"deploy-a", "dev", 4 * time.Hour},
		{"deploy-b", "prod", 3 * time.Hour},
		{"deploy-c", "dev", 2 * time.Hour},
		{"deploy-d", "prod2", time.Hour},
	}
	for _, sd := range seeds {
		rec := deployRecord(sd.runID, sd.env, "sha-"+sd.runID, now.Add(-sd.ago))
		if err := s.Emit(ctx, deployFinished(rec)); err != nil {
			t.Fatalf("Emit %s: %v", sd.runID, err)
		}
	}

	all, err := s.RecentDeploys("", 10)
	if err != nil {
		t.Fatalf("RecentDeploys(all): %v", err)
	}
	var order []string
	for _, r := range all {
		order = append(order, r.RunID)
	}
	want := []string{"deploy-d", "deploy-c", "deploy-b", "deploy-a"}
	if len(order) != len(want) {
		t.Fatalf("RecentDeploys(all) = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("RecentDeploys(all) = %v, want newest-first %v", order, want)
		}
	}

	dev, err := s.RecentDeploys("dev", 10)
	if err != nil {
		t.Fatalf("RecentDeploys(dev): %v", err)
	}
	if len(dev) != 2 || dev[0].RunID != "deploy-c" || dev[1].RunID != "deploy-a" {
		t.Errorf("RecentDeploys(dev) = %+v, want [deploy-c deploy-a]", dev)
	}

	limited, err := s.RecentDeploys("", 2)
	if err != nil {
		t.Fatalf("RecentDeploys(limit 2): %v", err)
	}
	if len(limited) != 2 || limited[0].RunID != "deploy-d" {
		t.Errorf("RecentDeploys(limit 2) = %+v, want the 2 newest", limited)
	}

	if none, err := s.RecentDeploys("nosuchenv", 10); err != nil || len(none) != 0 {
		t.Errorf("RecentDeploys(unknown env) = %+v, %v, want empty and no error", none, err)
	}
}
