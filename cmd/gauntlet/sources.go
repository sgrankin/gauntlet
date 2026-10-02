package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
	"github.com/sgrankin/gauntlet/internal/gitx"
)

func runPruneSources(args []string) error { return pruneSourcesTo(args, os.Stdout) }

func pruneSourcesTo(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("prune-sources", flag.ContinueOnError)
	path := flags.String("config", "", "daemon config [required]")
	state := flags.String("state", "", "stopped daemon state directory [required]")
	retention := flags.Duration("retention", 30*24*time.Hour, "time since the source's last use")
	apply := flags.Bool("apply", false, "delete expired local audit and review-cache refs (default: preview)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || *state == "" || *retention <= 0 || len(flags.Args()) != 0 {
		return fmt.Errorf("-config, -state and positive -retention required")
	}
	cfg, err := config.LoadDaemon(*path)
	if err != nil {
		return err
	}
	lock, err := AcquireLock(*state)
	if err != nil {
		return err
	}
	defer lock.Close()
	dir, err := filepath.Abs(filepath.Join(*state, "repos", remoteKey(cfg.Remote)))
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return fmt.Errorf("no existing daemon repository: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repo, err := gitx.Open(ctx, dir)
	if err != nil {
		return err
	}
	result, err := repo.PruneSources(ctx, time.Now().Add(-*retention), *apply)
	if err != nil {
		return err
	}
	verb := "would prune"
	if *apply {
		verb = "pruned"
	}
	for _, source := range result.Expired {
		fmt.Fprintln(out, verb, source)
	}
	fmt.Fprintf(out, "%d expired sources; %d unaged sources (a full retention window starts with -apply).\n", len(result.Expired), len(result.Unaged))
	return nil
}
