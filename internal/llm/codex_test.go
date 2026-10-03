package llm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	for _, name := range []string{"success", "missing output", "oversized output", "process failure", "cancel"} {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "codex")
			script := `#!/bin/sh
set -eu
output=''
all="$*"
case "$all" in *model_reasoning_effort=*high*) ;; *) exit 7;; esac
while test "$#" -gt 0; do
 if test "$1" = --output-last-message; then shift; output="$1"; fi
 shift
done
cat > prompt
 grep -q 'summarize changes' prompt
`
			switch name {
			case "success":
				script += `printf '%s' '{"summary":"Fix timeouts."}' > "$output"` + "\n"
			case "oversized output":
				script += `head -c 8193 /dev/zero > "$output"` + "\n"
			case "process failure":
				script += "exit 9\n"
			case "cancel":
				script += "while :; do sleep 1; done\n"
			}
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if name == "cancel" {
				cancel()
			}
			got, err := (Codex{Auth: "api-key", Token: "secret", Executable: binary, Model: "chosen-model", Effort: "high"}).Run(ctx, Request{Prompt: "summarize changes", Schema: []byte(`{"type":"object"}`)})
			if name == "success" {
				if err != nil || string(got) != `{"summary":"Fix timeouts."}` {
					t.Fatalf("got=%s err=%v", got, err)
				}
			} else if err == nil {
				t.Fatal("want error")
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("error leaked credential")
			}
		})
	}
}
