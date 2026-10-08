package orchestrator

// The limit gate is how a run waits out claude usage and rate limits: one
// per run (runShared.gate), and every agent call goes through it (see
// runAgentPausing). Its state machine:
//
//	open → paused → degraded → open
//
// open: calls start freely. The first call refused by a limit makes its
// task the pause leader: the gate turns paused, and no call starts until
// the leader has slept until the reset (or the shared backoff) once. A call
// refused while the gate is paused makes its task a follower: it doesn't
// sleep on its own — N tasks hitting the limit together must not become N
// pauses — but waits for the leader's pause to end, then retries its call.
// After the pause the gate is degraded: the run's effective parallelism is
// 1 (no new task starts, see Orchestrator.effectiveParallel, and at most
// one call runs at a time) until a call that started after the pause
// succeeds, which opens the gate again. A refusal while degraded starts a
// new pause, with the backoff doubled when the reply has no reset time.
//
// With --parallel 1 there is never a second task, so the gate only keeps
// the serial pause's backoff: the behaviour is the one before the gate.
//
// The gate's methods never do I/O or call into the orchestrator, so its
// mutex never wraps a slow operation.

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type gateState int

const (
	gateOpen gateState = iota
	gatePaused
	gateDegraded
)

// limitGate is the run's usage-limit gate (see the top of this file). All
// its fields are guarded by mu.
type limitGate struct {
	mu    sync.Mutex
	state gateState

	// released is closed and replaced at every change a waiter may be
	// waiting for: the end of a pause, a freed degraded slot, recovery.
	released chan struct{}

	// wake is signalled (never blocking) when the gate opens again, so
	// the dispatcher fills the freed slots without waiting for a task to
	// finish.
	wake chan struct{}

	// backoff is the wait of the next time-less pause; 0 means
	// pauseInitialBackoff. Reset by a successful call while open.
	backoff time.Duration

	// epoch counts the pauses; a ticket carries the epoch its call
	// started in, so only a call that started after the last pause
	// restores the degree.
	epoch uint64
	// slotBusy is the one call allowed at a time while degraded.
	slotBusy bool

	pauses    int // pauses started, for tests
	followers int // refusals that joined a pause, for tests
	waiting   int // calls blocked in acquire now, for tests
}

// gateTicket is one call's pass through the gate, handed back to done.
type gateTicket struct {
	epoch uint64
	slot  bool // the call holds the degraded slot
}

func newLimitGate() *limitGate {
	return &limitGate{released: make(chan struct{}), wake: make(chan struct{}, 1)}
}

// notify wakes every waiter; mu must be held.
func (g *limitGate) notify() {
	close(g.released)
	g.released = make(chan struct{})
}

// acquire waits until a call may start: never while paused, and while
// degraded only once the degraded slot is free (which it then takes). A
// cancelled ctx returns ctx's error; a graceful stop (stop or internal)
// one wrapping ErrInterrupted.
func (g *limitGate) acquire(ctx context.Context, stop, internal <-chan struct{}) (gateTicket, error) {
	counted := false
	defer func() {
		if counted {
			g.mu.Lock()
			g.waiting--
			g.mu.Unlock()
		}
	}()
	for {
		g.mu.Lock()
		switch {
		case g.state == gateOpen:
			t := gateTicket{epoch: g.epoch}
			g.mu.Unlock()
			return t, nil
		case g.state == gateDegraded && !g.slotBusy:
			g.slotBusy = true
			t := gateTicket{epoch: g.epoch, slot: true}
			g.mu.Unlock()
			return t, nil
		}
		ch := g.released
		if !counted {
			counted = true
			g.waiting++
		}
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return gateTicket{}, ctx.Err()
		case <-stop:
			return gateTicket{}, fmt.Errorf("waiting for the run's limit pause: %w", ErrInterrupted)
		case <-internal:
			return gateTicket{}, fmt.Errorf("waiting for the run's limit pause: %w", ErrInterrupted)
		}
	}
}

// done ends the call of ticket t; ok reports it succeeded. It frees the
// degraded slot, and a success of a call that started after the last
// pause opens the gate again: recovered reports that.
func (g *limitGate) done(t gateTicket, ok bool) (recovered bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	changed := false
	if t.slot {
		g.slotBusy = false
		changed = true
	}
	if ok && g.state == gateDegraded && t.epoch == g.epoch {
		g.state = gateOpen
		recovered, changed = true, true
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
	if ok && g.state == gateOpen {
		g.backoff = 0
	}
	if changed {
		g.notify()
	}
	return recovered
}

// refused records a call refused by a limit, after done(t, false). While
// the gate is paused the caller is a follower: wait is closed when the
// pause ends. Otherwise the caller leads a new pause (the gate is paused
// now), and degradedNow reports the gate was open before it: the run's
// degradation starts here.
func (g *limitGate) refused() (leader, degradedNow bool, wait <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == gatePaused {
		g.followers++
		return false, false, g.released
	}
	degradedNow = g.state == gateOpen
	g.state = gatePaused
	g.pauses++
	g.epoch++
	return true, degradedNow, nil
}

// nextBackoff is the wait of a time-less pause: pauseInitialBackoff, then
// doubling up to pauseMaxBackoff until a success starts it over.
func (g *limitGate) nextBackoff() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	wait := g.backoff
	if wait <= 0 {
		wait = pauseInitialBackoff
	}
	g.backoff = min(wait*2, pauseMaxBackoff)
	return wait
}

// endPause is the leader's end of its pause, slept out or interrupted:
// the gate turns degraded and the followers retry. After an interrupted
// sleep the run is stopping anyway, which the waiters see themselves.
func (g *limitGate) endPause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state = gateDegraded
	g.notify()
}

// effective is the run's effective parallelism for --parallel n: 1 while
// paused or degraded, else n.
func (g *limitGate) effective(n int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state != gateOpen {
		return 1
	}
	return n
}

func (g *limitGate) pauseCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pauses
}

func (g *limitGate) followerCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.followers
}

func (g *limitGate) waiters() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiting
}
