package gitx

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// OpenObjects opens a worker-local bare object store without configuring a
// remote or installing repository credentials.
func OpenObjects(ctx context.Context, dir string) (*Repo, error) {
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
			return nil, err
		}
		if _, err := runGit(ctx, "", nil, "init", "--bare", "-q", dir); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return &Repo{dir: dir}, nil
}

func validObject(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	_, err := hex.DecodeString(oid)
	return err == nil
}

// WritePack transports the complete reachable objects for wanted commits,
// excluding ancestry the receiving worker already possesses.
func (r *Repo) WritePack(ctx context.Context, out io.Writer, wants, haves []string) error {
	if len(wants) == 0 || len(wants) > 256 || len(haves) > 128 {
		return fmt.Errorf("gitx: invalid pack object count")
	}
	var revisions strings.Builder
	for _, oid := range wants {
		if !validObject(oid) {
			return fmt.Errorf("gitx: invalid wanted object")
		}
		fmt.Fprintln(&revisions, oid)
	}
	for _, oid := range haves {
		if !validObject(oid) {
			return fmt.Errorf("gitx: invalid known object")
		}
		if _, err := r.run(ctx, "cat-file", "-e", oid+"^{commit}"); err == nil {
			fmt.Fprintln(&revisions, "^"+oid)
		}
	}
	command := exec.CommandContext(ctx, "git", "--git-dir="+r.dir, "pack-objects", "--stdout", "--revs", "--delta-base-offset")
	command.Stdin = strings.NewReader(revisions.String())
	command.Stdout = out
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("gitx: write source pack: %w: %s", err, stderr.String())
	}
	return nil
}

func (r *Repo) ImportPack(ctx context.Context, source io.Reader) error {
	_, err := runGit(ctx, r.dir, source, "index-pack", "--stdin", "--fix-thin", "--strict")
	return err
}

func (r *Repo) ObjectTree(ctx context.Context, commit string) (string, error) {
	if !validObject(commit) {
		return "", fmt.Errorf("gitx: invalid commit identity")
	}
	tree, err := r.run(ctx, "rev-parse", "--verify", commit+"^{tree}")
	return strings.TrimSpace(tree), err
}

// CollectUnused reclaims old worker-cache objects after expired source refs
// have been removed. Call while the worker has no active materializations.
func (r *Repo) CollectUnused(ctx context.Context) error {
	r.sourceMu.Lock()
	defer r.sourceMu.Unlock()
	_, err := r.run(ctx, "gc", "--quiet", "--prune=24.hours.ago")
	return err
}
