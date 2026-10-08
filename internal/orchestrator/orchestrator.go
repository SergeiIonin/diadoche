// Package orchestrator runs diadoche's main loop: sync the spec queue,
// then for each queued task run planner -> worker -> tester for up to
// cfg.Iter iterations, until the tester marks the task DONE in STATUS.md or
// the iteration budget is exhausted. An exhausted task gets the --on-fail
// policy chain (see onfail.go: an escalated iteration on a stronger model,
// then BLOCKED by default; optionally split into smaller tasks by the
// preprocessor, see split.go); it is FAILED only with an empty chain, a
// chain that runs out, or another cause (an agent error, a cost cap). Between
// worker and tester, the red-green check (see redgreen.go) makes sure
// worker's tests fail on the task's base commit, i.e. actually test the
// new code.
//
// With cfg.Worktree each task runs in its own git worktree and branch
// (see worktree.go), merged into the current branch once DONE; without it
// tasks run in the main working tree and FAILED changes are parked (see
// park.go).
//
// An agent call refused by a claude usage or rate limit pauses the task
// (PAUSED in STATUS.md) and is retried once the limit lifts, without
// spending an iteration; --max-cost-per-task and --max-cost-per-run are
// checked after every agent call (see pause.go).
//
// After every step the run's position — task, iteration, agent and whether
// that agent is pending, running or done, plus the task's base commit and
// worktree — is saved to .claude/diadoche/run-state.json (see runstate.go).
// A restarted run continues with the first unfinished agent of that
// iteration instead of starting it over; the spec/ files (plan, reviews,
// NNN_BASE) remain the fallback when the file is missing or contradicts
// STATUS.md/QUEUE.md. A signal (Orchestrator.StopRequested) lets the agent
// call in flight finish, then Run saves the state and returns
// ErrInterrupted; the task stays pending and is never failed for it.
//
// Every run also gets .claude/diadoche/runs/<run-id>/ (see artifacts.go and
// package runlog): run.log, one line per event (run/task start and end,
// agent calls, tester verdicts, pauses, status changes, merges, parking),
// and per task a <NNN>-<slug>/ directory with each iteration's plan and
// review, every agent call's raw stdout/stderr and the task's final.diff,
// plus, for a DONE task, decisions.json — the decisions worker reported in
// the DONE iteration (see package decisions).
// Writing them is best effort: a failure is a warning, never the end of
// the run.
//
// After a DONE task is merged (or committed in place), the --on-task-done
// hooks run (see hooks.go): diadoche's one extension point for the moment a
// task is genuinely finished. With --adr the built-in ADR step runs first
// at the same point (see adr.go), turning the task's decisions into
// docs/adr/ records through the adr-writer agent. A failing hook or ADR
// step is a warning, never a task or run failure.
//
// A planner or worker that can't decide an open question alone writes it
// to QUESTIONS.md and, when the decision isn't reversible, ends its answer
// with a NEEDS_HUMAN line: the task becomes WAITING without spending an
// iteration and the run moves on; only --resume-waiting re-queues it (see
// waiting.go).
//
// --dry-run never changes state: it walks the queue read-only and prints
// the prompts each agent would get, and the hooks it would run (see
// dryrun.go).
//
// With --parallel N > 1 (see parallel.go) up to N tasks ready by the
// dependency graph run at once, each in its own worktree and goroutine;
// Run's goroutine is the merge owner, which starts the tasks and merges
// the finished ones one at a time, in the order they finished, along with
// every other main-tree, main-branch and spec-directory step. A merge
// conflict re-executes the task once from the new HEAD; a second one makes
// it BLOCKED. Each task logs through its own view of the Orchestrator,
// whose lines carry the task's id.
package orchestrator

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/implplan"
	"diadoche/internal/onfail"
	"diadoche/internal/questions"
	"diadoche/internal/runlog"
	"diadoche/internal/runstate"
	"diadoche/internal/skills"
	"diadoche/internal/speclock"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
	"diadoche/internal/usagelimit"
)

// lockTimeout bounds how long the main loop waits for the spec directory
// lock (e.g. held by a concurrent --preprocess run) before giving up.
const lockTimeout = 2 * time.Minute

// maxConsecutiveFailures bounds how many tasks in a row may fail before Run
// gives up entirely, instead of burning through the rest of the queue at
// full speed (e.g. every task failing identically because the underlying
// `claude` CLI is rate-limited or misconfigured).
const maxConsecutiveFailures = 3

// defaultHeartbeatInterval is how often a still-running agent turn is
// reported in the log when Orchestrator.HeartbeatInterval is unset.
const defaultHeartbeatInterval = time.Minute

type Orchestrator struct {
	Cfg    config.Config
	Layout specs.Layout
	Runner agents.Runner
	Log    *log.Logger

	// HeartbeatInterval is how often runAgent logs that an agent is still
	// running; zero means defaultHeartbeatInterval.
	HeartbeatInterval time.Duration

	// Now and Sleep are the clock and the wait of a usage-limit pause,
	// overridable for tests; nil means the real clock and an interruptible
	// wall-clock sleep.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	// priorCosts is the spend usage.jsonl held when the run started, for
	// --max-cost-per-task. Set once by Run before any task goroutine
	// starts and read-only afterwards, so the views share it without a
	// lock.
	priorCosts usage.Costs

	// StopRequested, once closed, asks the run to stop at the next point
	// between agent calls (the first SIGINT/SIGTERM, see package shutdown).
	// The agent call in flight runs on until Run's ctx is cancelled. nil
	// means never.
	StopRequested <-chan struct{}

	// runID, statePath and stateOn are this run's run-state.json; stateOn
	// is false in --dry-run, where nothing is saved.
	runID     string
	statePath string
	stateOn   bool

	// cur is the current task's record, one of sh.active; each task view
	// has its own (see taskView).
	cur *runstate.Task

	// sh is the state shared by every view of the run: the active and
	// saved task records, the WAITING records, the spec-file mutex and
	// the internal stop (see parallel.go).
	sh *runShared

	// owner is where a task view of a parallel run sends the work only the
	// merge owner may do (see onOwner); nil in the serial loop.
	owner chan<- ownerJob

	// onMerge and onFinished are for tests only: called right before and
	// after a DONE task's merge (or commit), and when a parallel run's
	// owner is done with a task.
	onMerge    func(specFile string, enter bool)
	onFinished func(specFile string)

	// run is this run's directory under .claude/diadoche/runs, holding
	// run.log and the tasks' artifacts; nil in --dry-run or when it
	// couldn't be created (every runlog method is a no-op on nil).
	run *runlog.Run

	// decisions maps a spec file to the absolute path of the decisions.json
	// this run wrote for it. The file lives under runs/, outside any
	// worktree, so it outlives the task's merge; later steps such as the
	// ADR step read it (see DecisionsPath). Guarded by sh.mu; Run creates
	// it before any task view, so all views share it.
	decisions map[string]string

	// escPlanner and escWorker are planner and worker on the escalation
	// model, for the escalated iteration of --on-fail escalate; built by
	// Run when the chain escalates.
	escPlanner, escWorker *agents.Agent

	// splitter is the preprocessor agent of --on-fail split; built by Run
	// when the chain contains split.
	splitter *agents.Agent

	// adrWriter is the adr-writer agent of the ADR step, loaded by
	// loadAgents when --adr is on or its model is overridden; nil otherwise.
	adrWriter *agents.Agent

	// simulate is for tests only: run the whole loop like a real run but
	// without git, usage.jsonl, run-state.json or run artifacts — what
	// --dry-run used to do. Real --dry-run is the read-only walk in
	// dryrun.go.
	simulate bool
}

