// Package shutdown turns SIGINT/SIGTERM into diadoche's two-stage stop.
// The first signal asks the run to stop once the agent call in flight has
// finished; the call itself is killed only when --shutdown-timeout runs out
// or a second signal arrives. diadoche-restarter repeats its SIGTERM every
// 200 ms, so for it the stop is immediate.
package shutdown

import (
	"context"
	"os"
	"time"
)

// Watch reads sigs until release is called or parent is done.
//
// stopRequested is closed on the first signal: the run should stop at the
// next point between agent calls. ctx (the "hard" context) is cancelled by
// a second signal, by timeout counted from the first signal, or right on
// the first signal when timeout <= 0; cancelling it kills in-flight agent
// calls. release stops watching and cancels ctx.
//
// sigs is a channel rather than signal.Notify so tests can send fake
// signals.
func Watch(parent context.Context, sigs <-chan os.Signal, timeout time.Duration, logf func(string, ...any)) (ctx context.Context, stopRequested <-chan struct{}, release func()) {
	ctx, cancel := context.WithCancel(parent)
	stop := make(chan struct{})
	quit := make(chan struct{})
	exited := make(chan struct{})

	go func() {
		defer close(exited)
		defer cancel()
		var timer <-chan time.Time
		stopped := false
		for {
			select {
			case sig := <-sigs:
				if stopped {
					logf("second signal (%s): exiting immediately", sig)
					return
				}
				stopped = true
				close(stop)
				if timeout <= 0 {
					logf("received %s: stopping", sig)
					return
				}
				logf("received %s: finishing the current agent call (up to --shutdown-timeout=%s), then saving state and exiting; send it again to exit immediately", sig, timeout)
				t := time.NewTimer(timeout)
				defer t.Stop()
				timer = t.C
			case <-timer:
				logf("--shutdown-timeout reached: stopping the agent call")
				return
			case <-ctx.Done():
				return
			case <-quit:
				return
			}
		}
	}()

	release = func() {
		select {
		case <-quit:
		default:
			close(quit)
		}
		<-exited
	}
	return ctx, stop, release
}
