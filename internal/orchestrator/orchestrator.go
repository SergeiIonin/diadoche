// Package orchestrator runs entelecheia's main loop: sync the spec queue,
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

	"entelecheia/internal/agents"
	"entelecheia/internal/config"
	"entelecheia/internal/skills"
	"entelecheia/internal/speclock"
	"entelecheia/internal/specs"
)

// lockTimeout bounds how long the main loop waits for the spec directory
// lock (e.g. held by a concurrent --preprocess run) before giving up.
const lockTimeout = 2 * time.Minute

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

	matchedSkills, err := skills.Resolve(o.Layout.SkillsDir, o.Cfg.Langs, o.Cfg.Frameworks)
	if err != nil {
		return fmt.Errorf("resolve skills: %w", err)
	}
	if len(matchedSkills) > 0 {
		o.Log.Printf("skills in scope: %s", strings.Join(matchedSkills, ", "))
	}

	planner, err := agents.Load(filepath.Join(o.Layout.AgentsDir, "planner.md"))
	if err != nil {
		return fmt.Errorf("load planner agent: %w", err)
	}
	worker, err := agents.Load(filepath.Join(o.Layout.AgentsDir, "worker.md"))
	if err != nil {
		return fmt.Errorf("load worker agent: %w", err)
	}
	tester, err := agents.Load(filepath.Join(o.Layout.AgentsDir, "tester.md"))
	if err != nil {
		return fmt.Errorf("load tester agent: %w", err)
	}

	for {
		if err := o.syncQueueLocked(); err != nil {
			return fmt.Errorf("sync spec queue: %w", err)
		}

		processedAny := false
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
			}
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
		return fmt.Errorf("read spec file: %w", err)
	}

	skillsNote := ""
	if len(skillNames) > 0 {
		skillsNote = fmt.Sprintf("\n\nRelevant skills available under .claude/skills: %s. Use them where applicable.", strings.Join(skillNames, ", "))
	}

	done := false
	for iter := 1; iter <= o.Cfg.Iter; iter++ {
		o.Log.Printf("--- %s iteration %d/%d ---", specFile, iter, o.Cfg.Iter)

		implPlanFile := fmt.Sprintf("%s_IMPL_PLAN.md", num)
		implPlanPath := filepath.Join(o.Layout.SpecDir, implPlanFile)
		reviewFile := fmt.Sprintf("%s_REVIEW_%d.md", num, iter)
		reviewPath := filepath.Join(o.Layout.SpecDir, reviewFile)

		var priorReview string
		if iter > 1 {
			prevReviewPath := filepath.Join(o.Layout.SpecDir, fmt.Sprintf("%s_REVIEW_%d.md", num, iter-1))
			if b, err := os.ReadFile(prevReviewPath); err == nil {
				priorReview = string(b)
			}
		}

		plannerPrompt := fmt.Sprintf("Task spec (%s):\n%s\n", specFile, specContent)
		if priorReview != "" {
			plannerPrompt += fmt.Sprintf("\nFeedback from the previous review, address every point:\n%s\n", priorReview)
		}
		plannerPrompt += fmt.Sprintf("\nWrite (or update) the implementation plan to %s.%s", implPlanPath, skillsNote)

		if _, err := o.Runner.Run(ctx, o.Cfg.Dir, planner, plannerPrompt); err != nil {
			return fmt.Errorf("planner: %w", err)
		}

		implPlanContent, _ := os.ReadFile(implPlanPath)

		workerPrompt := fmt.Sprintf("Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n", specFile, specContent, implPlanFile, implPlanContent)
		if priorReview != "" {
			workerPrompt += fmt.Sprintf("\nFeedback from the previous review, address every point:\n%s\n", priorReview)
		}
		workerPrompt += "\nImplement the plan in this repository." + skillsNote

		if _, err := o.Runner.Run(ctx, o.Cfg.Dir, worker, workerPrompt); err != nil {
			return fmt.Errorf("worker: %w", err)
		}

		testerPrompt := fmt.Sprintf(
			"Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n\n"+
				"Review the current state of the repository against the spec and plan. "+
				"Write your findings to %s. If, and only if, the task is fully and correctly done, "+
				"update the row for %q in %s to status DONE.",
			specFile, specContent, implPlanFile, implPlanContent, reviewPath, specFile, o.Layout.StatusPath)
		testerPrompt += skillsNote

		if _, err := o.Runner.Run(ctx, o.Cfg.Dir, tester, testerPrompt); err != nil {
			return fmt.Errorf("tester: %w", err)
		}

		status, err := specs.GetStatus(o.Layout, specFile)
		if err != nil {
			return err
		}
		if status == specs.StatusDone {
			done = true
			break
		}
	}

	if done {
		o.Log.Printf("task %s: DONE", specFile)
		return nil
	}

	o.Log.Printf("task %s: FAILED after %d iteration(s)", specFile, o.Cfg.Iter)
	return specs.SetStatus(o.Layout, specFile, specs.StatusFailed)
}