// offline reports whether the loop runs without git, usage.jsonl,
// run-state.json and run artifacts: in --dry-run, or simulated by a test.
func (o *Orchestrator) offline() bool {
	return o.Cfg.DryRun || o.simulate
}

func New(cfg config.Config) *Orchestrator {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	var runner agents.Runner
	if cfg.DryRun {
		runner = agents.DryRunRunner{Log: logger}
	} else {
		runner = agents.CLIRunner{
			Bin:                                cfg.ClaudeBin,
			MaxCallDuration:                    time.Duration(cfg.MaxCallMinutes) * time.Minute,
			ExcludeDynamicSystemPromptSections: cfg.StableSystemPrompt,
		}
	}
	return &Orchestrator{
		Cfg:    cfg,
		Layout: specs.NewLayout(cfg.Dir),
		Runner: runner,
		Log:    logger,
	}
}

func (o *Orchestrator) Run(ctx context.Context) (err error) {
	o.shared()
	if o.decisions == nil {
		o.decisions = map[string]string{}
	}
	if o.Cfg.DryRun {
		return o.dryRun(ctx)
	}
	if err := o.Layout.Ensure(); err != nil {
		return fmt.Errorf("ensure layout: %w", err)
	}

	if !o.offline() {
		if err := gitutil.EnsureRepo(ctx, o.Cfg.Dir); err != nil {
			return fmt.Errorf("ensure git repo: %w", err)
		}
		if o.Cfg.Worktree {
			if err := o.prepareWorktreeRun(ctx); err != nil {
				return err
			}
		}
	}

	o.openRun(ctx)
	defer o.run.Close()
	defer func() {
		result := "ok"
		switch {
		case errors.Is(err, ErrInterrupted) || errors.Is(err, context.Canceled):
			result = "interrupted"
		case err != nil:
			result = "error"
		}
		o.event("", 0, "", "run_end", runlog.F("result", result))
	}()

	planner, worker, tester, err := o.loadAgents()
	if err != nil {
		return err
	}
	if slices.Contains(o.Cfg.OnFail, onfail.Escalate) {
		if err := o.loadEscalationAgents(planner, worker); err != nil {
			return err
		}
	}
	if slices.Contains(o.Cfg.OnFail, onfail.Split) {
		if err := o.loadSplitAgent(planner.Prefix); err != nil {
			return err
		}
	}
	if len(o.Cfg.OnFail) > 0 {
		esc := ""
		if o.escPlanner != nil {
			esc = "; escalation model=" + o.escPlanner.ModelDescription()
		}
		if o.splitter != nil {
			esc += "; split preprocessor model=" + o.splitter.ModelDescription()
		}
		o.Log.Printf("on-fail: %s%s", strings.Join(o.Cfg.OnFail, ","), esc)
	}

	// Usage accounting writes real claude-CLI JSON output to disk; skip it
	// entirely in --dry-run, where there's no real usage to record and no
	// guarantee the project even has a .claude/diadoche directory worth
	// writing into yet.
	var uw *usage.Writer
	if !o.offline() {
		usagePath := filepath.Join(o.Layout.Root, "usage.jsonl")
		costs, err := usage.LoadCosts(usagePath)
		if err != nil {
			o.Log.Printf("warning: could not read earlier costs from %s (task budgets count this run only): %v", usagePath, err)
		}
		o.priorCosts = costs
		w, err := usage.Open(usagePath)
		if err != nil {
			return fmt.Errorf("open usage log: %w", err)
		}
		defer w.Close()
		uw = w
	}

	if o.Cfg.ResumeWaiting {
		if err := o.resumeWaitingTasks(); err != nil {
			return err
		}
	}

	stateInitialized := false
	for {
		if err := o.syncQueueLocked(); err != nil {
			return fmt.Errorf("sync spec queue: %w", err)
		}
		// A dangling Depends-on reference or a dependency cycle stops the
		// run before any agent is called for the tasks involved.
		if err := specs.ValidateGraph(o.Layout); err != nil {
			o.Log.Printf("%v", err)
			o.printUsageSummary(uw)
			return err
		}
		if !stateInitialized {
			stateInitialized = true
			queue, err := specs.ReadQueue(o.Layout.QueuePath)
			if err != nil {
				return fmt.Errorf("read queue: %w", err)
			}
			o.initRunState(ctx, queue)
			runDir := ""
			if o.run != nil {
				runDir = o.run.ID
			}
			o.event("", 0, "", "run_start", runlog.F("run", runDir), runlog.F("state_run", o.runID))
			if o.parallel() > 1 {
				return o.runParallel(ctx, planner, worker, tester, uw)
			}
		}

		processedAny := false
		var deferred []string // "<spec> waits for <deps>", this round
		consecutiveFailures := 0
		for {
			if o.stopRequested() {
				o.saveState()
				o.Log.Printf("stop requested: exiting before the next task")
				o.printUsageSummary(uw)
				return ErrInterrupted
			}
			next, err := o.popQueue()
			if err != nil {
				return fmt.Errorf("pop queue: %w", err)
			}
			if next == "" {
				break
			}
			// Before a popped task starts: a task that stopped being
			// pending after QUEUE.md was built is skipped; then one
			// depending on a WAITING task; then one whose declared
			// dependencies aren't all DONE (BLOCKED if one is FAILED or
			// BLOCKED, deferred if one is still pending); then the run
			// budget is checked.
			//
			// A task that stopped being pending after QUEUE.md was built
			// (e.g. BLOCKED because a task it depends on was) is skipped.
			if status, err := o.getStatus(next); err == nil &&
				(status == specs.StatusDone || status == specs.StatusFailed || status == specs.StatusBlocked || status == specs.StatusWaiting) {
				o.Log.Printf("skipping %s: it is %s", next, status)
				continue
			}
			// A task depending on a WAITING one waits too: it stays QUEUED
			// and runs once the WAITING task is resumed.
			if dep, err := specs.WaitingDependency(o.Layout, next); err == nil && dep != "" {
				o.Log.Printf("skipping %s: it depends on %s, which is %s", next, dep, specs.StatusWaiting)
				o.event(next, 0, "", "skipped", runlog.F("reason", "waiting_dependency"), runlog.F("waiting_on", dep))
				continue
			}
			// Only explicitly declared dependencies are checked here: the
			// implicit "previous task" one only orders the queue.
			chk, err := specs.CheckDependencies(o.Layout, next)
			if err != nil {
				return fmt.Errorf("check dependencies of %s: %w", next, err)
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
				o.Log.Printf("deferring %s: it depends on %s, not DONE yet", next, waitsFor)
				o.event(next, 0, "", "skipped", runlog.F("reason", "pending_dependency"), runlog.F("waiting_on", waitsFor))
				deferred = append(deferred, next+" waits for "+waitsFor)
				continue
			}
			// --max-cost-per-run crossed by the previous task's last call:
			// stop before starting another. The popped task stays QUEUED in
			// STATUS.md, so the next run's sync puts it back in the queue.
			if err := o.runBudgetExceeded(uw); err != nil {
				o.Log.Printf("run stopped before task %s: %v", next, err)
				o.saveState()
				o.printUsageSummary(uw)
				return err
			}
			processedAny = true
			if err := o.processTask(ctx, next, planner, worker, tester, uw); err != nil {
				o.Log.Printf("task %s: error: %v", next, err)
				var fatal *fatalError
				if errors.As(err, &fatal) {
					o.printUsageSummary(uw)
					return fatal.err
				}
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					o.printUsageSummary(uw)
					return fmt.Errorf("aborting after %d tasks failed in a row (last: %s): %w", consecutiveFailures, next, err)
				}
				continue
			}
			consecutiveFailures = 0
		}

		if !processedAny && len(deferred) > 0 {
			// Every task left waits for one that will never become DONE in
			// this run: validation ruled cycles out, so STATUS.md was
			// probably edited by hand.
			o.saveState()
			o.printUsageSummary(uw)
			return fmt.Errorf("no pending task is ready and none is in flight (a hand-edited STATUS.md?): %s", strings.Join(deferred, "; "))
		}
		if !processedAny {
			// Nothing left to do and nothing new showed up in this round's
			// sync: no leftover work, no new task specs. We're done.
			// run-state.json stays, with no task: nothing to continue.
			o.saveState()
			o.printUsageSummary(uw)
			return nil
		}
		o.Log.Printf("queue drained, re-checking %s for new task specs...", o.Layout.SpecDir)
	}
}

