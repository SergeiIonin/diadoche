package orchestrator

// --parallel N runs up to N tasks that are ready by the dependency graph at
// the same time. Run's own goroutine is then both the dispatcher and the
// merge owner: it starts tasks (worktree, IN_PROGRESS) and does everything
// that touches the main working tree, the current branch or the spec
// directory as a whole — merges, bookkeeping commits, --on-task-done hooks,
// the ADR step, parking, the spec lock and QUEUE.md. A task goroutine only
// runs the task's iterations (and its --on-fail chain) in its own worktree,
// then hands the outcome back over a channel; the owner finishes the
// outcomes one at a time, in the order they arrive, which is the order the
// tasks finished. A merge conflict re-executes the task once from the new
// HEAD (see reexecTask). With N == 1 none of this exists: Run is the
// serial loop.
//
// Every task works through its own shallow copy of the Orchestrator, its
// "view" (see taskView): the view's log lines carry the task's id, and its
// cur record is its own. What all views share sits behind the runShared
// pointer.
//
// Usage limits: one limit gate per run (see limitgate.go). A limit refusal
// in any task pauses the whole run once; tasks refused meanwhile wait with
// it, and no call starts while paused. Afterwards the effective
// parallelism is 1 — no new task starts and one call runs at a time —
// until a call that started after the pause succeeds; both transitions
// are logged and written to run.log as event=parallelism.
//
// Budgets: --max-cost-per-run counts every task together. Once it is
// crossed no new task starts, and the tasks in flight stop at the end of
// their current iteration (see runIterations), DONE ones being merged.
// --max-cost-per-task and --max-call-cost stay per task and per call.
//
// What the tasks share is either behind a mutex or has a single owner:
//   - usage.Writer and usage.Summary (usage.jsonl and the run's totals):
//     mutex; every record is one Write of a whole line.
//   - priorCosts (previous runs' totals): set once by Run before any task
//     goroutine, read-only afterwards.
//   - run.log (runlog.Run): mutex; every event is one Write of a line.
//   - run artifacts: every task writes its own directory only, from its
//     own goroutine or the owner after it; CallName's counters are under
//     runlog.Run's mutex, the decisions map under runShared.mu.
//   - cost bookkeeping: an iterationTally belongs to one task goroutine;
//     budgets read the mutex-guarded usage.Summary; the limit gate (pause
//     state, shared backoff, counters) has its own mutex.
//   - STATUS.md / QUEUE.md: diadoche's read-modify-writes go through
//     runShared.specMu (mutex); the spec-directory lock (speclock) is taken
//     only by the merge owner, never once per task goroutine.
//   - the ADR step and docs/adr/: single owner, it runs in finishTask on
//     the merge owner, so ADR numbers are assigned one task at a time.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"diadoche/internal/agents"
	"diadoche/internal/gitutil"
	"diadoche/internal/runlog"
	"diadoche/internal/runstate"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
)

// runShared is the state every view of a run shares.
type runShared struct {
	// mu guards active, resumes, waiting and the decisions map, and
	// serializes run-state.json writes.
	mu sync.Mutex

	// active are the records of the tasks running now, in start order.
	active []*runstate.Task
	// resumes are saved records a restarted run continues with, not taken
	// by a task yet, in file order.
	resumes []runstate.Task
	// waiting are the run-state.json records of WAITING tasks, keyed by
	// spec file, each as it was before the call that answered NEEDS_HUMAN
	// (see waiting.go); a re-queued WAITING task resumes from its record.
	waiting map[string]runstate.Task

	// specMu serializes diadoche's own read-modify-writes of STATUS.md and
	// QUEUE.md. The tester agent writes STATUS.md itself and can't take
	// it: a write of diadoche's that straddles the agent's edit could
	// still lose it, a window of milliseconds that Sync's skipping of
	// unchanged files narrows further.
	specMu sync.Mutex

	// stop, once closed, stops every task at its next point between agent
	// calls, like the first signal: a fatal error in one task.
	stop     chan struct{}
	stopOnce sync.Once

	// gate is the run's usage-limit gate, every agent call goes through
	// it (see limitgate.go); it has its own mutex.
	gate *limitGate

	// smokeBrowserWarned are the tasks that already got the "browser step
	// skipped" warning of the smoke check, keyed by spec file; guarded by
	// mu, created lazily.
	smokeBrowserWarned map[string]bool
}

