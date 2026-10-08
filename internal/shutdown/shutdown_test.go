package shutdown

import (
	"context"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type logRec struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRec) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, format)
}

func (l *logRec) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func cancelledWithin(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(time.Second):
		return false
	}
}

func TestFirstSignalStopsThenTimeoutCancels(t *testing.T) {
	sigs := make(chan os.Signal, 4)
	var l logRec
	ctx, stop, release := Watch(context.Background(), sigs, 200*time.Millisecond, l.logf)
	defer release()

	sigs <- syscall.SIGINT
	if !closed(stop) {
		t.Fatal("stopRequested not closed by the first signal")
	}
	if ctx.Err() != nil {
		t.Fatal("ctx cancelled right on the first signal; want it alive until the timeout")
	}
	if !cancelledWithin(ctx, 2*time.Second) {
		t.Fatal("ctx not cancelled after --shutdown-timeout")
	}
	if !strings.Contains(l.text(), "--shutdown-timeout reached") {
		t.Errorf("log = %q, want the timeout line", l.text())
	}
}

func TestSecondSignalCancelsAtOnce(t *testing.T) {
	sigs := make(chan os.Signal, 4)
	var l logRec
	ctx, stop, release := Watch(context.Background(), sigs, time.Hour, l.logf)
	defer release()

	sigs <- syscall.SIGINT
	if !closed(stop) {
		t.Fatal("stopRequested not closed")
	}
	if cancelledWithin(ctx, 50*time.Millisecond) {
		t.Fatal("ctx cancelled by the first signal")
	}
	sigs <- syscall.SIGINT
	if !cancelledWithin(ctx, time.Second) {
		t.Fatal("second signal did not cancel ctx")
	}
	if !strings.Contains(l.text(), "second signal") {
		t.Errorf("log = %q, want the second-signal line", l.text())
	}
}

func TestZeroTimeoutCancelsOnFirstSignal(t *testing.T) {
	sigs := make(chan os.Signal, 4)
	ctx, stop, release := Watch(context.Background(), sigs, 0, func(string, ...any) {})
	defer release()

	sigs <- syscall.SIGTERM
	if !cancelledWithin(ctx, time.Second) {
		t.Fatal("timeout 0: first signal did not cancel ctx")
	}
	if !closed(stop) {
		t.Fatal("stopRequested not closed")
	}
}

// diadoche-restarter sends SIGTERM to the process group every 200 ms: the
// repeat is a second signal, so the stop is immediate despite the timeout.
func TestRestarterStyleBurstCancelsAtOnce(t *testing.T) {
	sigs := make(chan os.Signal, 4)
	ctx, _, release := Watch(context.Background(), sigs, time.Hour, func(string, ...any) {})
	defer release()

	start := time.Now()
	go func() {
		for range 3 {
			sigs <- syscall.SIGTERM
			time.Sleep(50 * time.Millisecond)
		}
	}()
	if !cancelledWithin(ctx, time.Second) {
		t.Fatal("repeated SIGTERM did not cancel ctx")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("cancel took %s, want it within the burst", d)
	}
}

func TestReleaseCancelsAndStops(t *testing.T) {
	sigs := make(chan os.Signal, 4)
	ctx, stop, release := Watch(context.Background(), sigs, time.Hour, func(string, ...any) {})
	release() // returns only once the goroutine has exited
	if ctx.Err() == nil {
		t.Error("release did not cancel ctx")
	}
	select {
	case <-stop:
		t.Error("release closed stopRequested; no signal was received")
	default:
	}
	release() // idempotent
}

func TestParentCancelCancels(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, _, release := Watch(parent, make(chan os.Signal), time.Hour, func(string, ...any) {})
	defer release()
	cancel()
	if !cancelledWithin(ctx, time.Second) {
		t.Fatal("parent cancel did not cancel ctx")
	}
}
