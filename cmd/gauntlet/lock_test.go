package main

import (
	"testing"
)

// TestAcquireLock_SecondAcquireFails proves S2's whole point: a second
// AcquireLock against a directory whose lock is already held must fail
// loudly rather than silently proceeding to sweep anything.
func TestAcquireLock_SecondAcquireFails(t *testing.T) {
	dir := t.TempDir()

	l1, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	defer l1.Close()

	_, err = AcquireLock(dir)
	if err == nil {
		t.Fatal("second AcquireLock succeeded while the first still holds the lock; want a refusal error")
	}
}

// TestAcquireLock_CloseReleases proves the fd is genuinely released on
// Close: a fresh AcquireLock against the same dir must succeed afterward.
func TestAcquireLock_CloseReleases(t *testing.T) {
	dir := t.TempDir()

	l1, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	l2, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after Close still failed: %v", err)
	}
	defer l2.Close()
}

// TestAcquireLock_CreatesStateDir covers the convenience of not requiring
// -state to already exist: AcquireLock is the first thing run() does with
// it, ahead of any other directory creation.
func TestAcquireLock_CreatesStateDir(t *testing.T) {
	dir := t.TempDir() + "/does-not-exist-yet"

	l, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer l.Close()
}

// TestAcquireLock_ThirdAcquireStillFailsAfterFirstReleased is a sanity check
// that release/reacquire cycles don't leave the lock permanently unusable:
// after l1 releases and l2 acquires, a third concurrent acquire must still
// be refused while l2 holds it.
func TestAcquireLock_ThirdAcquireStillFailsAfterFirstReleased(t *testing.T) {
	dir := t.TempDir()

	l1, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	l2, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("second AcquireLock: %v", err)
	}
	defer l2.Close()

	if _, err := AcquireLock(dir); err == nil {
		t.Fatal("third AcquireLock succeeded while the second still holds the lock; want a refusal error")
	}
}