// loadAgents resolves the skills in scope, loads planner, worker and tester
// with their models, and gives all three the run's shared prompt prefix.
// When the ADR step needs it, adr-writer is loaded the same way into
// o.adrWriter.
func (o *Orchestrator) loadAgents() (planner, worker, tester *agents.Agent, err error) {
	matchedSkills, err := skills.Resolve(o.Layout.SkillsDir, o.Cfg.Langs, o.Cfg.Frameworks)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve skills: %w", err)
	}
	if len(matchedSkills) > 0 {
		o.Log.Printf("skills in scope: %s", strings.Join(matchedSkills, ", "))
	}

	planner, err = agents.LoadOrDefault(o.Layout.AgentsDir, "planner")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load planner agent: %w", err)
	}
	if err := agents.ResolveModel(planner, o.Cfg.PlannerModel); err != nil {
		return nil, nil, nil, err
	}
	worker, err = agents.LoadOrDefault(o.Layout.AgentsDir, "worker")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load worker agent: %w", err)
	}
	if err := agents.ResolveModel(worker, o.Cfg.WorkerModel); err != nil {
		return nil, nil, nil, err
	}
	tester, err = agents.LoadOrDefault(o.Layout.AgentsDir, "tester")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load tester agent: %w", err)
	}
	if err := agents.ResolveModel(tester, o.Cfg.TesterModel); err != nil {
		return nil, nil, nil, err
	}
	// adr-writer is resolved whenever its model is set, even without
	// --adr, so a bad --adr-model/DIADOCHE_ADR_MODEL fails at startup.
	o.adrWriter = nil
	if o.Cfg.ADR || o.Cfg.ADRModel.IsSet() {
		adrWriter, err := agents.LoadOrDefault(o.Layout.AgentsDir, "adr-writer")
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load adr-writer agent: %w", err)
		}
		if err := agents.ResolveModel(adrWriter, o.Cfg.ADRModel); err != nil {
			return nil, nil, nil, err
		}
		o.adrWriter = adrWriter
	}
	models := fmt.Sprintf("models: planner=%s worker=%s tester=%s", planner.ModelDescription(), worker.ModelDescription(), tester.ModelDescription())
	if o.Cfg.ADR {
		models += " adr-writer=" + o.adrWriter.ModelDescription()
	}
	o.Log.Print(models)

	// One prefix for the whole run, resolved once and shared byte for byte
	// by all three agents: that identity is what lets the API's prompt
	// cache serve the common context to the next agent instead of charging
	// for it again (see docs/prompt-caching.md).
	basePrefix, prefixSource, err := agents.LoadPrefixOrDefault(o.Layout.Root)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load prompt prefix: %w", err)
	}
	prefix := agents.BuildPrefix(basePrefix, matchedSkills)
	planner.Prefix, worker.Prefix, tester.Prefix = prefix, prefix, prefix
	if o.adrWriter != nil {
		o.adrWriter.Prefix = prefix
	}
	o.Log.Printf("prompt prefix: %d bytes (source: %s)", len(prefix), prefixSource)
	return planner, worker, tester, nil
}

func (o *Orchestrator) printUsageSummary(uw *usage.Writer) {
	if uw == nil || uw.Summary.Calls() == 0 {
		return
	}
	o.Log.Print(uw.Summary.Format())
}

// syncQueueLocked takes the spec directory lock and refreshes STATUS.md /
// QUEUE.md, so it never races with a concurrent --preprocess run writing new
// spec files into the same directory.
func (o *Orchestrator) syncQueueLocked() error {
	lock, err := speclock.Acquire(o.Layout.SpecDir, lockTimeout)
	if err != nil {
		return fmt.Errorf("acquire spec lock: %w", err)
	}
	defer lock.Release()
	return o.syncSpecs()
}

// processTask runs one task start to finish: startTask, runTask, then
// finishTask, back to back (the serial loop).
func (o *Orchestrator) processTask(ctx context.Context, specFile string, planner, worker, tester *agents.Agent, uw *usage.Writer) error {
	tr, err := o.startTask(ctx, specFile)
	if err != nil {
		return err
	}
	_, err = o.finishTask(ctx, o.runTask(ctx, tr, planner, worker, tester, uw), uw)
	return err
}

