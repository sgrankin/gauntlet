package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoteProfilesRequireWorkersAndKeepRuntimeAuthorityOnWorker(t *testing.T) {
	for _, tc := range []struct {
		settings string
		bad      bool
	}{
		{`executor "ci" kind="remote" { labels "linux"; cpus "2"; memory "4g"; }`, true},
		{`workers {}
executor "ci" kind="remote" { labels "linux"; cpus "2"; memory "4g"; }`, false},
		{`workers {}
executor "ci" kind="remote" { image "uncontrolled"; }`, true},
		{`workers { listen "0.0.0.0:8081"; }`, true},
		{`workers { lease "2m"; reconnect-grace "1m"; }`, true},
	} {
		cfg, err := loadDaemonString(t, profileTestBase+tc.settings)
		if (err != nil) != tc.bad {
			t.Fatalf("%s: %+v %v", tc.settings, cfg, err)
		}
		if err == nil && cfg.Workers.TokenEnv != "GAUNTLET_WORKER_TOKEN" {
			t.Fatal("worker token default missing")
		}
	}
}

func TestWorkerConfigurationNamesProfilesAndBoundsCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.kdl")
	body := `scheduler "https://queue.example.test"
name "sql-1"
labels "linux" "sql"
cpus "8"
memory "24g"
max-executions 4
urgent-reserve 1
executor "local"
executor "sql" kind="container" { runtime "docker"; image "sql-ci@sha256:abc"; }
services { allow "container"; }
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWorker(path)
	if err != nil || cfg.MaxExecutions != 4 || len(cfg.Profiles) != 1 || cfg.Profiles[0].Name != "sql" {
		t.Fatalf("worker config: %+v %v", cfg, err)
	}
	for _, pair := range [][2]string{{"0", "1g"}, {"NaN", "1g"}, {"1", "999999999999999999999999999g"}, {"1", "0"}} {
		if _, _, err := ResourceValues(pair[0], pair[1]); err == nil {
			t.Fatalf("accepted invalid resources %v", pair)
		}
	}
}
