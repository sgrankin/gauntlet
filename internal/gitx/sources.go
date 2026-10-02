package gitx

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const sourcePrefix = "refs/gauntlet/source/"

func (r *Repo) touchSource(source string) error {
	dir := filepath.Join(r.dir, "source-retention")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// The file's mtime is last use, not the source commit's author date.
	return os.WriteFile(filepath.Join(dir, source), nil, 0600)
}

func (r *Repo) retainSource(ctx context.Context, source string) error {
	if _, err := r.run(ctx, "update-ref", sourcePrefix+source, source); err != nil {
		return err
	}
	return r.touchSource(source)
}

// RetainSources anchors inputs before their last other ref can move, and
// excludes them from pruning until every consumer releases its lease.
func (r *Repo) RetainSources(ctx context.Context, sources ...string) (func(), error) {
	r.sourceMu.Lock()
	defer r.sourceMu.Unlock()
	unique := make(map[string]bool)
	for _, source := range sources {
		if source == "" || unique[source] {
			continue
		}
		if len(source) != 40 && len(source) != 64 {
			return nil, fmt.Errorf("invalid source OID %q", source)
		}
		if _, err := hex.DecodeString(source); err != nil {
			return nil, fmt.Errorf("invalid source OID %q", source)
		}
		if err := r.retainSource(ctx, source); err != nil {
			return nil, err
		}
		unique[source] = true
	}
	if r.sourceUsers == nil {
		r.sourceUsers = make(map[string]int)
	}
	for source := range unique {
		r.sourceUsers[source]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.sourceMu.Lock()
			defer r.sourceMu.Unlock()
			for source := range unique {
				r.sourceUsers[source]--
				if r.sourceUsers[source] == 0 {
					delete(r.sourceUsers, source)
				}
			}
		})
	}, nil
}

type SourcePruneResult struct {
	Expired []string
	Unaged  []string
}

// PruneSources releases expired audit and review-cache refs. Live leases,
// fetches, and source normalization share the lock; Git GC runs separately.
func (r *Repo) PruneSources(ctx context.Context, cutoff time.Time, apply bool) (SourcePruneResult, error) {
	r.sourceMu.Lock()
	defer r.sourceMu.Unlock()
	var result SourcePruneResult
	out, err := r.run(ctx, "for-each-ref", "--format=%(refname) %(objectname)", sourcePrefix, "refs/gauntlet/reviews/")
	if err != nil {
		return result, err
	}
	refs := map[string][]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.HasPrefix(fields[0], sourcePrefix) && strings.TrimPrefix(fields[0], sourcePrefix) != fields[1] {
			return result, fmt.Errorf("unexpected source archive ref %s", fields[0])
		}
		refs[fields[1]] = append(refs[fields[1]], fields[0])
	}
	// A moving review head can leave a clock file with no ref. Age those
	// files out too; otherwise the retention bookkeeping itself grows forever.
	entries, err := os.ReadDir(filepath.Join(r.dir, "source-retention"))
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || (len(name) != 40 && len(name) != 64) {
			continue
		}
		if _, err := hex.DecodeString(name); err != nil {
			continue
		}
		if _, exists := refs[name]; !exists {
			refs[name] = nil
		}
	}
	sources := make([]string, 0, len(refs))
	for source := range refs {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		if r.sourceUsers[source] > 0 {
			continue
		}
		stamp := filepath.Join(r.dir, "source-retention", source)
		info, err := os.Stat(stamp)
		if os.IsNotExist(err) {
			result.Unaged = append(result.Unaged, source)
			if apply {
				if err := r.touchSource(source); err != nil {
					return result, err
				}
			}
			continue
		}
		if err != nil {
			return result, err
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if apply {
			for _, ref := range refs[source] {
				if _, err := r.run(ctx, "update-ref", "-d", ref, source); err != nil {
					return result, err
				}
			}
			if err := os.Remove(stamp); err != nil && !os.IsNotExist(err) {
				return result, err
			}
		}
		result.Expired = append(result.Expired, source)
	}
	return result, nil
}
