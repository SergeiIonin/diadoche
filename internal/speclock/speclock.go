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
	"time"
)

// StaleAfter is how long a lock file may exist before it's considered
// abandoned (e.g. the process that created it crashed) and safe to steal.
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

		if stale, statErr := isStale(path); statErr == nil && stale {
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

func isStale(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return time.Since(info.ModTime()) > StaleAfter, nil
}

// Release removes the lock file.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	return os.Remove(l.path)
}