// startTask sets specFile up to run, on the merge owner: its saved record
// (if any), its workspace or worktree, IN_PROGRESS in STATUS.md. An error
// setting the workspace up is fatal.
func (o *Orchestrator) startTask(ctx context.Context, specFile string) (*taskRun, error) {
	o.Log.Printf("=== task %s ===", specFile)
	num := specs.TaskNumber(specFile)
	resume := o.takeResume(specFile)
	tr := &taskRun{specFile: specFile, num: num, view: o, resume: resume, attempt: 1}
	if resume != nil && resume.Attempt > 1 {
		tr.attempt = resume.Attempt
	}

	var ws *workspace
	if !o.offline() {
		if resume != nil {
			o.seedBaseFromResume(num, resume)
		}
		var err error
		if o.Cfg.Worktree {
			ws, err = o.prepareWorktree(ctx, specFile, num)
		} else {
			ws, err = o.prepareWorkspace(ctx, specFile, num)
		}
		if err != nil {
			return nil, &fatalError{err}
		}
		ws.attempt = tr.attempt
		if tr.attempt > 1 {
			if b := conflictBranchPrefix + taskID(specFile, num); gitutil.BranchExists(ctx, o.Cfg.Dir, b) {
				ws.conflictBranch = b
			}
		}
		if resume != nil {
			o.checkResumedWorkspace(specFile, ws, resume)
		}
		o.restoreWaiting(ctx, specFile, num, ws)
	}
	if ws != nil {
		o.event(specFile, 0, "", "task_start", runlog.F("base", ws.base))
	} else {
		o.event(specFile, 0, "", "task_start")
	}

	if err := o.setStatus(specFile, specs.StatusInProgress); err != nil {
		return nil, err
	}
	o.event(specFile, 0, "", "status", runlog.F("status", specs.StatusInProgress))
	tr.ws = ws
	return tr, nil
}

// runTask runs a started task's iterations and, when they ran out, its
// --on-fail chain, then writes its final.diff: only agent calls and the
// task's own worktree, so in a parallel run it runs on the task's own
// goroutine.
func (o *Orchestrator) runTask(ctx context.Context, tr *taskRun, planner, worker, tester *agents.Agent, uw *usage.Writer) taskOutcome {
	specFile, num, ws := tr.specFile, tr.num, tr.ws
	done, cause := o.runIterations(ctx, specFile, num, ws, tr.resume, tr.priorAttempt, planner, worker, tester, uw)
	// Iterations exhausted: the --on-fail chain decides (see onfail.go).
	// Every outcome of its escalated iteration is then handled below like
	// the same outcome of a normal one.
	var onFail onFailResult
	if !done && cause == nil && o.parallel() > 1 {
		// The run budget ran out meanwhile: the task stays pending, its
		// --on-fail chain would only spend more.
		cause = o.runBudgetExceeded(uw)
	}
	if !done && cause == nil && len(o.Cfg.OnFail) > 0 && !o.Cfg.DryRun {
		onFail = o.applyOnFail(ctx, onFailInput{specFile: specFile, num: num, ws: ws, planner: planner, worker: worker, tester: tester, uw: uw})
		done, cause = onFail.done, onFail.cause
	}
	o.writeFinalDiff(ctx, specFile, num, ws)
	return taskOutcome{tr: tr, done: done, cause: cause, onFail: onFail}
}

// finishTask carries out a task's outcome, on the merge owner: merge or
// commit and the task-done handlers for DONE, WAITING, split, BLOCKED or
// FAILED with their changes preserved, or the task left pending for the
// next run. The error is the task's, as for the serial loop (a
// *fatalError stops the run). reexec reports a merge conflict the task is
// re-executed for (--parallel): its record then stays active.
func (o *Orchestrator) finishTask(ctx context.Context, out taskOutcome, uw *usage.Writer) (reexec bool, err error) {
	specFile, num, ws := out.tr.specFile, out.tr.num, out.tr.ws
	done, cause, onFail := out.done, out.cause, out.onFail
	taskEnd := func(status, reason string) {
		fields := []runlog.Field{runlog.F("status", status)}
		if reason != "" {
			fields = append(fields, runlog.F("reason", reason))
		}
		o.event(specFile, 0, "", "task_end", fields...)
	}

	// The resumable outcomes below keep the task's run-state.json record
	// (the next run continues from it); every other one closes it.
	var interrupted *pauseInterruptedError
	if errors.As(cause, &interrupted) {
		o.saveState()
		o.Log.Printf("task %s: interrupted while paused; it stays PAUSED and is resumed by the next run", specFile)
		taskEnd(specs.StatusPaused, "interrupted")
		return false, &fatalError{cause}
	}
	if !done && (errors.Is(cause, ErrInterrupted) || ctx.Err() != nil) {
		// A signal is never a reason to fail a task: nothing is parked.
		o.saveState()
		o.Log.Printf("task %s: interrupted by signal; it stays IN_PROGRESS and the next run resumes it %s", specFile, o.resumePoint())
		taskEnd(specs.StatusInProgress, "interrupted")
		if cause == nil || errors.Is(cause, ErrInterrupted) {
			return false, &fatalError{cmp.Or(cause, ErrInterrupted)}
		}
		return false, &fatalError{fmt.Errorf("%w (%v)", ErrInterrupted, cause)}
	}
	var nh *needsHumanError
	if errors.As(cause, &nh) {
		// Not a failure and no iteration spent: the task waits for a
		// human's answer in QUESTIONS.md, and the run moves on.
		err := o.waitTask(ctx, specFile, num, ws, nh)
		taskEnd(specs.StatusWaiting, "needs_human")
		if err != nil {
			return false, &fatalError{fmt.Errorf("task %s: preserve WAITING changes: %w", specFile, err)}
		}
		return false, nil
	}
	if errors.Is(cause, ErrRunBudgetExceeded) {
		o.saveState()
		o.Log.Printf("task %s: run budget exhausted; leaving it pending with its changes in place", specFile)
		taskEnd(specs.StatusInProgress, "run_budget")
		return false, &fatalError{cause}
	}
	if cause != nil && usagelimit.Mentions(cause.Error()) {
		// Not a failure: the account's usage window ran out. Nothing is
		// parked and the task stays IN_PROGRESS, so the next run (e.g. by
		// diadoche-restarter once the window reopens) resumes it with its
		// changes still in the working tree (or its worktree).
		o.saveState()
		o.Log.Printf("task %s: interrupted by a claude usage limit; leaving it IN_PROGRESS with its changes in place", specFile)
		taskEnd(specs.StatusInProgress, "usage_limit")
		return false, &fatalError{cause}
	}
	closeState := true
	defer func() {
		if closeState {
			o.closeTaskState()
		}
	}()
	if done {
		o.Log.Printf("task %s: DONE", specFile)
		if o.onMerge != nil {
			o.onMerge(specFile, true)
		}
		var err error
		if ws != nil && ws.worktree {
			// Only a parallel run re-executes a conflicting task, and only
			// once.
			reexec, err = o.finishDoneWorktree(ctx, specFile, ws, o.parallel() > 1 && ws.attempt < 2)
		} else {
			if ws != nil {
				ws.removeBaseFile()
			}
			var landed string
			landed, err = o.commitDone(ctx, specFile)
			if ws != nil {
				ws.mergeCommit = landed
			}
		}
		if o.onMerge != nil {
			o.onMerge(specFile, false)
		}
		if reexec {
			closeState = false
			return true, nil
		}
		// finishDoneWorktree turns a task whose merge failed BLOCKED.
		status, serr := o.getStatus(specFile)
		if serr != nil || status == "" {
			status = specs.StatusDone
		}
		reason := ""
		if err == nil && serr == nil && status == specs.StatusDone && ws != nil && !o.offline() && o.hasTaskDoneHandlers() {
			if o.runTaskDoneHooks(ctx, specFile, num, ws, uw) {
				reason = "on_task_done_failed"
			}
		}
		taskEnd(status, reason)
		return false, err
	}
	if len(onFail.split) > 0 {
		// Not a failure: the split-off tasks carry on in its place.
		err := o.splitTask(ctx, specFile, num, ws, onFail.applied, onFail.split)
		taskEnd(specs.StatusBlocked, "split")
		if err != nil {
			return false, &fatalError{fmt.Errorf("task %s: preserve split changes: %w", specFile, err)}
		}
		return false, nil
	}
	if onFail.blocked {
		err := o.blockTask(ctx, specFile, num, ws, onFail.applied)
		taskEnd(specs.StatusBlocked, "on_fail")
		if err != nil {
			return false, &fatalError{fmt.Errorf("task %s: preserve BLOCKED changes: %w", specFile, err)}
		}
		return false, nil
	}
	if cause == nil {
		o.Log.Printf("task %s: FAILED after %d iteration(s)", specFile, o.Cfg.Iter)
	}
	err = o.failTask(ctx, specFile, num, ws, cause)
	taskEnd(specs.StatusFailed, "")
	if err != nil {
		return false, &fatalError{fmt.Errorf("task %s: park FAILED changes: %w", specFile, err)}
	}
	return false, cause
}

