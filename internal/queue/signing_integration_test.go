package queue

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/executor"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

func TestIntegration_SignedLandingIsTestedCommit(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(allowed, append([]byte(testCommitter.Email+" "), pub...), 0600); err != nil {
		t.Fatal(err)
	}
	gated := executor.NewGatedExecutor()
	h := newIntegrationHarness(t, nil, gated, config.Target{Name: "main", Branch: "main", Landing: "squash"})
	h.git, err = gitx.New(context.Background(), h.dir, h.remote.Dir, gitx.WithSSHSigning(key, time.Second*10))
	if err != nil {
		t.Fatal(err)
	}
	h.d.git = h.git
	h.remote.Seed("main", checkSpecFile("test"))
	h.remote.PushCandidate("main", "alice", "signed", map[string]string{"new.txt": "change"})
	h.reconcile()
	r := h.d.headRun("main")
	if r == nil {
		t.Fatal("no signed trial")
	}
	tip := r.chainTip
	h.awaitStarted(gated, r.runID, "test")
	if out, err := h.gitDirQuery("-c", "gpg.format=ssh", "-c", "gpg.ssh.allowedSignersFile="+allowed, "verify-commit", tip); err != nil {
		t.Fatalf("tested commit signature: %v %s", err, out)
	}
	h.releaseGated(gated, r.runID, "test", core.CheckResult{Name: "test", Status: core.CheckPassed})
	if got := h.remote.Ref("refs/heads/main"); got != tip {
		t.Fatalf("landed %s, tested signed commit %s", got, tip)
	}
}
