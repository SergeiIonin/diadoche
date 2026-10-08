package orchestrator

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/specs"
)

// dryRun is Run in --dry-run: a read-only walk of the queue. For every
// pending task, in queue order, it sends the iteration-1 planner, worker
// and tester prompts through o.Runner (agents.DryRunRunner in production,
// which prints them), with labelled placeholders where a real input (the
// plan, the diff) doesn't exist.
//
// It changes nothing: no STATUS.md, QUEUE.md or spec/ file is written (not
// even the spec directory or its lock), no usage.jsonl, run-state.json or
// run artifacts, and git is never touched. No status changes, no plan
// validation, no failure counting. The --on-task-done hooks are only
// listed per task, never run.
func (o *Orchestrator) dryRun(ctx context.Context) error {
	planner, worker, tester, err := o.loadAgents()
	if err != nil {
		return err
	}

	queue, err := specs.PendingQueue(o.Layout)
	if err != nil {
		return fmt.Errorf("dry-run: read queue: %w", err)
	}
	if len(queue) == 0 {
		o.Log.Print("dry-run: no pending tasks")
	} else {
		o.Log.Printf("dry-run: %d pending task(s) in queue order: %s", len(queue), strings.Join(queue, ", "))
	}
	if o.Cfg.Worktree {
		o.Log.Printf("dry-run: a real run would run each task in its own git worktree under %s; the dry run shows %s as the work dir",
			o.relPath(filepath.Join(o.Layout.Root, worktreesDirName)), o.Cfg.Dir)
	}
	if len(o.Cfg.OnTaskDone) > 0 {
		o.Log.Printf("dry-run: on-task-done hooks: %d, timeout %s each", len(o.Cfg.OnTaskDone), cmp.Or(o.Cfg.HookTimeout, config.DefaultHookTimeout))
	}

	for _, specFile := range queue {
		if o.stopRequested() {
			o.Log.Printf("stop requested: exiting before the next task")
			return ErrInterrupted
		}
		num := specs.TaskNumber(specFile)
		specContent, err := os.ReadFile(filepath.Join(o.Layout.SpecDir, specFile))
		if err != nil {
			return fmt.Errorf("dry-run: read spec file %s: %w", specFile, err)
		}
		implPlanFile := fmt.Sprintf("%s_IMPL_PLAN.md", num)
		implPlanPath := filepath.Join(o.Layout.SpecDir, implPlanFile)
		reviewPath := filepath.Join(o.Layout.SpecDir, fmt.Sprintf("%s_REVIEW_1.md", num))
		plan := dryRunPlan(implPlanFile)

		o.Log.Printf("=== [dry-run] task %s (iteration 1/%d) ===", specFile, o.Cfg.Iter)
		calls := []struct {
			agent  *agents.Agent
			prompt string
		}{
			{planner, plannerPrompt(specFile, specContent, "", implPlanPath)},
			{worker, workerPrompt(specFile, specContent, implPlanFile, plan, "")},
			{tester, testerPrompt(testerPromptInput{
				specFile: specFile, specContent: specContent, implPlanFile: implPlanFile, plan: plan,
				testCommand: dryRunTestCommand, diff: dryRunDiff, guidance: normalDiffGuidance,
				reviewPath: reviewPath, statusPath: o.Layout.StatusPath,
			})},
		}
		for _, c := range calls {
			if _, err := o.Runner.Run(ctx, o.Cfg.Dir, c.agent, c.prompt); err != nil {
				return fmt.Errorf("dry-run: %s for %s: %w", c.agent.Name, specFile, err)
			}
		}
		for i, command := range o.Cfg.OnTaskDone {
			o.Log.Printf("[dry-run] once %s is DONE and merged, would run on-task-done hook %d: %s", taskID(specFile, num), i+1, command)
		}
	}

	o.Log.Print("dry-run: no state was changed")
	return nil
}
