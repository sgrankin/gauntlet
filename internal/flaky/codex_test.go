package flaky

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/llm"
)

func TestCodexServiceAccountIsolation(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-codex")
	t.Setenv("GITHUB_TOKEN", "must-not-inherit")
	t.Setenv("OPENAI_API_KEY", "must-not-inherit")
	script := `#!/bin/sh
set -eu
test "$CODEX_ACCESS_TOKEN" = 'service-token'
test -z "${GITHUB_TOKEN-}"
test -z "${OPENAI_API_KEY-}"
test "$HOME" = "$CODEX_HOME"
test ! -f "$CODEX_HOME/auth.json"
output=''
all="$*"
case "$all" in *service-token*) exit 8;; esac
case "$all" in *--ignore-user-config*--ignore-rules*--ephemeral*--skip-git-repo-check*--sandbox*read-only*features.shell_tool=false*web_search=*--model*chosen-model*) ;; *) exit 9;; esac
while test "$#" -gt 0; do
 if test "$1" = --output-last-message; then shift; output="$1"; fi
 shift
done
cat > input.json
grep -q 'Failure JSON' input.json
printf '%s' '{"action":"retry","confidence":0.95,"reason":"transient timeout"}' > "$output"
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c := Codex{Client: llm.Codex{Auth: "chatgpt", Executable: binary, Token: "service-token", Model: "chosen-model"}, MaxOutputBytes: 256}
	decision, err := c.Classify(context.Background(), core.CheckJob{Name: "test", Dir: "candidate-workspace"}, core.CheckResult{Output: "timeout"})
	if err != nil || decision.Action != "retry" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestCodexAPIKeyIsolation(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
set -eu
test -z "${CODEX_ACCESS_TOKEN-}"
test -z "${GITHUB_TOKEN-}"
grep -q 'api-secret' "$CODEX_HOME/auth.json"
output=''
while test "$#" -gt 0; do
 if test "$1" = --output-last-message; then shift; output="$1"; fi
 shift
done
cat > /dev/null
printf '%s' '{"action":"abstain","confidence":0.5,"reason":"uncertain"}' > "$output"
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_ACCESS_TOKEN", "must-not-inherit")
	t.Setenv("GITHUB_TOKEN", "must-not-inherit")
	c := Codex{Client: llm.Codex{Auth: "api-key", Executable: binary, Token: "api-secret", Model: "model"}, MaxOutputBytes: 256}
	decision, err := c.Classify(context.Background(), core.CheckJob{}, core.CheckResult{})
	if err != nil || decision.Action != "abstain" {
		t.Fatalf("%+v %v", decision, err)
	}
}
