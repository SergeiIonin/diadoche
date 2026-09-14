package speclock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireReleaseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	lock, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".lock")); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".lock")); !os.IsNotExist(err) {
		t.Fatalf("lock file still present after release: err=%v", err)
	}
}

func TestAcquireBlocksConcurrentHolder(t *testing.T) {
	dir := t.TempDir()
	lock, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if _, err := Acquire(dir, 300*time.Millisecond); err == nil {
		t.Fatal("expected second Acquire to time out while lock is held")
	}
}

func TestAcquireStealsStaleLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleTime := time.Now().Add(-2 * StaleAfter)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	lock, err := Acquire(dir, time.Second)
	if err != nil {
		t.Fatalf("expected stale lock to be stolen, got error: %v", err)
	}
	defer lock.Release()
}