// ownerJob is work a task goroutine needs done by the merge owner (the
// --on-fail split's spec-locked section); done is closed once fn ran.
type ownerJob struct {
	fn   func()
	done chan struct{}
}

// shared returns the run's shared state, creating it on first use (always
// before any task goroutine exists).
func (o *Orchestrator) shared() *runShared {
	if o.sh == nil {
		o.sh = &runShared{stop: make(chan struct{}), gate: newLimitGate()}
	}
	return o.sh
}

// effectiveParallel is how many tasks may be in flight now: parallel(),
// but 1 while the limit gate is paused or degraded (see limitgate.go).
// runParallel's dispatcher is the only place that applies it.
func (o *Orchestrator) effectiveParallel() int {
	return o.shared().gate.effective(o.parallel())
}

// parallel is how many tasks run at once: --parallel, but 1 without
// worktrees or offline.
func (o *Orchestrator) parallel() int {
	if o.offline() || !o.Cfg.Worktree {
		return 1
	}
	return max(1, o.Cfg.Parallel)
}

// taskView is o for one task of a parallel run: its log lines start with
// "[<NNN>-<slug>] ", and it has its own run-state record; everything else,
// the limit gate included, is shared with o.
func (o *Orchestrator) taskView(specFile string) *Orchestrator {
	v := *o
	id := taskID(specFile, specs.TaskNumber(specFile))
	v.Log = log.New(o.Log.Writer(), o.Log.Prefix()+"["+id+"] ", o.Log.Flags()|log.Lmsgprefix)
	v.cur = nil
	return &v
}

// requestInternalStop stops every task at its next point between agent
// calls (see runShared.stop).
func (o *Orchestrator) requestInternalStop() {
	sh := o.shared()
	sh.stopOnce.Do(func() { close(sh.stop) })
}

// onOwner runs fn on the merge owner and waits for it; in the serial loop,
// or on the owner itself, it runs fn right away. false means ctx ended
// before the owner took the job: fn did not run.
func (o *Orchestrator) onOwner(ctx context.Context, fn func()) bool {
	if o.owner == nil {
		fn()
		return true
	}
	job := ownerJob{fn: fn, done: make(chan struct{})}
	select {
	case o.owner <- job:
	case <-ctx.Done():
		return false
	}
	<-job.done
	return true
}

// The spec helpers below are package specs' read-modify-writes of
// STATUS.md and QUEUE.md, serialized by runShared.specMu.

func (o *Orchestrator) setStatus(specFile, status string) error {
	return o.setStatusReason(specFile, status, "")
}

func (o *Orchestrator) setStatusReason(specFile, status, reason string) error {
	sh := o.shared()
	sh.specMu.Lock()
	defer sh.specMu.Unlock()
	return specs.SetStatusReason(o.Layout, specFile, status, reason)
}

func (o *Orchestrator) getStatus(specFile string) (string, error) {
	sh := o.shared()
	sh.specMu.Lock()
	defer sh.specMu.Unlock()
	return specs.GetStatus(o.Layout, specFile)
}

func (o *Orchestrator) blockDependents(specFile string) ([]specs.Row, error) {
	sh := o.shared()
	sh.specMu.Lock()
	defer sh.specMu.Unlock()
	return specs.BlockDependents(o.Layout, specFile)
}

func (o *Orchestrator) blockForDependency(specFile string, dep specs.Row) ([]specs.Row, error) {
	sh := o.shared()
	sh.specMu.Lock()
	defer sh.specMu.Unlock()
	return specs.BlockForDependency(o.Layout, specFile, dep)
}

