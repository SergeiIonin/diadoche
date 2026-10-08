// Package orchestrator runs diadoche's main loop: sync the spec queue,
// then for each queued task run planner -> worker -> tester for up to
// cfg.Iter iterations, until the tester marks the task DONE in STATUS.md or
// the iteration budget is exhausted (FAILED). Between worker and tester,
// the red-green check (see redgreen.go) makes sure worker's tests fail on
// the task's base commit, i.e. actually test the new code.
//
// With cfg.Worktree each task runs in its own git worktree and branch
// (see worktree.go), merged into the current branch once DONE; without it
// tasks run in the main working tree and FAILED changes are parked (see
// park.go).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/implplan"
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

func (o *Orchestrator) Run(ctx context.Context) error {
	if err := o.Layout.Ensure(); err != nil {
		return fmt.Errorf("ensure layout: %w", err)
	}

	if !o.Cfg.DryRun {
		if err := gitutil.EnsureRepo(ctx, o.Cfg.Dir); err != nil {
			return fmt.Errorf("ensure git repo: %w", err)
		}
		if o.Cfg.Worktree {
			if err := o.prepareWorktreeRun(ctx); err != nil {
				return err
			}
		}
	}

	matchedSkills, err := skills.Resolve(o.Layout.SkillsDir, o.Cfg.Langs, o.Cfg.Frameworks)
	if err != nil {
		return fmt.Errorf("resolve skills: %w", err)
	}
	if len(matchedSkills) > 0 {
		o.Log.Printf("skills in scope: %s", strings.Join(matchedSkills, ", "))
	}

	planner, err := agents.LoadOrDefault(o.Layout.AgentsDir, "planner")
	if err != nil {
		return fmt.Errorf("load planner agent: %w", err)
	}
	if err := agents.ResolveModel(planner, o.Cfg.PlannerModel); err != nil {
		return err
	}
	worker, err := agents.LoadOrDefault(o.Layout.AgentsDir, "worker")
	if err != nil {
		return fmt.Errorf("load worker agent: %w", err)
	}
	if err := agents.ResolveModel(worker, o.Cfg.WorkerModel); err != nil {
		return err
	}
	tester, err := agents.LoadOrDefault(o.Layout.AgentsDir, "tester")
	if err != nil {
		return fmt.Errorf("load tester agent: %w", err)
	}
	if err := agents.ResolveModel(tester, o.Cfg.TesterModel); err != nil {
		return err
	}
	o.Log.Printf("models: planner=%s worker=%s tester=%s", planner.ModelDescription(), worker.ModelDescription(), tester.ModelDescription())

	// One prefix for the whole run, resolved once and shared byte for byte
	// by all three agents: that identity is what lets the API's prompt
	// cache serve the common context to the next agent instead of charging
	// for it again (see docs/prompt-caching.md).
	basePrefix, prefixSource, err := agents.LoadPrefixOrDefault(o.Layout.Root)
	if err != nil {
		return fmt.Errorf("load prompt prefix: %w", err)
	}
	prefix := agents.BuildPrefix(basePrefix, matchedSkills)
	planner.Prefix, worker.Prefix, tester.Prefix = prefix, prefix, prefix
	o.Log.Printf("prompt prefix: %d bytes (source: %s)", len(prefix), prefixSource)

	// Usage accounting writes real claude-CLI JSON output to disk; skip it
	// entirely in --dry-run, where there's no real usage to record and no
	// guarantee the project even has a .claude/diadoche directory worth
	// writing into yet.
	var uw *usage.Writer
	if !o.Cfg.DryRun {
		w, err := usage.Open(filepath.Join(o.Layout.Root, "usage.jsonl"))
		if err != nil {
			return fmt.Errorf("open usage log: %w", err)
		}
		defer w.Close()
		uw = w
	}

	for {
		if err := o.syncQueueLocked(); err != nil {
			return fmt.Errorf("sync spec queue: %w", err)
		}

		processedAny := false
		consecutiveFailures := 0
		for {
			next, err := specs.PopQueue(o.Layout)
			if err != nil {
				return fmt.Errorf("pop queue: %w", err)
			}
			if next == "" {
				break
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

		if !processedAny {
			// Nothing left to do and nothing new showed up in this round's
			// sync: no leftover work, no new task specs. We're done.
			o.printUsageSummary(uw)
			return nil
		}
		o.Log.Printf("queue drained, re-checking %s for new task specs...", o.Layout.SpecDir)
	}
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
	_, _, err = specs.Sync(o.Layout)
	return err
}

func (o *Orchestrator) processTask(ctx context.Context, specFile string, planner, worker, tester *agents.Agent, uw *usage.Writer) error {
	o.Log.Printf("=== task %s ===", specFile)
	num := specs.TaskNumber(specFile)

	var ws *workspace
	if !o.Cfg.DryRun {
		var err error
		if o.Cfg.Worktree {
			ws, err = o.prepareWorktree(ctx, specFile, num)
		} else {
			ws, err = o.prepareWorkspace(ctx, specFile, num)
		}
		if err != nil {
			return &fatalError{err}
		}
	}

	if err := specs.SetStatus(o.Layout, specFile, specs.StatusInProgress); err != nil {
		return err
	}

	done, cause := o.runIterations(ctx, specFile, num, ws, planner, worker, tester, uw)
	if cause != nil && usagelimit.Mentions(cause.Error()) {
		// Not a failure: the account's usage window ran out. Nothing is
		// parked and the task stays IN_PROGRESS, so the next run (e.g. by
		// diadoche-restarter once the window reopens) resumes it with its
		// changes still in the working tree (or its worktree).
		o.Log.Printf("task %s: interrupted by a claude usage limit; leaving it IN_PROGRESS with its changes in place", specFile)
		return &fatalError{cause}
	}
	if done {
		o.Log.Printf("task %s: DONE", specFile)
		if ws != nil && ws.worktree {
			return o.finishDoneWorktree(ctx, specFile, ws)
		}
		if ws != nil {
			ws.removeBaseFile()
		}
		return o.commitDone(ctx, specFile)
	}
	if cause == nil {
		o.Log.Printf("task %s: FAILED after %d iteration(s)", specFile, o.Cfg.Iter)
	}
	if err := o.failTask(ctx, specFile, num, ws, cause); err != nil {
		return &fatalError{fmt.Errorf("task %s: park FAILED changes: %w", specFile, err)}
	}
	return cause
}

// runIterations runs up to cfg.Iter planner/worker/tester rounds for one
// task, with the red-green check (cfg.RedGreen) between worker and tester:
// a failed check writes the iteration's review itself and skips tester.
// done reports whether tester marked the task DONE; a non-nil err is why
// the task was cut short (an agent error, --max-call-cost, ...). Both
// false and nil means the iteration budget ran out.
func (o *Orchestrator) runIterations(ctx context.Context, specFile, num string, ws *workspace, planner, worker, tester *agents.Agent, uw *usage.Writer) (done bool, err error) {
	specPath := filepath.Join(o.Layout.SpecDir, specFile)
	specContent, err := os.ReadFile(specPath)
	if err != nil {
		return false, fmt.Errorf("read spec file: %w", err)
	}

	implPlanFile := fmt.Sprintf("%s_IMPL_PLAN.md", num)
	implPlanPath := filepath.Join(o.Layout.SpecDir, implPlanFile)

	// Resume point after a crash: skip iterations whose review already
	// exists on disk (planner/worker/tester already ran for them before the
	// process died) instead of always restarting this task from iteration 1
	// and burning through the --ITER budget again on work already done.
	startIter := lastCompletedIteration(o.Layout.SpecDir, num, o.Cfg.Iter) + 1
	if startIter > 1 {
		o.Log.Printf("%s: resuming at iteration %d/%d (found review(s) from a previous run)", specFile, startIter, o.Cfg.Iter)
	}

	for iter := startIter; iter <= o.Cfg.Iter; iter++ {
		o.Log.Printf("--- %s iteration %d/%d ---", specFile, iter, o.Cfg.Iter)
		call := agentCall{specFile: specFile, iter: iter, workDir: o.Cfg.Dir, uw: uw, tally: &iterationTally{}}
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

		// At the exact resume point, an existing plan from before the crash
		// is reused as-is rather than regenerated — the task's own request.
		// Every iteration beyond that still asks planner to update the plan
		// against that iteration's fresh review, as usual. A plan missing
		// the required Files/Test Command sections (e.g. left over from
		// before this validation existed) is never reused — planner runs
		// again so the contract is enforced even across a restart.
		reusingPlan := false
		if iter == startIter {
			if b, err := os.ReadFile(implPlanPath); err == nil {
				if plan := strings.TrimSpace(string(b)); plan != "" && implplan.HasRequiredSections(plan) {
					reusingPlan = true
					o.Log.Printf("%s: reusing existing %s from before the restart", specFile, implPlanFile)
				}
			}
		}

		if !reusingPlan {
			plannerPrompt := fmt.Sprintf("Task spec (%s):\n%s\n", specFile, specContent)
			if priorReview != "" {
				plannerPrompt += fmt.Sprintf("\nFeedback from the previous review, address every point:\n%s\n", priorReview)
			}
			plannerPrompt += fmt.Sprintf("\nWrite (or update) the implementation plan to %s.", implPlanPath)

			result, err := o.runAgent(ctx, call, planner, plannerPrompt)
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
				continue
			}
		}

		implPlanBytes, err := os.ReadFile(implPlanPath)
		if err != nil {
			return false, fmt.Errorf("read impl plan: %w", err)
		}
		plan := string(implPlanBytes)

		workerPrompt := fmt.Sprintf("Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n", specFile, specContent, implPlanFile, plan)
		if priorReview != "" {
			workerPrompt += fmt.Sprintf("\nFeedback from the previous review, address every point:\n%s\n", priorReview)
		}
		workerPrompt += "\nImplement the plan in this repository."

		workerResult, err := o.runAgent(ctx, call, worker, workerPrompt)
		if err != nil {
			return false, fmt.Errorf("worker: %w", err)
		}
		if err := o.checkCallCost("worker", workerResult); err != nil {
			return false, err
		}

		testCommand := implplan.TestCommand(plan)

		diffForTester := "[dry-run] diff computation skipped"
		diffNote := ""
		guidance := normalDiffGuidance
		if !o.Cfg.DryRun {
			td, err := o.buildTesterDiff(ctx, ws, plan)
			if err != nil {
				return false, err
			}
			if td.Empty {
				o.Log.Printf("%s iteration %d: empty diff after worker, skipping tester (iteration unsuccessful)", specFile, iter)
				continue
			}
			diffForTester, diffNote = td.Body, td.Note
			if td.Oversized {
				guidance = fmt.Sprintf(oversizedDiffGuidance, ws.base)
			}
		}

		// The red-green tester note becomes its own paragraph after the
		// diff guidance; without one the prompt is unchanged.
		if o.Cfg.RedGreen && !o.Cfg.DryRun {
			rg, err := o.redGreenCheck(ctx, specFile, specContent, ws, plan)
			if err != nil {
				return false, fmt.Errorf("red-green check: %w", err)
			}
			if rg.fail {
				if err := os.WriteFile(reviewPath, []byte(rg.review), 0o644); err != nil {
					return false, fmt.Errorf("write red-green review: %w", err)
				}
				o.Log.Printf("%s iteration %d: red-green check failed, skipping tester (iteration unsuccessful)", specFile, iter)
				continue
			}
			if rg.testerNote != "" {
				guidance += "\n\n" + rg.testerNote
			}
		}

		testerPrompt := fmt.Sprintf(
			"Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n\n"+
				"Test/lint command from the plan:\n%s\n\n"+
				"Diff of the worker's changes since this task started%s:\n%s\n\n"+
				"%s\n\n"+
				"Review the diff and the test results against the spec and plan. "+
				"Write your findings to %s. If, and only if, the task is fully and correctly done, "+
				"update the row for %q in %s to status DONE.",
			specFile, specContent, implPlanFile, plan, testCommand, diffNote, diffForTester, guidance, reviewPath, specFile, o.Layout.StatusPath)

		testerResult, err := o.runAgent(ctx, call, tester, testerPrompt)
		if err != nil {
			return false, fmt.Errorf("tester: %w", err)
		}

		status, err := specs.GetStatus(o.Layout, specFile)
		if err != nil {
			return false, err
		}
		if status == specs.StatusDone {
			// A task that just reached DONE is never failed for cost after
			// the fact — that would throw away completed work for no
			// benefit, since there's no further spend left to prevent this
			// iteration was the last call regardless.
			return true, nil
		}
		if err := o.checkCallCost("tester", testerResult); err != nil {
			return false, err
		}
	}

	return false, nil
}

// agentCall is where in the loop one agent turn happens: which task and
// iteration it belongs to, the directory the agent runs in, where its
// usage is recorded, and the running token/cost tally of the iteration
// it's part of.
type agentCall struct {
	specFile string
	iter     int
	workDir  string
	uw       *usage.Writer
	tally    *iterationTally
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
	prefix := fmt.Sprintf("%s iter %d/%d: [%s]", c.specFile, c.iter, o.Cfg.Iter, a.Name)
	o.Log.Printf("%s started (model=%s)", prefix, a.Model)

	stopHeartbeat := o.startHeartbeat(prefix)
	result, err := o.Runner.Run(ctx, c.workDir, a, prompt)
	stopHeartbeat()
	o.recordUsage(c.uw, c.specFile, a.Name, c.iter, a.Model, result)

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
func (o *Orchestrator) commitDone(ctx context.Context, specFile string) error {
	if o.Cfg.DryRun {
		o.Log.Printf("[dry-run] would commit: task %s done", specFile)
		return nil
	}
	msg := fmt.Sprintf("diadoche: complete %s", specFile)
	if err := gitutil.CommitAll(ctx, o.Cfg.Dir, msg); err != nil {
		return fmt.Errorf("commit %s: %w", specFile, err)
	}
	o.Log.Printf("task %s: committed", specFile)
	return nil
}
