// Package orchestrator runs diadoche's main loop: sync the spec queue,
// then for each queued task run planner -> worker -> tester for up to
// cfg.Iter iterations, until the tester marks the task DONE in STATUS.md or
// the iteration budget is exhausted (FAILED).
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

type Orchestrator struct {
	Cfg    config.Config
	Layout specs.Layout
	Runner agents.Runner
	Log    *log.Logger
}

func New(cfg config.Config) *Orchestrator {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	var runner agents.Runner
	if cfg.DryRun {
		runner = agents.DryRunRunner{Log: logger}
	} else {
		runner = agents.CLIRunner{Bin: cfg.ClaudeBin, MaxCallDuration: time.Duration(cfg.MaxCallMinutes) * time.Minute}
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
	if err := agents.ApplyModelOverride(planner, o.Cfg.PlannerModel); err != nil {
		return err
	}
	worker, err := agents.LoadOrDefault(o.Layout.AgentsDir, "worker")
	if err != nil {
		return fmt.Errorf("load worker agent: %w", err)
	}
	if err := agents.ApplyModelOverride(worker, o.Cfg.WorkerModel); err != nil {
		return err
	}
	tester, err := agents.LoadOrDefault(o.Layout.AgentsDir, "tester")
	if err != nil {
		return fmt.Errorf("load tester agent: %w", err)
	}
	if err := agents.ApplyModelOverride(tester, o.Cfg.TesterModel); err != nil {
		return err
	}
	o.Log.Printf("models: planner=%s worker=%s tester=%s", planner.Model, worker.Model, tester.Model)

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
			if err := o.processTask(ctx, next, planner, worker, tester, matchedSkills, uw); err != nil {
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

func (o *Orchestrator) processTask(ctx context.Context, specFile string, planner, worker, tester *agents.Agent, skillNames []string, uw *usage.Writer) error {
	o.Log.Printf("=== task %s ===", specFile)
	num := specs.TaskNumber(specFile)

	var ws *workspace
	if !o.Cfg.DryRun {
		var err error
		ws, err = o.prepareWorkspace(ctx, specFile, num)
		if err != nil {
			return &fatalError{err}
		}
	}

	if err := specs.SetStatus(o.Layout, specFile, specs.StatusInProgress); err != nil {
		return err
	}

	done, cause := o.runIterations(ctx, specFile, num, ws, planner, worker, tester, skillNames, uw)
	if cause != nil && usagelimit.Mentions(cause.Error()) {
		// Not a failure: the account's usage window ran out. Nothing is
		// parked and the task stays IN_PROGRESS, so the next run (e.g. by
		// diadoche-restarter once the window reopens) resumes it with its
		// changes still in the working tree.
		o.Log.Printf("task %s: interrupted by a claude usage limit; leaving it IN_PROGRESS with its changes in place", specFile)
		return &fatalError{cause}
	}
	if done {
		o.Log.Printf("task %s: DONE", specFile)
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
// task. done reports whether tester marked the task DONE; a non-nil err is
// why the task was cut short (an agent error, --max-call-cost, ...). Both
// false and nil means the iteration budget ran out.
func (o *Orchestrator) runIterations(ctx context.Context, specFile, num string, ws *workspace, planner, worker, tester *agents.Agent, skillNames []string, uw *usage.Writer) (done bool, err error) {
	specPath := filepath.Join(o.Layout.SpecDir, specFile)
	specContent, err := os.ReadFile(specPath)
	if err != nil {
		return false, fmt.Errorf("read spec file: %w", err)
	}

	skillsNote := ""
	if len(skillNames) > 0 {
		skillsNote = fmt.Sprintf("\n\nRelevant skills available under .claude/skills: %s. Use them where applicable.", strings.Join(skillNames, ", "))
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
			plannerPrompt += fmt.Sprintf("\nWrite (or update) the implementation plan to %s.%s", implPlanPath, skillsNote)

			result, err := o.Runner.Run(ctx, o.Cfg.Dir, planner, plannerPrompt)
			o.recordUsage(uw, specFile, "planner", iter, planner.Model, result)
			if err != nil {
				return false, fmt.Errorf("planner: %w", err)
			}
			if err := o.checkCallCost("planner", result); err != nil {
				return false, err
			}

			planBytes, _ := os.ReadFile(implPlanPath)
			if !implplan.HasRequiredSections(string(planBytes)) {
				note := fmt.Sprintf(
					"Implementation plan rejected: %s must contain both a \"## %s\" section "+
						"(the exact file paths to create or modify, one per bullet) and a \"## %s\" section "+
						"(the command to run this project's tests and linter). Rewrite the plan to include "+
						"both sections in full, using exactly these headings.",
					implPlanFile, implplan.FilesHeading, implplan.TestCommandHeading)
				if err := os.WriteFile(reviewPath, []byte(note), 0o644); err != nil {
					return false, fmt.Errorf("write plan-contract review: %w", err)
				}
				o.Log.Printf("%s iteration %d: %s is missing the required Files/Test Command sections; retrying planner next iteration", specFile, iter, implPlanFile)
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
		workerPrompt += "\nImplement the plan in this repository." + skillsNote

		workerResult, err := o.Runner.Run(ctx, o.Cfg.Dir, worker, workerPrompt)
		o.recordUsage(uw, specFile, "worker", iter, worker.Model, workerResult)
		if err != nil {
			return false, fmt.Errorf("worker: %w", err)
		}
		if err := o.checkCallCost("worker", workerResult); err != nil {
			return false, err
		}

		testCommand := implplan.TestCommand(plan)

		diffForTester := "[dry-run] diff computation skipped"
		diffNote := ""
		if !o.Cfg.DryRun {
			diff, err := gitutil.DiffSinceExcluding(ctx, o.Cfg.Dir, ws.base, ws.exclude...)
			if err != nil {
				return false, fmt.Errorf("git diff: %w", err)
			}
			untrackedAll, err := gitutil.UntrackedFiles(ctx, o.Cfg.Dir)
			if err != nil {
				return false, fmt.Errorf("git ls-files: %w", err)
			}
			untracked := excludeUnder(untrackedAll, ws.exclude)

			if strings.TrimSpace(diff) == "" && len(untracked) == 0 {
				o.Log.Printf("%s iteration %d: empty diff after worker, skipping tester (iteration unsuccessful)", specFile, iter)
				continue
			}

			if len(diff) > o.Cfg.MaxDiffBytes {
				stat, err := gitutil.DiffStatSinceExcluding(ctx, o.Cfg.Dir, ws.base, ws.exclude...)
				if err != nil {
					return false, fmt.Errorf("git diff --stat: %w", err)
				}
				restricted, err := gitutil.DiffSinceForFiles(ctx, o.Cfg.Dir, ws.base, implplan.Files(plan))
				if err != nil {
					return false, fmt.Errorf("git diff for plan files: %w", err)
				}
				diffForTester = stat + "\n\n" + restricted
				diffNote = fmt.Sprintf(" (exceeded --max-diff-bytes=%d; showing `git diff --stat` plus the diff restricted to the plan's %s list)", o.Cfg.MaxDiffBytes, implplan.FilesHeading)
			} else {
				diffForTester = diff
			}
			if len(untracked) > 0 {
				diffForTester += "\n\nUntracked files:\n" + strings.Join(untracked, "\n")
			}
		}

		testerPrompt := fmt.Sprintf(
			"Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n\n"+
				"Test/lint command from the plan:\n%s\n\n"+
				"Diff of the worker's changes since this task started%s:\n%s\n\n"+
				"Review the diff and the test results against the spec and plan. "+
				"Write your findings to %s. If, and only if, the task is fully and correctly done, "+
				"update the row for %q in %s to status DONE.",
			specFile, specContent, implPlanFile, plan, testCommand, diffNote, diffForTester, reviewPath, specFile, o.Layout.StatusPath)
		testerPrompt += skillsNote

		testerResult, err := o.Runner.Run(ctx, o.Cfg.Dir, tester, testerPrompt)
		o.recordUsage(uw, specFile, "tester", iter, tester.Model, testerResult)
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
// logged as a warning, per 09_optimize_agents.md's "не должен ронять
// прогон" requirement.
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