func (o *Orchestrator) popQueue() (string, error) {
	sh := o.shared()
	sh.specMu.Lock()
	defer sh.specMu.Unlock()
	return specs.PopQueue(o.Layout)
}

func (o *Orchestrator) writeQueue(items []string) error {
	sh := o.shared()
	sh.specMu.Lock()
	defer sh.specMu.Unlock()
	return specs.WriteQueue(o.Layout.QueuePath, items)
}

// removeFromQueue drops specFile from QUEUE.md: it has started.
func (o *Orchestrator) removeFromQueue(specFile string) error {
	sh := o.shared()
	sh.specMu.Lock()
	defer sh.specMu.Unlock()
	queue, err := specs.ReadQueue(o.Layout.QueuePath)
	if err != nil || !slices.Contains(queue, specFile) {
		return err
	}
	return specs.WriteQueue(o.Layout.QueuePath, slices.DeleteFunc(queue, func(s string) bool { return s == specFile }))
}

func (o *Orchestrator) syncSpecs() error {
	sh := o.shared()
	sh.specMu.Lock()
	defer sh.specMu.Unlock()
	_, _, err := specs.Sync(o.Layout)
	return err
}

// taskRun is one started task: what startTask set up and runTask and
// finishTask work with.
type taskRun struct {
	specFile, num string
	// view is the Orchestrator the task runs with (o itself when serial).
	view *Orchestrator
	// resume is the saved record the task continues from, if any.
	resume *runstate.Task
	ws     *workspace
	// attempt is 2 for a task re-executed after a merge conflict.
	attempt int
	// priorAttempt is the history of the first attempt, given to planner
	// and worker in the first iteration of the second (see reexecTask).
	priorAttempt string
}

// taskOutcome is how runTask left a task, for finishTask.
type taskOutcome struct {
	tr     *taskRun
	done   bool
	cause  error
	onFail onFailResult
}