// runIterations runs up to cfg.Iter planner/worker/tester rounds for one
// task, with the red-green check (cfg.RedGreen) between worker and tester:
// a failed check writes the iteration's review itself and skips tester.
// done reports whether tester marked the task DONE; a non-nil err is why
// the task was cut short (an agent error, --max-call-cost, ...). Both
// false and nil means the iteration budget ran out.
//
// resume, when non-nil, is the task's saved run-state.json record: the
// loop then starts at the first agent of its iteration that hadn't
// finished (see runstate.Task.Next), unless the reviews on disk are
// already past it.
//
// priorAttempt, when set, is a re-executed task's first-attempt history
// (see reexecTask): planner and worker get it in iteration 1 in place of
// the previous review.
func (o *Orchestrator) runIterations(ctx context.Context, specFile, num string, ws *workspace, resume *runstate.Task, priorAttempt string, planner, worker, tester *agents.Agent, uw *usage.Writer) (done bool, err error) {
	specPath := filepath.Join(o.Layout.SpecDir, specFile)
	specContent, err := os.ReadFile(specPath)
	if err != nil {
		return false, fmt.Errorf("read spec file: %w", err)
	}

	// Resume point after a crash: skip iterations whose review already
	// exists on disk (planner/worker/tester already ran for them before the
	// process died) instead of always restarting this task from iteration 1
	// and burning through the --ITER budget again on work already done.
	completed := lastCompletedIteration(o.Layout.SpecDir, num, o.Cfg.Iter)
	startIter, startAgent := completed+1, ""
	if resume != nil {
		// The state may lawfully be ahead of the reviews (an empty-diff
		// iteration writes none), but never behind them.
		if rIter, rAgent := resume.Next(); rIter >= completed {
			startIter, startAgent = rIter, rAgent
			o.Log.Printf("%s: resuming from %s at iteration %d, agent %s", specFile, runstate.FileName, startIter, agentLabel(startAgent))
		} else {
			o.Log.Printf("warning: %s: %s is behind the reviews on disk; resuming from spec/ files", specFile, runstate.FileName)
		}
	}
	if startAgent == "" && startIter > 1 {
		o.Log.Printf("%s: resuming at iteration %d/%d (found review(s) from a previous run)", specFile, startIter, o.Cfg.Iter)
	}
	o.beginTaskState(specFile, num, ws, startIter, cmp.Or(startAgent, runstate.Agents[0]))

	for iter := startIter; iter <= o.Cfg.Iter; iter++ {
		if o.stopRequested() {
			return false, ErrInterrupted
		}
		done, err := o.runIteration(ctx, iterationInput{
			specFile: specFile, num: num, specContent: specContent, ws: ws, iter: iter,
			resumePoint: iter == startIter, startAgent: startAgent, priorAttempt: priorAttempt,
			planner: planner, worker: worker, tester: tester, uw: uw,
		})
		if done || err != nil {
			return done, err
		}
		// With --parallel the run budget is shared by every task in
		// flight: one crossed by any of them stops this task here, at the
		// end of its iteration (see checkBudgets).
		if o.parallel() > 1 {
			if err := o.runBudgetExceeded(uw); err != nil {
				o.Log.Printf("%s: run budget exceeded; stopping at the end of iteration %d", specFile, iter)
				return false, err
			}
		}
	}

	// A resume point past --ITER (a run stopped during the escalated
	// iteration of --on-fail escalate) runs no iteration here: the budget
	// is spent, so the --on-fail chain starts over, and the escalated
	// iteration is rerun from its planner.
	return false, nil
}

// iterationInput is one planner/worker/tester round of a task: what
// runIteration needs to run iteration iter.
type iterationInput struct {
	specFile, num string
	specContent   []byte
	ws            *workspace
	iter          int

	// resumePoint is set for the first iteration a (possibly resumed) run
	// starts with: an existing plan may then be reused, and startAgent
	// (from run-state.json, "" when unknown) says which agent is next.
	resumePoint bool
	startAgent  string

	// escalated marks the extra iteration of --on-fail escalate, numbered
	// --ITER+1, with planner and worker on the escalation model.
	escalated bool

	// priorAttempt replaces the previous review in iteration 1: a
	// re-executed task's first-attempt history (see reexecTask).
	priorAttempt string

	planner, worker, tester *agents.Agent
	uw                      *usage.Writer
}

