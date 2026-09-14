// Package speclock provides a simple interprocess advisory lock over the
// .claude/entelecheia/spec directory, so the main loop (reading the
// directory and STATUS.md/QUEUE.md) and the preprocessor (writing new task
// spec files into the same directory from a separate process) never observe
// each other's half-finished writes.
package speclock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// StaleAfter is the fallback: how long a lock file may exist before it's
// considered abandoned regardless of whether its owning process can be
// checked. Acquire also steals the lock immediately, well before this, once
// it can positively confirm the PID that created it is dead — see isStale.
const StaleAfter = 10 * time.Minute

const pollInterval = 200 * time.Millisecond

// Lock represents a held lock on a spec directory. Release it when done.
type Lock struct {
	path string
}

// Acquire creates specDir if needed and takes an exclusive lock on it,
// waiting up to timeout for a concurrent holder to release (or go stale).
func Acquire(specDir string, timeout time.Duration) (*Lock, error) {
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(specDir, ".lock")
	deadline := time.Now().Add(timeout)

	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
			f.Close()
			return &Lock{path: path}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}

		if isStale(path) {
			// Best-effort steal of an abandoned lock; if another process
			// wins the race, the next loop iteration's OpenFile fails again
			// and we simply keep retrying.
			os.Remove(path)
			continue
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for lock %s", timeout, path)
		}
		time.Sleep(pollInterval)
	}
}

// isStale reports whether the lock at path was abandoned: either its owning
// process can be confirmed dead (so a restart right after a crash recovers
// immediately, instead of waiting out StaleAfter), or the file is simply
// older than StaleAfter as a fallback for when liveness can't be checked.
func isStale(path string) bool {
	if pid, ok := readPID(path); ok && !processAlive(pid) {
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > StaleAfter
}

func readPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether pid is (still) a running process. Any
// ambiguity (permission error, unsupported on this platform, ...) is
// resolved as "alive" — the caller only wants a positive, safe signal that
// a lock's owner is gone, never a false one that could make two holders
// think they both own the lock.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true // can't determine either way; don't steal on a guess
	}
	// Signal 0 does no actual signaling, just an existence/permission check.
	// Every diadoche process runs as the current OS user, so in practice the
	// only errors here are "no such process" (dead → steal) — a permission
	// error from some other user's process is not a realistic case for this
	// single-user, local lock file.
	return proc.Signal(syscall.Signal(0)) == nil
}

// Release removes the lock file.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	return os.Remove(l.path)
}