// runParallel is Run's loop with --parallel > 1 (see the top of this
// file). It is called once the first sync, the graph validation and
// run-state.json are done, and returns like the serial loop: the first
// fatal error, an exceeded run budget, a sync or graph error, the
// consecutive-failures abort, ErrInterrupted after a stop, or nil once
// nothing is pending.
func (o *Orchestrator) runParallel(ctx context.Context, planner, worker, tester *agents.Agent, uw *usage.Writer) error {
	n := o.parallel()
	jobs := make(chan ownerJob)
	finished := make(chan taskOutcome, n)
	inflight := map[string]*taskRun{}
	// notes is the last skip/defer reason logged per task, so a reason is
	// logged once, not on every pass.
	notes := map[string]string{}
	var fatalErr, budgetErr, loopErr, abortErr error
	failures := 0
	stopLogged := false
	var deferred []string
	// startFailed is set when a task failed to start in the last pass, so
	// the loop goes on (until the failures abort it) even with nothing in
	// flight, like the serial loop.
	startFailed := false

	o.Log.Printf("parallel: up to %d tasks at a time; merges stay serial, in the order tasks finish", n)

	run := func(tr *taskRun) {
		go func() { finished <- tr.view.runTask(ctx, tr, planner, worker, tester, uw) }()
	}
	// result classifies a task's final error: fatal stops every task,
	// any other counts towards the consecutive-failures abort, nil resets
	// the count.
	result := func(v *Orchestrator, spec string, err error) {
		if err == nil {
			failures = 0
			return
		}
		v.Log.Printf("task %s: error: %v", spec, err)
		// The shared run budget ran out: no new task starts, but the
		// tasks in flight end their current iteration (see
		// runIterations) rather than being stopped at their next call.
		if errors.Is(err, ErrRunBudgetExceeded) {
			if budgetErr == nil {
				var fatal *fatalError
				budgetErr = err
				if errors.As(err, &fatal) {
					budgetErr = fatal.err
				}
				o.Log.Printf("run budget exceeded: no new task starts; waiting for %d in-flight task(s) to end their iteration", len(inflight))
			}
			return
		}
		var fatal *fatalError
		if errors.As(err, &fatal) {
			if fatalErr == nil {
				fatalErr = fatal.err
				o.requestInternalStop()
			}
			return
		}
		failures++
		if failures >= maxConsecutiveFailures && abortErr == nil {
			abortErr = fmt.Errorf("aborting after %d tasks failed in a row (last: %s): %w", failures, spec, err)
			o.Log.Printf("%v; no new task starts, waiting for %d in-flight task(s)", abortErr, len(inflight))
		}
	}
	note := func(spec, text string, emit func()) {
		if notes[spec] != text {
			notes[spec] = text
			emit()
		}
	}
	// fill syncs the queue and starts every ready task there is a slot
	// for, walking QUEUE.md in order with the serial loop's checks. A task
	// is held back while a predecessor in the dependency graph is in
	// flight or held back itself; deferred lists the held-back tasks.
	fill := func() {
		deferred, startFailed = nil, false
		if err := o.syncQueueLocked(); err != nil {
			loopErr = fmt.Errorf("sync spec queue: %w", err)
			return
		}
		if err := specs.ValidateGraph(o.Layout); err != nil {
			o.Log.Printf("%v", err)
			loopErr = err
			return
		}
		queue, err := specs.ReadQueue(o.Layout.QueuePath)
		if err != nil {
			loopErr = fmt.Errorf("read queue: %w", err)
			return
		}
		preds, err := specs.PendingPredecessors(o.Layout, slices.Sorted(maps.Keys(inflight)))
		if err != nil {
			loopErr = fmt.Errorf("read the dependency graph: %w", err)
			return
		}
		blocking := map[string]bool{}
		for s := range inflight {
			blocking[s] = true
		}
		for _, next := range queue {
			if inflight[next] != nil {
				continue
			}
			if status, err := o.getStatus(next); err == nil &&
				(status == specs.StatusDone || status == specs.StatusFailed || status == specs.StatusBlocked || status == specs.StatusWaiting) {
				note(next, "status:"+status, func() { o.Log.Printf("skipping %s: it is %s", next, status) })
				continue
			}
			if dep, err := specs.WaitingDependency(o.Layout, next); err == nil && dep != "" {
				note(next, "waiting:"+dep, func() {
					o.Log.Printf("skipping %s: it depends on %s, which is %s", next, dep, specs.StatusWaiting)
					o.event(next, 0, "", "skipped", runlog.F("reason", "waiting_dependency"), runlog.F("waiting_on", dep))
				})
				continue
			}
			chk, err := specs.CheckDependencies(o.Layout, next)
			if err != nil {
				loopErr = fmt.Errorf("check dependencies of %s: %w", next, err)
				return
			}
			if chk.Blocker.Spec != "" {
				rows, err := o.blockForDependency(next, chk.Blocker)
				if err != nil {
					o.Log.Printf("task %s: couldn't block it or the tasks that depend on it: %v", next, err)
				}
				o.reportDependencyBlocks(rows, chk.Blocker.Spec)
				continue
			}
			if len(chk.Unfinished) > 0 {
				waitsFor := strings.Join(chk.Unfinished, ", ")
				note(next, "unfinished:"+waitsFor, func() {
					o.Log.Printf("deferring %s: it depends on %s, not DONE yet", next, waitsFor)
					o.event(next, 0, "", "skipped", runlog.F("reason", "pending_dependency"), runlog.F("waiting_on", waitsFor))
				})
				blocking[next] = true
				deferred = append(deferred, next+" waits for "+waitsFor)
				continue
			}
			var before []string
			for _, p := range preds[next] {
				if blocking[p] {
					before = append(before, p)
				}
			}
			if len(before) > 0 {
				waitsFor := strings.Join(before, ", ")
				note(next, "before:"+waitsFor, func() {
					o.Log.Printf("deferring %s: it comes after %s, not merged yet", next, waitsFor)
					o.event(next, 0, "", "skipped", runlog.F("reason", "pending_predecessor"), runlog.F("waiting_on", waitsFor))
				})
				blocking[next] = true
				deferred = append(deferred, next+" waits for "+waitsFor)
				continue
			}
			blocking[next] = true
			if len(inflight) >= o.effectiveParallel() {
				continue
			}
			// --max-cost-per-run crossed by a call so far: start nothing
			// more. The task stays QUEUED for the next run.
			if err := o.runBudgetExceeded(uw); err != nil {
				o.Log.Printf("run stopped before task %s: %v", next, err)
				budgetErr = err
				return
			}
			delete(notes, next)
			v := o.taskView(next)
			v.owner = jobs
			tr, err := v.startTask(ctx, next)
			if err != nil {
				startFailed = true
				result(v, next, err)
				if fatalErr != nil {
					return
				}
				continue
			}
			if err := o.removeFromQueue(next); err != nil {
				o.Log.Printf("warning: couldn't remove %s from %s: %v", next, filepath.Base(o.Layout.QueuePath), err)
			}
			inflight[next] = tr
			run(tr)
		}
	}

	for {
		stopping := o.stopRequested()
		if stopping && !stopLogged {
			stopLogged = true
			if len(inflight) > 0 {
				o.Log.Printf("stop requested: no new task starts; waiting for %d in-flight task(s)", len(inflight))
			}
		}
		canLaunch := !stopping && fatalErr == nil && budgetErr == nil && loopErr == nil && abortErr == nil
		startFailed = false
		if canLaunch && len(inflight) < o.effectiveParallel() {
			fill()
		}
		if len(inflight) == 0 {
			if startFailed && fatalErr == nil && abortErr == nil {
				continue
			}
			break
		}
		select {
		case out := <-finished:
			tr, v := out.tr, out.tr.view
			reexec, err := v.finishTask(ctx, out, uw)
			if reexec {
				if err := v.reexecTask(ctx, tr); err != nil {
					delete(inflight, tr.specFile)
					result(v, tr.specFile, &fatalError{err})
				} else {
					run(tr)
				}
				continue
			}
			delete(inflight, tr.specFile)
			result(v, tr.specFile, err)
			if o.onFinished != nil {
				o.onFinished(tr.specFile)
			}
		case job := <-jobs:
			job.fn()
			close(job.done)
		case <-o.shared().gate.wake:
			// The limit gate opened again after a pause: fill the slots
			// it freed.
		}
	}

	o.saveState()
	stopping := o.stopRequested()
	if stopping && fatalErr == nil && !stopLogged {
		o.Log.Printf("stop requested: exiting before the next task")
	}
	o.printUsageSummary(uw)
	switch {
	case fatalErr != nil:
		return fatalErr
	case budgetErr != nil:
		return budgetErr
	case loopErr != nil:
		return loopErr
	case abortErr != nil:
		return abortErr
	case stopping:
		return ErrInterrupted
	case len(deferred) > 0:
		// Every task left waits for one that will never become DONE in
		// this run: validation ruled cycles out, so STATUS.md was probably
		// edited by hand.
		return fmt.Errorf("no pending task is ready and none is in flight (a hand-edited STATUS.md?): %s", strings.Join(deferred, "; "))
	}
	return nil
}

