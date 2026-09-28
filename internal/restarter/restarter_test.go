package restarter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diadoche/internal/specs"
)

func TestResetStatusKeepsOnlyDoneAndParkedFailed(t *testing.T) {
	l := specs.NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	rows := []specs.Row{
		{Num: "001", Spec: "001_a.md", Status: specs.StatusDone},
		{Num: "002", Spec: "002_b.md", Status: specs.StatusFailed},
		{Num: "003", Spec: "003_c.md", Status: specs.StatusInProgress},
		{Num: "004", Spec: "004_d.md", Status: specs.StatusQueued},
		{Num: "005", Spec: "005_e.md", Status: specs.StatusFailed},
	}
	if err := specs.WriteStatus(l.StatusPath, rows); err != nil {
		t.Fatal(err)
	}
	// 005 FAILED for real and was parked; 002 was only marked FAILED by the
	// interruption, so it must be re-queued.
	if err := os.WriteFile(filepath.Join(l.SpecDir, "005_FAILED.md"), []byte("parked"), 0o644); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(l.SpecDir, ".QUEUE.md.tmp123")
	if err := os.WriteFile(leftover, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := ResetStatus(l)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"002_b.md", "003_c.md", "004_d.md"}; fmt.Sprint(removed) != fmt.Sprint(want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	got, err := specs.ReadStatus(l.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Spec != "001_a.md" || got[1].Spec != "005_e.md" {
		t.Errorf("STATUS.md rows = %+v, want only 001_a.md and 005_e.md", got)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("interrupted-write temp file not cleaned up: %v", err)
	}
}

// fakeDiadoche writes a shell script standing in for diadoche. Each run
// appends a line to runs.log; the first `limitRuns` runs mark 002 FAILED
// (as diadoche's failTask does), print a limit error and then hang — the
// restarter must kill them. Later runs exit with finalCode.
func fakeDiadoche(t *testing.T, dir string, limitRuns, finalCode int) string {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
echo run >> %[1]q/runs.log
n=$(wc -l < %[1]q/runs.log)
if [ "$n" -le %[2]d ]; then
  printf '| # | Spec | Status |\n|---|------|--------|\n| 001 | 001_a.md | DONE |\n| 002 | 002_b.md | FAILED |\n' > %[3]q
  echo 'task 002_b.md: error: planner: exit status 1: {"is_error":true,"result":"You'"'"'ve hit your session limit · resets 7pm (UTC)"}'
  sleep 30
fi
exit %[4]d
`, dir, limitRuns, specs.NewLayout(dir).StatusPath, finalCode)
	path := filepath.Join(dir, "fake-diadoche.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestRestarter(t *testing.T, dir, bin string, slept *[]time.Duration) *Restarter {
	now := time.Date(2026, 9, 24, 15, 0, 0, 0, time.UTC)
	l := specs.NewLayout(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	return &Restarter{
		Bin:          bin,
		Layout:       l,
		ResetBuffer:  2 * time.Minute,
		FallbackWait: 30 * time.Minute,
		Stdout:       &bytes.Buffer{},
		Stderr:       io.Discard,
		Log:          log.New(io.Discard, "", 0),
		Now:          func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			*slept = append(*slept, d)
			return nil
		},
	}
}

func runCount(t *testing.T, dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "runs.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

func TestRunRestartsAfterLimitAtResetTime(t *testing.T) {
	dir := t.TempDir()
	var slept []time.Duration
	r := newTestRestarter(t, dir, fakeDiadoche(t, dir, 2, 0), &slept)

	start := time.Now()
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Error("hanging diadoche wasn't killed after the limit error")
	}
	if n := runCount(t, dir); n != 3 {
		t.Errorf("diadoche ran %d times, want 3", n)
	}
	// now = 15:00 UTC, reset 19:00 UTC, + 2m buffer.
	want := 4*time.Hour + 2*time.Minute
	if len(slept) != 2 || slept[0] != want || slept[1] != want {
		t.Errorf("slept %v, want 2 x %s", slept, want)
	}
	rows, _ := specs.ReadStatus(r.Layout.StatusPath)
	for _, row := range rows {
		if row.Status != specs.StatusDone {
			t.Errorf("non-DONE row left in STATUS.md: %+v", row)
		}
	}
	if !strings.Contains(r.Stdout.(*bytes.Buffer).String(), "hit your session limit") {
		t.Error("diadoche output wasn't passed through")
	}
}

func TestRunPropagatesNonLimitExit(t *testing.T) {
	dir := t.TempDir()
	var slept []time.Duration
	r := newTestRestarter(t, dir, fakeDiadoche(t, dir, 0, 3), &slept)

	err := r.Run(context.Background())
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 {
		t.Fatalf("Run err = %v, want ExitError{3}", err)
	}
	if len(slept) != 0 || runCount(t, dir) != 1 {
		t.Errorf("unexpected restart: slept=%v runs=%d", slept, runCount(t, dir))
	}
}

func TestRunMaxRestarts(t *testing.T) {
	dir := t.TempDir()
	var slept []time.Duration
	r := newTestRestarter(t, dir, fakeDiadoche(t, dir, 5, 0), &slept)
	r.MaxRestarts = 1

	err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--max-restarts=1") {
		t.Fatalf("Run err = %v, want max-restarts error", err)
	}
	if n := runCount(t, dir); n != 2 {
		t.Errorf("diadoche ran %d times, want 2", n)
	}
}

func TestSleepUntilFollowsTheWallClockAcrossSystemSleep(t *testing.T) {
	start := time.Now()
	wake := start.Add(time.Hour)
	calls := 0
	// The first reading is before the machine sleeps; every later one is
	// after it woke up past the wake time, while only milliseconds of
	// monotonic time went by.
	now := func() time.Time {
		calls++
		if calls == 1 {
			return start
		}
		return start.Add(time.Hour + time.Minute)
	}
	done := make(chan error, 1)
	go func() { done <- sleepUntil(context.Background(), wake, now, 10*time.Millisecond) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sleepUntil: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sleepUntil kept waiting after the wall clock passed the wake time")
	}
}

func TestSleepUntilReturnsAtOnceForAPastWake(t *testing.T) {
	if err := sleepUntil(context.Background(), time.Now().Add(-time.Minute), time.Now, time.Hour); err != nil {
		t.Fatalf("sleepUntil: %v", err)
	}
}

func TestSleepUntilStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sleepUntil(ctx, time.Now().Add(time.Hour), time.Now, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
