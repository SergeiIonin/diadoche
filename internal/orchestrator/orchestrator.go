// Package orchestrator runs diadoche's main loop: sync the spec queue,
// then for each queued task run planner -> worker -> tester for up to
// cfg.Iter iterations, until the tester marks the task DONE in STATUS.md or
// the iteration budget is exhausted (FAILED).
package orchestrator

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/skills"
	"diadoche/internal/speclock"
	"diadoche/internal/specs"
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
		runner = agents.CLIRunner{Bin: cfg.ClaudeBin}
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
	worker, err := agents.LoadOrDefault(o.Layout.AgentsDir, "worker")
	if err != nil {
		return fmt.Errorf("load worker agent: %w", err)
	}
	tester, err := agents.LoadOrDefault(o.Layout.AgentsDir, "tester")
	if err != nil {
		return fmt.Errorf("load tester agent: %w", err)
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
			if err := o.processTask(ctx, next, planner, worker, tester, matchedSkills); err != nil {
				o.Log.Printf("task %s: error: %v", next, err)
				consecutiveFailures++
				if consecutiveFailures >= maxConsecutiveFailures {
					return fmt.Errorf("aborting after %d tasks failed in a row (last: %s): %w", consecutiveFailures, next, err)
				}
				continue
			}
			consecutiveFailures = 0
		}

		if !processedAny {
			// Nothing left to do and nothing new showed up in this round's
			// sync: no leftover work, no new task specs. We're done.
			return nil
		}
		o.Log.Printf("queue drained, re-checking %s for new task specs...", o.Layout.SpecDir)
	}
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

func (o *Orchestrator) processTask(ctx context.Context, specFile string, planner, worker, tester *agents.Agent, skillNames []string) error {
	o.Log.Printf("=== task %s ===", specFile)
	if err := specs.SetStatus(o.Layout, specFile, specs.StatusInProgress); err != nil {
		return err
	}

	num := specs.TaskNumber(specFile)
	specPath := filepath.Join(o.Layout.SpecDir, specFile)
	specContent, err := os.ReadFile(specPath)
	if err != nil {
		return o.failTask(specFile, fmt.Errorf("read spec file: %w", err))
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

	done := false
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
		// against that iteration's fresh review, as usual.
		reusingPlan := false
		if iter == startIter {
			if b, err := os.ReadFile(implPlanPath); err == nil && strings.TrimSpace(string(b)) != "" {
				reusingPlan = true
				o.Log.Printf("%s: reusing existing %s from before the restart", specFile, implPlanFile)
			}
		}

		if !reusingPlan {
			plannerPrompt := fmt.Sprintf("Task spec (%s):\n%s\n", specFile, specContent)
			if priorReview != "" {
				plannerPrompt += fmt.Sprintf("\nFeedback from the previous review, address every point:\n%s\n", priorReview)
			}
			plannerPrompt += fmt.Sprintf("\nWrite (or update) the implementation plan to %s.%s", implPlanPath, skillsNote)

			if _, err := o.Runner.Run(ctx, o.Cfg.Dir, planner, plannerPrompt); err != nil {
				return o.failTask(specFile, fmt.Errorf("planner: %w", err))
			}
		}

		implPlanContent, _ := os.ReadFile(implPlanPath)

		workerPrompt := fmt.Sprintf("Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n", specFile, specContent, implPlanFile, implPlanContent)
		if priorReview != "" {
			workerPrompt += fmt.Sprintf("\nFeedback from the previous review, address every point:\n%s\n", priorReview)
		}
		workerPrompt += "\nImplement the plan in this repository." + skillsNote

		if _, err := o.Runner.Run(ctx, o.Cfg.Dir, worker, workerPrompt); err != nil {
			return o.failTask(specFile, fmt.Errorf("worker: %w", err))
		}

		testerPrompt := fmt.Sprintf(
			"Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n\n"+
				"Review the current state of the repository against the spec and plan. "+
				"Write your findings to %s. If, and only if, the task is fully and correctly done, "+
				"update the row for %q in %s to status DONE.",
			specFile, specContent, implPlanFile, implPlanContent, reviewPath, specFile, o.Layout.StatusPath)
		testerPrompt += skillsNote

		if _, err := o.Runner.Run(ctx, o.Cfg.Dir, tester, testerPrompt); err != nil {
			return o.failTask(specFile, fmt.Errorf("tester: %w", err))
		}

		status, err := specs.GetStatus(o.Layout, specFile)
		if err != nil {
			return o.failTask(specFile, err)
		}
		if status == specs.StatusDone {
			done = true
			break
		}
	}

	if done {
		o.Log.Printf("task %s: DONE", specFile)
		return o.commitDone(ctx, specFile)
	}

	o.Log.Printf("task %s: FAILED after %d iteration(s)", specFile, o.Cfg.Iter)
	return specs.SetStatus(o.Layout, specFile, specs.StatusFailed)
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

// failTask marks specFile FAILED (best-effort — a broken STATUS.md write is
// logged but doesn't shadow the original cause) and returns cause unchanged.
// Every early return out of processTask's iteration loop must go through
// this: leaving a task at IN_PROGRESS after an error would make the next
// sync's crash-recovery logic (meant to resume work a killed process left
// unfinished) re-queue it forever within this same, still-running process —
// exactly the tight retry loop that motivated adding this helper.
func (o *Orchestrator) failTask(specFile string, cause error) error {
	if err := specs.SetStatus(o.Layout, specFile, specs.StatusFailed); err != nil {
		o.Log.Printf("task %s: also failed to record FAILED status: %v", specFile, err)
	}
	return cause
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
