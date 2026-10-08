// Package restarter keeps diadoche running across claude usage-limit
// windows: it runs diadoche as a child process, watches its output for the
// claude CLI's "you've hit your limit" error, and when that shows up kills
// diadoche, drops every row from STATUS.md that isn't DONE, BLOCKED or a
// parked FAILED (so the next run re-queues those tasks instead of seeing
// them as FAILED), sleeps until the window reopens and starts diadoche
// again.
package restarter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"diadoche/internal/speclock"
	"diadoche/internal/specs"
)

// lockTimeout bounds how long ResetStatus waits for the spec directory lock.
const lockTimeout = 2 * time.Minute

// killGrace is how long diadoche gets to exit after SIGTERM before SIGKILL.
const killGrace = 10 * time.Second

type Restarter struct {
	Bin    string   // diadoche binary
	Args   []string // forwarded to every diadoche run
	Layout specs.Layout

	// ResetBuffer is added on top of the parsed reset time, so the restart
	// never races the usage window actually reopening.
	ResetBuffer time.Duration
	// FallbackWait is how long to wait when the limit message carries no
	// reset time diadoche-restarter can parse.
	FallbackWait time.Duration
	// MaxRestarts caps the number of restarts; 0 = unlimited.
	MaxRestarts int

	Stdout, Stderr io.Writer
	Log            *log.Logger

	// Now and Sleep are overridable for tests.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// ExitError carries diadoche's own non-zero exit code, so the restarter can
// exit with it.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("diadoche exited with status %d", e.Code) }

// Run starts diadoche and restarts it after every usage-limit hit. It
// returns nil once diadoche exits successfully, an *ExitError if diadoche
// fails for any reason other than a usage limit, and ctx's error if
// interrupted.
func (r *Restarter) Run(ctx context.Context) error {
	restarts := 0
	for {
		hitLine, err := r.runOnce(ctx)
		if hitLine == "" {
			return err
		}

		if r.MaxRestarts > 0 && restarts >= r.MaxRestarts {
			return fmt.Errorf("usage limit hit again, but --max-restarts=%d already reached", r.MaxRestarts)
		}
		restarts++

		removed, err := ResetStatus(r.Layout)
		if err != nil {
			return fmt.Errorf("reset %s: %w", r.Layout.StatusPath, err)
		}
		if len(removed) > 0 {
			r.Log.Printf("restarter: removed unfinished task(s) from STATUS.md so they're re-queued: %v", removed)
		}

		now := r.now()
		wake, ok := ParseResetTime(hitLine, now)
		if ok {
			wake = wake.Add(r.ResetBuffer)
			r.Log.Printf("restarter: usage window reopens at %s", wake.Add(-r.ResetBuffer).Format(time.RFC1123))
		} else {
			wake = now.Add(r.FallbackWait)
			r.Log.Printf("restarter: couldn't parse a reset time from the limit message, retrying after --fallback-wait=%s", r.FallbackWait)
		}
		// Never spin: even a reset time already in the past waits at least
		// ResetBuffer before the next attempt.
		if minWake := now.Add(r.ResetBuffer); wake.Before(minWake) {
			wake = minWake
		}
		r.Log.Printf("restarter: restart #%d scheduled for %s (in %s)", restarts, wake.Format(time.RFC1123), wake.Sub(now).Round(time.Second))
		if err := r.sleep(ctx, wake.Sub(now)); err != nil {
			return err
		}
		r.Log.Printf("restarter: restarting diadoche (restart #%d)", restarts)
	}
}

// runOnce runs diadoche to completion, or until its output shows a usage
// limit error — then diadoche is killed right away, so it doesn't go on to
// fail (and mark FAILED) the rest of the queue against an exhausted
// account. hitLine is the offending output line, "" if no limit was hit.
func (r *Restarter) runOnce(ctx context.Context) (hitLine string, err error) {
	hits := make(chan string, 1)
	onLine := func(line string) {
		if IsLimitLine(line) {
			select {
			case hits <- line:
			default:
			}
		}
	}
	stdout := &lineWatcher{out: r.Stdout, onLine: onLine}
	stderr := &lineWatcher{out: r.Stderr, onLine: onLine}

	cmd := exec.Command(r.Bin, r.Args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	setProcessGroup(cmd)
	// A claude grandchild that outlives diadoche could hold the output
	// pipes open forever; WaitDelay bounds how long Wait waits for them.
	cmd.WaitDelay = killGrace

	r.Log.Printf("restarter: starting %s %v", r.Bin, r.Args)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start %s: %w", r.Bin, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	select {
	case hitLine = <-hits:
		r.Log.Printf("restarter: usage limit hit, stopping diadoche")
		waitErr = r.stop(cmd, done)
	case <-ctx.Done():
		r.stop(cmd, done)
		return "", ctx.Err()
	case waitErr = <-done:
	}
	stdout.flush()
	stderr.flush()

	// diadoche may have exited on its own right after printing the limit
	// error, before the select above saw it.
	if hitLine == "" {
		select {
		case hitLine = <-hits:
		default:
		}
	}
	if hitLine != "" {
		return hitLine, nil
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return "", &ExitError{Code: exitErr.ExitCode()}
	}
	return "", waitErr
}

// stop terminates diadoche's whole process group (diadoche plus any claude
// it has in flight): SIGTERM, repeated so a child forked while the first
// one was in flight is caught too, then SIGKILL after killGrace.
func (r *Restarter) stop(cmd *exec.Cmd, done <-chan error) error {
	deadline := time.After(killGrace)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		terminate(cmd)
		select {
		case err := <-done:
			return err
		case <-tick.C:
		case <-deadline:
			kill(cmd)
			return <-done
		}
	}
}