// runIteration runs one planner -> worker -> tester round (see
// runIterations): done reports tester's DONE verdict, a non-nil err why the
// task was cut short; both false and nil is an unsuccessful iteration (a
// rejected plan, an empty diff, a failed red-green check or a NOT_DONE
// verdict).
func (o *Orchestrator) runIteration(ctx context.Context, in iterationInput) (done bool, err error) {
	specFile, num, specContent, ws, iter, uw := in.specFile, in.num, in.specContent, in.ws, in.iter, in.uw
	planner, worker, tester, startAgent := in.planner, in.worker, in.tester, in.startAgent
	implPlanFile := fmt.Sprintf("%s_IMPL_PLAN.md", num)
	implPlanPath := filepath.Join(o.Layout.SpecDir, implPlanFile)

	if in.escalated {
		o.Log.Printf("--- %s escalated iteration %d (after %d; planner/worker on %s) ---", specFile, iter, o.Cfg.Iter, planner.Model)
		o.event(specFile, iter, "", "escalation_start", runlog.F("model", planner.Model), runlog.F("after", o.Cfg.Iter))
	} else {
		o.Log.Printf("--- %s iteration %d/%d ---", specFile, iter, o.Cfg.Iter)
	}
	call := agentCall{specFile: specFile, num: num, iter: iter, escalated: in.escalated, workDir: o.Cfg.Dir, uw: uw, tally: &iterationTally{}}
	if ws != nil {
		call.workDir = ws.dir
	}

	reviewFile := fmt.Sprintf("%s_REVIEW_%d.md", num, iter)
	reviewPath := filepath.Join(o.Layout.SpecDir, reviewFile)

	var priorReview string
	if iter > 1 {
		prevReviewPath := filepath.Join(o.Layout.SpecDir, fmt.Sprintf("%s_REVIEW_%d.md", num, iter-1))
		if b, err := os.ReadFile(prevReviewPath); err == nil {
			priorReview = string(b)
		}
	}
	if iter == 1 && in.priorAttempt != "" {
		priorReview = in.priorAttempt
	}
	questionsBlock := o.questionsBlock(specFile)

	// At the exact resume point, an existing plan from before the crash
	// is reused as-is rather than regenerated — the task's own request.
	// Every iteration beyond that still asks planner to update the plan
	// against that iteration's fresh review, as usual. A plan missing
	// the required Files/Test Command sections (e.g. left over from
	// before this validation existed) is never reused — planner runs
	// again so the contract is enforced even across a restart.
	//
	// Resumed from run-state.json, the plan is reused when planner had
	// finished (worker or tester is next), and worker is skipped too
	// when tester is next; a planner cut off mid-call runs again, as
	// its plan file may be a partial write.
	reusingPlan, skipWorker := false, false
	if in.resumePoint {
		planOK := false
		if b, err := os.ReadFile(implPlanPath); err == nil {
			plan := strings.TrimSpace(string(b))
			planOK = plan != "" && implplan.HasRequiredSections(plan)
		}
		switch startAgent {
		case "planner":
		case "worker", "tester":
			if planOK {
				reusingPlan, skipWorker = true, startAgent == "tester"
				o.Log.Printf("%s: reusing existing %s from before the restart", specFile, implPlanFile)
			} else {
				o.Log.Printf("warning: %s: %s says %s is next, but %s is missing the required sections; starting the iteration with planner", specFile, runstate.FileName, startAgent, implPlanFile)
			}
		default:
			if planOK {
				reusingPlan = true
				o.Log.Printf("%s: reusing existing %s from before the restart", specFile, implPlanFile)
			}
		}
	}
	first := "planner"
	switch {
	case skipWorker:
		first = "tester"
	case reusingPlan:
		first = "worker"
	}
	o.setAgentState(iter, first, runstate.AgentPending)

	if !reusingPlan {
		result, err := o.runStep(ctx, call, planner, plannerPrompt(specFile, specContent, priorReview, implPlanPath, questionsBlock))
		o.copyArtifact(specFile, num, fmt.Sprintf("iter-%d-IMPL_PLAN.md", iter), implPlanPath)
		if err := o.checkBudgets(specFile, uw); err != nil {
			return false, err
		}
		if err != nil {
			return false, fmt.Errorf("planner: %w", err)
		}
		if err := o.checkCallCost("planner", result); err != nil {
			return false, err
		}

		planBytes, _ := os.ReadFile(implPlanPath)
		if !implplan.HasRequiredSections(string(planBytes)) {
			note := fmt.Sprintf(
				"Implementation plan rejected: %s must contain both a \"## %s\" section and a "+
					"\"## %s\" section, using exactly these headings.\n\n"+
					"\"## %s\" must list at least one file, one per bullet, in the format\n"+
					"    %s\n"+
					"with the path first (backticks optional) — only the path is parsed, so it must be "+
					"exact and relative to the repository root — and the list must be exhaustive, since "+
					"worker works strictly from it without exploring the repository. Prose such as "+
					"\"(none yet)\" or \"TBD\" does not count as a file.\n\n"+
					"\"## %s\" holds the concrete command(s) that run this project's tests and linter.\n\n"+
					"Rewrite the plan with both sections filled in.",
				implPlanFile, implplan.FilesHeading, implplan.TestCommandHeading,
				implplan.FilesHeading, implplan.FilesBulletFormat, implplan.TestCommandHeading)
			if err := os.WriteFile(reviewPath, []byte(note), 0o644); err != nil {
				return false, fmt.Errorf("write plan-contract review: %w", err)
			}
			o.Log.Printf("%s iteration %d: %s is missing or empty in the required Files/Test Command sections (Files must list at least one file); retrying planner next iteration", specFile, iter, implPlanFile)
			o.copyArtifact(specFile, num, fmt.Sprintf("iter-%d-REVIEW.md", iter), reviewPath)
			o.event(specFile, iter, "", "plan_rejected", runlog.F("review", reviewFile))
			return false, nil
		}
		if o.stopRequested() {
			return false, ErrInterrupted
		}
	} else {
		o.copyArtifact(specFile, num, fmt.Sprintf("iter-%d-IMPL_PLAN.md", iter), implPlanPath)
	}

	implPlanBytes, err := os.ReadFile(implPlanPath)
	if err != nil {
		return false, fmt.Errorf("read impl plan: %w", err)
	}
	plan := string(implPlanBytes)
	o.warnMissingRelevantADRs(specFile, implPlanFile, plan, iter)

	// workerOutput is this iteration's worker answer, for its
	// ## Decisions section; workerRan is false when a resumed run
	// skipped worker, whose answer is then not available.
	var workerOutput string
	workerRan := false
	if !skipWorker {
		workerResult, err := o.runStep(ctx, call, worker, workerPrompt(specFile, specContent, implPlanFile, plan, priorReview, questionsBlock))
		if err := o.checkBudgets(specFile, uw); err != nil {
			return false, err
		}
		if err != nil {
			return false, fmt.Errorf("worker: %w", err)
		}
		if err := o.checkCallCost("worker", workerResult); err != nil {
			return false, err
		}
		workerOutput, workerRan = workerResult.Output, true
		if o.stopRequested() {
			return false, ErrInterrupted
		}
	}

	testCommand := implplan.TestCommand(plan)

	diffForTester := "[dry-run] diff computation skipped"
	diffNote := ""
	guidance := normalDiffGuidance
	if !o.offline() {
		td, err := o.buildTesterDiff(ctx, ws, plan)
		if err != nil {
			return false, err
		}
		if td.Empty {
			o.Log.Printf("%s iteration %d: empty diff after worker, skipping tester (iteration unsuccessful)", specFile, iter)
			o.event(specFile, iter, "", "empty_diff")
			return false, nil
		}
		diffForTester, diffNote = td.Body, td.Note
		if td.Oversized {
			guidance = fmt.Sprintf(oversizedDiffGuidance, ws.base)
		}
	}

	// The red-green tester note becomes its own paragraph after the
	// diff guidance; without one the prompt is unchanged.
	if o.Cfg.RedGreen && !o.offline() {
		rg, err := o.redGreenCheck(ctx, specFile, specContent, ws, plan)
		if err != nil {
			return false, fmt.Errorf("red-green check: %w", err)
		}
		if rg.fail {
			if err := os.WriteFile(reviewPath, []byte(rg.review), 0o644); err != nil {
				return false, fmt.Errorf("write red-green review: %w", err)
			}
			o.Log.Printf("%s iteration %d: red-green check failed, skipping tester (iteration unsuccessful)", specFile, iter)
			o.copyArtifact(specFile, num, fmt.Sprintf("iter-%d-REVIEW.md", iter), reviewPath)
			o.event(specFile, iter, "", "redgreen_failed", runlog.F("review", reviewFile))
			return false, nil
		}
		if rg.testerNote != "" {
			guidance += "\n\n" + rg.testerNote
		}
	}

	// The ADRs are read from the task's work dir, the tree planner read
	// the index from.
	adrRoot := o.Cfg.Dir
	if ws != nil && ws.dir != "" {
		adrRoot = ws.dir
	}
	adrs := o.relevantADRsBlock(adrBlockInput{
		specFile: specFile, iter: iter, plan: plan, root: adrRoot,
		workerOutput: workerOutput, workerRan: workerRan,
	})

	prompt := testerPrompt(testerPromptInput{
		specFile: specFile, specContent: specContent, implPlanFile: implPlanFile, plan: plan,
		testCommand: testCommand, diffNote: diffNote, diff: diffForTester, guidance: guidance,
		adrs: adrs, reviewPath: reviewPath, statusPath: o.Layout.StatusPath,
	})
	testerResult, err := o.runStep(ctx, call, tester, prompt)
	if err != nil {
		return false, fmt.Errorf("tester: %w", err)
	}

	status, err := o.getStatus(specFile)
	if err != nil {
		return false, err
	}
	o.copyArtifact(specFile, num, fmt.Sprintf("iter-%d-REVIEW.md", iter), reviewPath)
	verdict := "NOT_DONE"
	if status == specs.StatusDone {
		verdict = specs.StatusDone
	}
	verdictFields := []runlog.Field{runlog.F("verdict", verdict), runlog.F("review", reviewFile)}
	if in.escalated {
		verdictFields = append(verdictFields, runlog.F("escalated", true))
	}
	o.event(specFile, iter, "tester", "verdict", verdictFields...)
	if status == specs.StatusDone {
		o.event(specFile, iter, "tester", "status", runlog.F("status", specs.StatusDone), runlog.F("by", "tester"))
		o.recordDecisions(specFile, num, iter, workerOutput, workerRan)
		// A task that just reached DONE is never failed for cost after
		// the fact — that would throw away completed work for no
		// benefit, since there's no further spend left to prevent this
		// iteration was the last call regardless.
		return true, nil
	}
	if err := o.checkBudgets(specFile, uw); err != nil {
		return false, err
	}
	if err := o.checkCallCost("tester", testerResult); err != nil {
		return false, err
	}
	// A stop requested meanwhile is honoured at the top of the next
	// iteration; after the last one the budget is spent anyway.
	return false, nil
}

