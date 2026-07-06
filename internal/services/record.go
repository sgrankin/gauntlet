package services

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
)

// Record is the on-disk snapshot of one pool instance, persisted under
// <state>/services/<full-keyhash>.json (docs/plans/services.md §3 "Pool
// records"; docs/plans/services-impl.md §3.5 — this struct's JSON shape is
// pinned there). Records are efficiency hints, not truth (DESIGN.md
// Invariant 4): boot (Pool.Adopt) treats the driver's live-instance listing
// as truth and these records as hints toward matching it — a live instance
// with no matchable record is destroyed (slower, never wrong), and a
// record with no live instance is simply stale and ignored. No SQLite.
type Record struct {
	Key         string         `json:"key"`
	Name        string         `json:"name"`
	Repo        string         `json:"repo"`
	Mode        string         `json:"mode"`
	Spec        config.Service `json:"spec"`
	ContainerID string         `json:"containerID"`
	Endpoint    Endpoint       `json:"endpoint"`
	Network     string         `json:"network,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	LastUsed    time.Time      `json:"lastUsed"`
}

// Endpoint is Record's host+port pair, split out to mirror the pinned JSON
// shape ({"host": "…", "port": "…"}) rather than flattening it into Record.
type Endpoint struct {
	Host string `json:"host"`
	Port string `json:"port"`
}

func recordPath(stateDir, key string) string {
	return filepath.Join(stateDir, key+".json")
}

// writeRecord atomically (write-temp-then-rename, same directory so the
// rename is same-filesystem and thus atomic) persists rec under stateDir.
// Used both for the initial write after Create and for every subsequent
// touch (lastUsed on Release, M3) — always a full rewrite, never a partial
// patch, so a crash mid-write can never leave a record holding a mix of old
// and new fields.
func writeRecord(stateDir string, rec Record) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("services: marshal record %s: %w", rec.Key, err)
	}
	tmp, err := os.CreateTemp(stateDir, rec.Key+".*.tmp")
	if err != nil {
		return fmt.Errorf("services: create temp record: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("services: write temp record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("services: close temp record: %w", err)
	}
	if err := os.Rename(tmpName, recordPath(stateDir, rec.Key)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("services: rename record %s: %w", rec.Key, err)
	}
	return nil
}

// readRecord reads and unmarshals key's record from stateDir.
func readRecord(stateDir, key string) (Record, error) {
	data, err := os.ReadFile(recordPath(stateDir, key))
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("services: unmarshal record %s: %w", key, err)
	}
	return rec, nil
}

// listRecords reads every *.json record under stateDir. A malformed record
// is skipped, not fatal — records are hints (Record's doc); a boot must not
// fail because one file got corrupted by a crash racing this very
// atomic-replace scheme (temp+rename bounds the window, not zero on every
// possible host filesystem/crash combination).
func listRecords(stateDir string) ([]Record, error) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil, fmt.Errorf("services: list records: %w", err)
	}
	var records []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, e.Name()))
		if err != nil {
			continue
		}
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			continue
		}
		records = append(records, rec)
	}
	return records, nil
}

// removeRecord deletes key's record file, if any, after eviction/destroy —
// best-effort: a leftover record for a now-gone instance is merely a stale
// hint the next Adopt will fail to match and clean up anyway.
func removeRecord(stateDir, key string) {
	os.Remove(recordPath(stateDir, key))
}

// touchRecordLastUsed rewrites key's record with LastUsed=now (M3: the idle
// clock starts when the last in-flight reference drops, so Release — never
// Ensure — is what moves this). Best-effort from the caller's point of
// view: if the record doesn't exist (e.g. an eviction raced this touch)
// there's nothing to touch, and the error is not load-bearing (records are
// hints, not truth).
func touchRecordLastUsed(stateDir, key string, now time.Time) error {
	rec, err := readRecord(stateDir, key)
	if err != nil {
		return err
	}
	rec.LastUsed = now
	return writeRecord(stateDir, rec)
}
