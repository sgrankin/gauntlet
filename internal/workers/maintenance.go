package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sgrankin/gauntlet/internal/services"
)

func (a *Agent) maintain(ctx context.Context, jobsDir string, idle bool) error {
	now := time.Now()
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(jobsDir, entry.Name())
		if _, active := a.running.Load(dir); active {
			continue
		}
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(dir, "result.json"))
		if os.IsNotExist(err) {
			if now.Sub(stat.ModTime()) > 24*time.Hour {
				if err = os.RemoveAll(dir); err != nil {
					return err
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		var result retainedResult
		if json.Unmarshal(data, &result) != nil {
			return fmt.Errorf("invalid retained worker result")
		}
		if now.Sub(result.At) > 24*time.Hour {
			if err = os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	unique := map[*services.Pool]bool{}
	for _, pool := range a.Services {
		if !unique[pool] {
			pool.Reap(ctx)
			unique[pool] = true
		}
	}
	if _, err = a.Objects.PruneSources(ctx, now.Add(-24*time.Hour), true); err != nil {
		return err
	}
	if idle && now.Sub(a.lastGC) > time.Hour {
		if err = a.Objects.CollectUnused(ctx); err != nil {
			return err
		}
		a.lastGC = now
	}
	return nil
}