// runStep is runAgentPausing that records the agent in run-state.json:
// running before the call, done once it succeeded. A failed call stays
// running, so a resumed run repeats it. A call killed because Run's ctx
// was cancelled (--shutdown-timeout, a second signal) is ErrInterrupted;
// its error deliberately leaves out claude's output, so nothing in it
// looks like a limit line to diadoche-restarter. A planner or worker answer
// with a NEEDS_HUMAN line is a *needsHumanError, and the record is put back
// exactly as it was before the call: the call spent no iteration.
func (o *Orchestrator) runStep(ctx context.Context, c agentCall, a *agents.Agent, prompt string) (agents.RunResult, error) {
	var before *runstate.Task
	if o.cur != nil {
		sh := o.shared()
		sh.mu.Lock()
		b := *o.cur
		sh.mu.Unlock()
		before = &b
	}
	o.setAgentState(c.iter, a.Name, runstate.AgentRunning)
	result, err := o.runAgentPausing(ctx, c, a, prompt)
	if err != nil {
		var paused *pauseInterruptedError
		if ctx.Err() != nil && !errors.As(err, &paused) {
			return result, fmt.Errorf("%s call stopped: %w", a.Name, ErrInterrupted)
		}
		return result, err
	}
	if !o.Cfg.DryRun && asksHuman(a.Name, result.Output) {
		if before != nil && o.cur != nil {
			sh := o.shared()
			sh.mu.Lock()
			*o.cur = *before
			o.saveStateLocked()
			sh.mu.Unlock()
		}
		o.Log.Printf("%s answered %s", c.logPrefix(o.Cfg.Iter, a.Name), questions.Marker)
		return result, &needsHumanError{agent: a.Name, iter: c.iter}
	}
	o.setAgentState(c.iter, a.Name, runstate.AgentDone)
	return result, nil
}

// agentCall is where in the loop one agent turn happens: which task and
// iteration it belongs to, the directory the agent runs in, where its
// usage is recorded, and the running token/cost tally of the iteration
// it's part of.
type agentCall struct {
	specFile string
	num      string
	iter     int

	// escalated marks a call of the escalated iteration (--on-fail
	// escalate), which is logged as such.
	escalated bool

	// onFail names the --on-fail policy making the call outside any
	// iteration (split's preprocessor), which is logged as such.
	onFail string

	workDir string
	uw      *usage.Writer
	tally   *iterationTally
}

// logPrefix is how a call is named in the log: "<spec> iter i/n: [agent]",
// "<spec> on-fail <policy>: [agent]" for a policy's own call, or
// "<spec>: [agent]" for a call outside the iterations (the ADR step's
// adr-writer).
func (c agentCall) logPrefix(iters int, agent string) string {
	if c.onFail != "" {
		return fmt.Sprintf("%s on-fail %s: [%s]", c.specFile, c.onFail, agent)
	}
	if c.iter == 0 {
		return fmt.Sprintf("%s: [%s]", c.specFile, agent)
	}
	return fmt.Sprintf("%s iter %d/%d: [%s]", c.specFile, c.iter, iters, agent)
}

// iterationTally sums the parsed usage of every agent turn in one
// iteration, so each turn's log line can also show the iteration so far.
type iterationTally struct {
	tokens  int
	costUSD float64
}

