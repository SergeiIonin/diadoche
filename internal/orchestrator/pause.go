package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/runlog"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
	"diadoche/internal/usagelimit"
)

const (
	// pauseInitialBackoff is the first wait after a limit refusal that
	// carries no reset time; each further one doubles it, up to
	// pauseMaxBackoff. A successful call starts it over.
	pauseInitialBackoff = 5 * time.Minute
	pauseMaxBackoff     = time.Hour

	// pauseWakePoll caps a single timer of the default sleep, so a laptop
	// suspended mid-pause wakes up on time by the wall clock.
	pauseWakePoll = 30 * time.Second

	// pauseMinWait is the wait used when the reply's reset time is already
	// past (clock skew), so the retry never spins.
	pauseMinWait = time.Minute
)

// ErrRunBudgetExceeded is wrapped by the error that stops a run once its
// recorded cost exceeds --max-cost-per-run.
var ErrRunBudgetExceeded = errors.New("run budget exceeded")

// budgetError is an exceeded --max-cost-per-task (task) or
// --max-cost-per-run (!task) budget.
type budgetError struct {
	task         bool
	spent, limit float64
}

func (e *budgetError) Error() string {
	if e.task {
		return fmt.Sprintf("budget exceeded: task cost $%.4f over --max-cost-per-task=$%.4f", e.spent, e.limit)
	}
	return fmt.Sprintf("budget exceeded: run cost $%.4f over --max-cost-per-run=$%.4f", e.spent, e.limit)
}

func (e *budgetError) Unwrap() error {
	if e.task {
		return nil
	}
	return ErrRunBudgetExceeded
}

// pauseInterruptedError is a limit pause cut short by a signal: the first
// one (a graceful stop, see Orchestrator.StopRequested) ends a pause at
// once, as does the run's context being cancelled. Its message is diadoche's own wording only — never the
// claude error text — so diadoche-restarter doesn't take it for a limit.
type pauseInterruptedError struct{ err error }

func (e *pauseInterruptedError) Error() string { return "interrupted while paused: " + e.err.Error() }
func (e *pauseInterruptedError) Unwrap() error { return e.err }

func (o *Orchestrator) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// sleep waits d or until ctx is done, whichever comes first, returning
// ctx.Err() in the latter case. o.Sleep replaces it in tests.
func (o *Orchestrator) sleep(ctx context.Context, d time.Duration) error {
	if o.Sleep != nil {
		return o.Sleep(ctx, d)
	}
	return sleepWallClock(ctx, time.Now().Add(d))
}

// sleepWallClock waits until the wall clock reaches until. Timers run on
// the monotonic clock, which stops while the machine is suspended, so it
// waits in steps of at most pauseWakePoll and re-reads the wall clock.
func sleepWallClock(ctx context.Context, until time.Time) error {
	for {
		left := time.Until(until)
		if left <= 0 {
			return nil
		}
		t := time.NewTimer(min(left, pauseWakePoll))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// runAgentPausing is runAgent that waits out claude usage and rate limits:
// a call refused by one (see usagelimit.Detect) marks the task PAUSED,
// sleeps until the reset time from the reply (else an exponential backoff)
// and retries the very same call, so the refusal spends no iteration. Any
// other error is returned as is. The log lines it writes are diadoche's
// own wording and never quote the claude error, so diadoche-restarter
// (usagelimit.IsLimitLine) doesn't kill a pausing diadoche.
func (o *Orchestrator) runAgentPausing(ctx context.Context, c agentCall, a *agents.Agent, prompt string) (agents.RunResult, error) {
	prefix := fmt.Sprintf("%s iter %d/%d: [%s]", c.specFile, c.iter, o.Cfg.Iter, a.Name)
	paused := false
	for {
		result, err := o.runAgent(ctx, c, a, prompt)
		if err == nil {
			if paused {
				if err := specs.SetStatus(o.Layout, c.specFile, specs.StatusInProgress); err != nil {
					o.Log.Printf("%s could not set status back to %s: %v", prefix, specs.StatusInProgress, err)
				} else {
					o.event(c.specFile, c.iter, a.Name, "status", runlog.F("status", specs.StatusInProgress))
				}
				o.Log.Printf("%s resumed after pause", prefix)
				o.event(c.specFile, c.iter, a.Name, "resume")
			}
			o.pauseBackoff = 0
			return result, nil
		}
		if ctx.Err() != nil {
			return result, err
		}
		now := o.now()
		hit, ok := usagelimit.Detect(err.Error()+"\n"+result.Output, result.ExitCode, now)
		if !ok {
			return result, err
		}

		var wait time.Duration
		reason := "claude " + hit.Kind.String()
		until := "backoff"
		if hit.HasReset {
			wait = hit.ResetAt.Sub(now)
			if wait <= 0 {
				wait = pauseMinWait
			}
			reason += ", resumes at " + hit.ResetAt.Format(time.RFC3339)
			until = "until " + hit.ResetAt.Format(time.RFC3339)
		} else {
			wait = o.pauseBackoff
			if wait <= 0 {
				wait = pauseInitialBackoff
			}
			o.pauseBackoff = min(wait*2, pauseMaxBackoff)
		}

		if err := specs.SetStatusReason(o.Layout, c.specFile, specs.StatusPaused, reason); err != nil {
			o.Log.Printf("%s could not record %s status: %v", prefix, specs.StatusPaused, err)
		} else {
			o.event(c.specFile, c.iter, a.Name, "status", runlog.F("status", specs.StatusPaused))
		}
		o.Log.Printf("%s paused: claude %s (%s), retrying the same call in %s", prefix, hit.Kind, until, wait)
		untilField := "-"
		if hit.HasReset {
			untilField = hit.ResetAt.Format(time.RFC3339)
		}
		o.event(c.specFile, c.iter, a.Name, "pause", runlog.F("kind", hit.Kind), runlog.F("wait", wait), runlog.F("until", untilField))
		sctx, cancel := o.gracefulCtx(ctx)
		err = o.sleep(sctx, wait)
		cancel()
		if err != nil {
			return result, &pauseInterruptedError{err}
		}
		paused = true
	}
}

// checkBudgets enforces --max-cost-per-run (this run's recorded spend) and
// --max-cost-per-task (the task's recorded spend, previous runs included)
// after an agent call. A nil uw (--dry-run) has no cost data: no check.
func (o *Orchestrator) checkBudgets(specFile string, uw *usage.Writer) error {
	if err := o.runBudgetExceeded(uw); err != nil {
		return err
	}
	if uw == nil || o.Cfg.MaxCostPerTaskUSD <= 0 {
		return nil
	}
	spent := o.priorCosts.ByTask[specFile] + uw.Summary.TaskCostUSD(specFile)
	if spent > o.Cfg.MaxCostPerTaskUSD {
		return &budgetError{task: true, spent: spent, limit: o.Cfg.MaxCostPerTaskUSD}
	}
	return nil
}

// runBudgetExceeded reports an exceeded --max-cost-per-run.
func (o *Orchestrator) runBudgetExceeded(uw *usage.Writer) error {
	if uw == nil || o.Cfg.MaxCostPerRunUSD <= 0 {
		return nil
	}
	if spent := uw.Summary.CostUSD(); spent > o.Cfg.MaxCostPerRunUSD {
		return &budgetError{spent: spent, limit: o.Cfg.MaxCostPerRunUSD}
	}
	return nil
}