func (r *Restarter) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// wakePoll bounds how late a restart can be after the machine wakes from
// system sleep.
const wakePoll = 30 * time.Second

func (r *Restarter) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	return sleepUntil(ctx, time.Now().Add(d), time.Now, wakePoll)
}

// sleepUntil waits until the wall clock reaches wake. A single timer of the
// whole duration would run on the monotonic clock, which on macOS stops
// while the system sleeps, so a restart scheduled across a laptop's sleep
// would fire as late as the sleep lasted. Instead it waits in steps of at
// most poll and re-checks the wall clock after each one.
func sleepUntil(ctx context.Context, wake time.Time, now func() time.Time, poll time.Duration) error {
	wake = wake.Round(0) // compare wall clocks, never monotonic readings
	for {
		left := wake.Sub(now().Round(0))
		if left <= 0 {
			return nil
		}
		t := time.NewTimer(min(left, poll))
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
	}
}

// ResetStatus removes every row but DONE ones from STATUS.md, returning the
// removed specs. The next diadoche run's sync re-adds those task files as
// QUEUED, and resumes each one from the plan/reviews already on disk. A
// FAILED task whose changes diadoche already parked (it has a
// spec/<NNN>_FAILED.md) genuinely failed, so it's kept as is. So is a
// BLOCKED task (with its reason): it waits for a human to resolve its
// merge, and re-queuing it would rerun the task while its worktree and
// branch are still there.
func ResetStatus(l specs.Layout) ([]string, error) {
	lock, err := speclock.Acquire(l.SpecDir, lockTimeout)
	if err != nil {
		return nil, fmt.Errorf("acquire spec lock: %w", err)
	}
	defer lock.Release()

	// Temp files of a STATUS.md/QUEUE.md write that the kill interrupted.
	for _, name := range []string{l.StatusPath, l.QueuePath} {
		leftovers, _ := filepath.Glob(filepath.Join(l.SpecDir, "."+filepath.Base(name)+".tmp*"))
		for _, f := range leftovers {
			_ = os.Remove(f)
		}
	}

	rows, err := specs.ReadStatus(l.StatusPath)
	if err != nil {
		return nil, err
	}
	var kept []specs.Row
	var removed []string
	for _, row := range rows {
		if row.Status == specs.StatusDone || row.Status == specs.StatusBlocked ||
			row.Status == specs.StatusFailed && parked(l, row.Spec) {
			kept = append(kept, row)
		} else {
			removed = append(removed, row.Spec)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	return removed, specs.WriteStatus(l.StatusPath, kept)
}

// parked reports whether diadoche parked specFile's changes when it FAILED.
func parked(l specs.Layout, specFile string) bool {
	_, err := os.Stat(filepath.Join(l.SpecDir, specs.TaskNumber(specFile)+"_FAILED.md"))
	return err == nil
}

// lineWatcher passes everything written to it straight through to out,
// and calls onLine for each complete line (plus a trailing partial one on
// flush).
type lineWatcher struct {
	out    io.Writer
	onLine func(string)

	mu  sync.Mutex
	buf []byte
}

func (w *lineWatcher) Write(p []byte) (int, error) {
	if _, err := w.out.Write(p); err != nil {
		return 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.onLine(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *lineWatcher) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.onLine(string(w.buf))
		w.buf = nil
	}
}