// conflictAttemptNote heads the history a re-executed task's planner and
// worker get in place of the previous review (see reexecTask).
const conflictAttemptNote = "This task is being re-executed (attempt 2 of 2). Its first attempt was DONE, but merging its branch " +
	"conflicted with changes merged into %s meanwhile. The first attempt's changes are kept on branch `%s`; " +
	"this attempt starts from the new HEAD %s, so redo the task on top of the current code. " +
	"The first attempt's plan and reviews are archived as %s; they follow as history.\n"

// reexecTask sets a task whose merge conflicted up for its second and last
// attempt, on the merge owner: the first attempt's branch is kept as
// diadoche/conflict/<NNN>-<slug> (its worktree removed, see
// retireWorktree), its plan and reviews are archived as
// NNN_ATTEMPT1_IMPL_PLAN.md / NNN_ATTEMPT1_REVIEW_<M>.md, and a fresh
// worktree is made from the current HEAD. tr then runs again from
// iteration 1, with the first attempt's history for planner and worker.
// An error means the task couldn't be set up again: fatal for the run.
func (o *Orchestrator) reexecTask(ctx context.Context, tr *taskRun) error {
	spec, num, ws := tr.specFile, tr.num, tr.ws
	oldBase := ws.base
	target := gitutil.CurrentBranch(ctx, o.Cfg.Dir)
	branch, _, _, err := o.retireWorktree(ctx, spec, num, ws, conflictBranchPrefix, "diadoche: first attempt of "+spec+" (merge conflict)")
	if err != nil {
		return fmt.Errorf("task %s: keep its first attempt: %w", spec, err)
	}
	ws.removeBaseFile()
	archived, err := o.archiveAttempt(num, 1)
	if err != nil {
		return fmt.Errorf("task %s: archive its first attempt: %w", spec, err)
	}
	newWS, err := o.prepareWorktree(ctx, spec, num)
	if err != nil {
		return fmt.Errorf("task %s: set up its second attempt: %w", spec, err)
	}
	newWS.attempt, newWS.conflictBranch = 2, branch

	if err := o.setStatus(spec, specs.StatusInProgress); err != nil {
		o.Log.Printf("task %s: couldn't set it back to %s: %v", spec, specs.StatusInProgress, err)
	} else {
		o.event(spec, 0, "", "status", runlog.F("status", specs.StatusInProgress), runlog.F("reason", "reexec"))
	}
	o.event(spec, 0, "", "reexec", runlog.F("reason", "merge_conflict"), runlog.F("branch", branch),
		runlog.F("old_base", oldBase), runlog.F("base", newWS.base), runlog.F("attempt", 2))
	o.Log.Printf("task %s: re-executing from %s (attempt 2/2); first attempt kept on %s", spec, newWS.base, branch)

	paths := make([]string, len(archived))
	for i, f := range archived {
		paths[i] = o.relPath(filepath.Join(o.Layout.SpecDir, f))
	}
	files := "(none)"
	if len(paths) > 0 {
		files = strings.Join(paths, ", ")
	}
	var b strings.Builder
	fmt.Fprintf(&b, conflictAttemptNote, target, branch, newWS.base, files)
	for _, f := range archived {
		content, err := os.ReadFile(filepath.Join(o.Layout.SpecDir, f))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n\n%s\n", f, strings.TrimRight(string(content), "\n"))
	}
	tr.priorAttempt = b.String()
	tr.ws, tr.attempt, tr.resume = newWS, 2, nil
	return nil
}