// runAgent runs one agent turn, logging which agent is starting and, once
// it returns, how long it took and how many tokens/dollars it spent —
// alone and together with the iteration's earlier turns — then records it
// to usage.jsonl.
func (o *Orchestrator) runAgent(ctx context.Context, c agentCall, a *agents.Agent, prompt string) (agents.RunResult, error) {
	prefix := c.logPrefix(o.Cfg.Iter, a.Name)
	startFields := []runlog.Field{runlog.F("model", a.Model)}
	if c.escalated {
		prefix = fmt.Sprintf("%s iter %d (escalated): [%s]", c.specFile, c.iter, a.Name)
		startFields = append(startFields, runlog.F("escalated", true))
	}
	if c.onFail != "" {
		prefix = c.logPrefix(o.Cfg.Iter, a.Name)
		startFields = append(startFields, runlog.F("on_fail", c.onFail))
	}
	o.Log.Printf("%s started (model=%s)", prefix, a.Model)
	o.event(c.specFile, c.iter, a.Name, "agent_start", startFields...)

	stopHeartbeat := o.startHeartbeat(prefix)
	result, err := o.Runner.Run(ctx, c.workDir, a, prompt)
	stopHeartbeat()
	o.recordUsage(c.uw, c.specFile, a.Name, c.iter, a.Model, result)
	o.saveCallOutput(c, a, result)
	cost, outcomeField := "unknown", "ok"
	if result.Parsed {
		cost = fmt.Sprintf("$%.4f", result.TotalCostUSD)
	}
	if err != nil {
		outcomeField = "failed"
	}
	o.event(c.specFile, c.iter, a.Name, "agent_end",
		runlog.F("duration", (time.Duration(result.DurationMS)*time.Millisecond).Round(time.Millisecond)),
		runlog.F("exit", result.ExitCode), runlog.F("cost", cost),
		runlog.F("tokens", result.Usage.Total()), runlog.F("result", outcomeField))

	if result.Parsed {
		c.tally.tokens += result.Usage.Total()
		c.tally.costUSD += result.TotalCostUSD
	}
	outcome := "finished"
	if err != nil {
		outcome = "failed"
	}
	o.Log.Printf("%s %s — %s | iteration so far: %d tokens, $%.4f",
		prefix, outcome, result.Summary(), c.tally.tokens, c.tally.costUSD)
	return result, err
}

// startHeartbeat logs "<prefix> still running (<elapsed>)" every
// HeartbeatInterval until the returned stop func is called, so a long agent
// turn is visibly alive rather than looking hung. stop waits for the
// heartbeat goroutine to exit, so nothing is logged after it returns.
func (o *Orchestrator) startHeartbeat(prefix string) (stop func()) {
	interval := o.HeartbeatInterval
	if interval <= 0 {
		interval = defaultHeartbeatInterval
	}
	start := time.Now()
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				o.Log.Printf("%s still running (%s)", prefix, time.Since(start).Round(time.Second))
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

// checkCallCost enforces --max-call-cost: if result carries a parsed cost
// over the configured cap, the task fails right there instead of spending
// on another agent/iteration. Cost is only known once a call has already
// returned, so this can only stop *further* spend, never the call it's
// checking — --max-call-minutes is what bounds an individual call.
func (o *Orchestrator) checkCallCost(agentName string, result agents.RunResult) error {
	if o.Cfg.MaxCallCostUSD <= 0 || !result.Parsed {
		return nil
	}
	if result.TotalCostUSD > o.Cfg.MaxCallCostUSD {
		return fmt.Errorf("%s call cost $%.4f, exceeding --max-call-cost=$%.4f", agentName, result.TotalCostUSD, o.Cfg.MaxCallCostUSD)
	}
	return nil
}

// recordUsage appends one usage.jsonl line for a single agent call. A nil
// uw (--dry-run) is a silent no-op. Neither a write failure nor an
// unparseable claude JSON result is ever fatal to the run — both are
// logged as a warning, per 002_usage_accounting.md's "must not crash
// the run" requirement.
func (o *Orchestrator) recordUsage(uw *usage.Writer, specFile, agentName string, iter int, model string, result agents.RunResult) {
	if uw == nil {
		return
	}
	rec := usage.Record{
		Time:                     time.Now(),
		Task:                     specFile,
		Agent:                    agentName,
		Iteration:                iter,
		Model:                    model,
		InputTokens:              result.Usage.InputTokens,
		OutputTokens:             result.Usage.OutputTokens,
		CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
		CostUSD:                  result.TotalCostUSD,
		DurationMS:               result.DurationMS,
		ExitCode:                 result.ExitCode,
		Parsed:                   result.Parsed,
	}
	if err := uw.Append(rec); err != nil {
		o.Log.Printf("%s: failed to write usage record for %s: %v", specFile, agentName, err)
		return
	}
	if !result.Parsed {
		o.Log.Printf("%s: could not parse usage/cost from %s's claude output; recorded a zeroed usage line", specFile, agentName)
	}
}

// relExcludeDirs resolves each of dirs relative to base (skipping any that
// can't be, e.g. on a different volume) and returns them slash-separated,
// the form both git pathspecs and excludeUnder expect.
func relExcludeDirs(base string, dirs ...string) []string {
	var out []string
	for _, d := range dirs {
		if rel, err := filepath.Rel(base, d); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out
}

// excludeUnder filters out any path in files that is one of dirs itself or
// lies under one of them (git always reports paths with "/", so this
// compares against "/" regardless of OS).
func excludeUnder(files []string, dirs []string) []string {
	if len(dirs) == 0 {
		return files
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		excluded := false
		for _, d := range dirs {
			if f == d || strings.HasPrefix(f, d+"/") {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, f)
		}
	}
	return out
}

// lastCompletedIteration returns the highest N in 1..maxIter for which
// spec/<num>_REVIEW_N.md exists — i.e. the last iteration whose full
// planner/worker/tester cycle actually ran and got reviewed before this
// process (or a previous one) stopped. 0 means no iteration completed yet.
func lastCompletedIteration(specDir, num string, maxIter int) int {
	last := 0
	for n := 1; n <= maxIter; n++ {
		path := filepath.Join(specDir, fmt.Sprintf("%s_REVIEW_%d.md", num, n))
		if _, err := os.Stat(path); err == nil {
			last = n
		}
	}
	return last
}

// commitDone commits everything the planner/worker/tester changed for a
// task that just turned DONE, so completed work is captured as its own git
// commit. A no-op in --dry-run mode, and a no-op if there's nothing to
// commit (e.g. the task only involved discussion, no file changes).
// landed is HEAD afterwards: the commit the task's changes landed as.
func (o *Orchestrator) commitDone(ctx context.Context, specFile string) (landed string, err error) {
	if o.offline() {
		o.Log.Printf("[dry-run] would commit: task %s done", specFile)
		return "", nil
	}
	msg := fmt.Sprintf("diadoche: complete %s", specFile)
	if err := gitutil.CommitAll(ctx, o.Cfg.Dir, msg); err != nil {
		return "", fmt.Errorf("commit %s: %w", specFile, err)
	}
	o.Log.Printf("task %s: committed", specFile)
	o.event(specFile, 0, "", "commit")
	landed, err = gitutil.BaseCommit(ctx, o.Cfg.Dir)
	if err != nil {
		o.Log.Printf("warning: task %s: couldn't read the commit it landed as: %v", specFile, err)
		return "", nil
	}
	return landed, nil
}
