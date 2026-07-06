package services

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sgrankin/gauntlet/internal/config"
)

func TestRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	rec := Record{
		Key: "abc123", Name: "gauntlet-svc-tok-abc123456789", Repo: "https://example.test/repo.git",
		Mode: "network", Spec: config.Service{Name: "db", Image: "img", Port: 1, ReadyTimeout: time.Second, IdleTTL: time.Hour},
		ContainerID: "cid", Endpoint: Endpoint{Host: "h", Port: "1"}, Network: "gauntlet-svc-tok",
		CreatedAt: now, LastUsed: now,
	}

	if err := writeRecord(dir, rec); err != nil {
		t.Fatalf("writeRecord: %v", err)
	}

	records, err := listRecords(dir)
	if err != nil {
		t.Fatalf("listRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("listRecords: got %d records, want 1", len(records))
	}
	if records[0].Key != rec.Key || records[0].Spec.Name != rec.Spec.Name {
		t.Fatalf("listRecords[0] = %+v, want match for %+v", records[0], rec)
	}

	got, err := readRecord(dir, rec.Key)
	if err != nil {
		t.Fatalf("readRecord: %v", err)
	}
	if !got.LastUsed.Equal(rec.LastUsed) {
		t.Errorf("readRecord.LastUsed = %v, want %v", got.LastUsed, rec.LastUsed)
	}

	touched := now.Add(4 * time.Hour)
	if err := touchRecordLastUsed(dir, rec.Key, touched); err != nil {
		t.Fatalf("touchRecordLastUsed: %v", err)
	}
	got, err = readRecord(dir, rec.Key)
	if err != nil {
		t.Fatalf("readRecord after touch: %v", err)
	}
	if !got.LastUsed.Equal(touched) {
		t.Errorf("after touch, LastUsed = %v, want %v", got.LastUsed, touched)
	}
	if got.ContainerID != rec.ContainerID {
		t.Errorf("touch mutated an unrelated field: ContainerID = %q, want %q", got.ContainerID, rec.ContainerID)
	}

	// No leftover temp files after either atomic write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("leftover non-json file after atomic write: %s", e.Name())
		}
	}
}

func TestListRecordsSkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/garbage.json", []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := Record{Key: "good1", Name: "n", Repo: "r", Mode: "network", CreatedAt: time.Now(), LastUsed: time.Now()}
	if err := writeRecord(dir, good); err != nil {
		t.Fatal(err)
	}

	records, err := listRecords(dir)
	if err != nil {
		t.Fatalf("listRecords: %v", err)
	}
	if len(records) != 1 || records[0].Key != "good1" {
		t.Fatalf("listRecords = %+v, want just the well-formed record", records)
	}
}

func TestRemoveRecord(t *testing.T) {
	dir := t.TempDir()
	rec := Record{Key: "gone1", Name: "n", Repo: "r", Mode: "network"}
	if err := writeRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	removeRecord(dir, rec.Key)
	if _, err := readRecord(dir, rec.Key); err == nil {
		t.Fatal("readRecord after removeRecord: want error, got nil")
	}
	// Idempotent: removing an already-gone record must not error/panic.
	removeRecord(dir, rec.Key)
}
