package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sgrankin/gauntlet/internal/gitx"
	"github.com/sgrankin/gauntlet/internal/testutil"
)

func TestPruneSourcesRequiresStoppedDaemon(t *testing.T) {
	state := t.TempDir()
	remote := testutil.NewRemote(t)
	cfg := filepath.Join(t.TempDir(), "daemon.kdl")
	body := `remote "` + remote.Dir + `"
 committer { name "Queue"; email "queue@example.com"; }
 target "main" { branch "main"; }
 `
	if err := os.WriteFile(cfg, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.New(context.Background(), filepath.Join(state, "repos", remoteKey(remote.Dir)), remote.Dir); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(state)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-config", cfg, "-state", state, "-apply"}
	var output bytes.Buffer
	err = pruneSourcesTo(args, &output)
	lock.Close()
	if err == nil || !strings.Contains(err.Error(), "another gauntlet daemon") {
		t.Fatalf("live daemon was not protected: %v", err)
	}
	if err := pruneSourcesTo(args, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "0 expired") {
		t.Fatal(output.String())
	}
}