// archiveAttempt renames task num's plan and reviews to
// <num>_ATTEMPT<k>_IMPL_PLAN.md and <num>_ATTEMPT<k>_REVIEW_<M>.md, so the
// next attempt starts at iteration 1. archived are the new names: the
// plan, then the reviews by M.
func (o *Orchestrator) archiveAttempt(num string, k int) (archived []string, err error) {
	prefix := fmt.Sprintf("%s_ATTEMPT%d_", num, k)
	plan := num + "_IMPL_PLAN.md"
	if fileExists(filepath.Join(o.Layout.SpecDir, plan)) {
		if err := os.Rename(filepath.Join(o.Layout.SpecDir, plan), filepath.Join(o.Layout.SpecDir, prefix+"IMPL_PLAN.md")); err != nil {
			return nil, err
		}
		archived = append(archived, prefix+"IMPL_PLAN.md")
	}
	for _, r := range taskReviews(o.Layout.SpecDir, num) {
		m := reviewRe.FindStringSubmatch(r.Name)
		n, _ := strconv.Atoi(m[1])
		name := fmt.Sprintf("%sREVIEW_%d.md", prefix, n)
		if err := os.Rename(filepath.Join(o.Layout.SpecDir, r.Name), filepath.Join(o.Layout.SpecDir, name)); err != nil {
			return archived, err
		}
		archived = append(archived, name)
	}
	return archived, nil
}
