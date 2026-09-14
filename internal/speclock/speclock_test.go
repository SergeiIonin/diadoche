package speclock

import (
	"os"
	"path/filepath"
	"strconv"
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

// TestAcquireStealsLockFromDeadProcessImmediately guards restart-after-crash:
// if diadoche is killed while holding the spec lock, a restart right after
// must recover immediately rather than waiting out the full StaleAfter
// window — that's exactly the scenario a user hits when they Ctrl-C or the
// process is killed mid-Sync and then re-run diadoche a few seconds later.
func TestAcquireStealsLockFromDeadProcessImmediately(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A PID that (almost certainly) doesn't exist, written just now — fresh
	// mtime, so only the liveness check (not the time-based fallback) can
	// explain a fast steal.
	if err := os.WriteFile(lockPath, []byte("999999999\n"+time.Now().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	lock, err := Acquire(dir, 3*time.Second)
	if err != nil {
		t.Fatalf("expected lock held by a dead PID to be stolen quickly, got error: %v", err)
	}
	defer lock.Release()

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Acquire took %s — expected near-instant steal of a dead process's lock, not a timeout-driven one", elapsed)
	}
}

func TestAcquireDoesNotStealLockFromLiveProcess(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, ".lock")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Our own PID: very much alive for the duration of this test.
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())+"\n"+time.Now().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Acquire(dir, 500*time.Millisecond); err == nil {
		t.Fatal("expected Acquire to time out, not steal a lock owned by a live process")
	}
}
