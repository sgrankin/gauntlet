package flaky

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOnlyGitToolsBoundRevisionsAndPaths(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	os.WriteFile(filepath.Join(dir, "source.go"), []byte("source evidence"), 0600)
	git("add", "source.go")
	git("-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "source")
	sha := git("rev-parse", "HEAD")
	tools := readerTools{capability: toolContext{GitDir: filepath.Join(dir, ".git"), Revisions: []string{sha}}}
	out, err := tools.call(context.Background(), "read_file", toolInput{Revision: sha, Path: "source.go"})
	if err != nil || out != "source evidence" {
		t.Fatalf("read: %q %v", out, err)
	}
	for _, in := range []toolInput{{Revision: "HEAD", Path: "source.go"}, {Revision: sha, Path: "../../etc/passwd"}, {Revision: sha, Path: "/etc/passwd"}, {Revision: sha, Base: "HEAD"}} {
		name := "read_file"
		if in.Base != "" {
			name = "diff"
		}
		if _, err := tools.call(context.Background(), name, in); err == nil {
			t.Fatalf("accepted forbidden read: %+v", in)
		}
	}
	tools.calls = 24
	if _, err := tools.call(context.Background(), "paths", toolInput{Revision: sha}); err == nil {
		t.Fatal("unbounded tool calls")
	}
}
