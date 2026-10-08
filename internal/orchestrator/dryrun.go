package orchestrator

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"diadoche/internal/adr"
	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/onfail"
	"diadoche/internal/specs"
)

// dryRun is Run in --dry-run: a read-only walk of the queue. For every
// pending task, in queue order, it sends the iteration-1 planner, worker
// and tester prompts through o.Runner (agents.DryRunRunner in production,
// which prints them), with labelled placeholders where a real input (the
// plan, the diff, the relevant-ADRs block) doesn't exist.
//
// It changes nothing: no STATUS.md, QUEUE.md or spec/ file is written (not
// even the spec directory or its lock), no usage.jsonl, run-state.json or
// run artifacts, and git is never touched. No status changes, no plan
// validation, no failure counting. The --on-task-done hooks and the --adr
// step are only listed per task, never run: adr-writer is not called. The
// --on-fail chain is only named: no policy is applied and no status
// changes. No answer is inspected for NEEDS_HUMAN and nothing writes
// QUESTIONS.md; with --resume-waiting the WAITING tasks are walked too
// (their prompts carry their questions and answers), but they stay
// WAITING.
//
// Like a real run it first validates the task dependency graph (see
// specs.ValidateGraph): a dangling Depends-on reference or a cycle is
// reported and returned as the error, and no agent is called. The queue is
// walked in the same dependency order a real run uses.
func (o *Orchestrator) dryRun(ctx context.Context) error {
	if err := specs.ValidateGraph(o.Layout); err != nil {
		o.Log.Printf("dry-run: %v", err)
		return fmt.Errorf("dry-run: %w", err)
	}
	planner, worker, tester, err := o.loadAgents()
	if err != nil {
		return err
	}

	queue, err := specs.PendingQueue(o.Layout)
	if err != nil {
		return fmt.Errorf("dry-run: read queue: %w", err)
	}
	var waiting []string
	if o.Cfg.ResumeWaiting {
		waiting, err = specs.WaitingSpecs(o.Layout)
		if err != nil {
			return fmt.Errorf("dry-run: read waiting tasks: %w", err)
		}
		if len(waiting) > 0 {
			o.Log.Printf("dry-run: would resume WAITING task(s): %s", strings.Join(waiting, ", "))
			queue = mergeByTaskNumber(queue, waiting)
		} else {
			o.Log.Print("dry-run: --resume-waiting: no WAITING tasks")
		}
	}
	if len(queue) == 0 {
		o.Log.Print("dry-run: no pending tasks")
	} else {
		o.Log.Printf("dry-run: %d pending task(s) in queue order: %s", len(queue), strings.Join(queue, ", "))
	}
	// offline() keeps o.parallel() at 1 in a dry run: read the flags.
	if n := o.Cfg.Parallel; n > 1 && o.Cfg.Worktree && len(queue) > 0 {
		if err := o.dryRunWaves(queue, waiting, n); err != nil {
			return err
		}
	}
	if o.Cfg.Worktree {
		o.Log.Printf("dry-run: a real run would run each task in its own git worktree under %s; the dry run shows %s as the work dir",
			o.relPath(filepath.Join(o.Layout.Root, worktreesDirName)), o.Cfg.Dir)
	}
	if o.Cfg.ADR {
		o.Log.Printf("dry-run: --adr: adr-writer (%s) would turn each DONE task's decisions into %s/", o.adrWriter.ModelDescription(), adr.Dir)
	}
	if len(o.Cfg.OnTaskDone) > 0 {
		o.Log.Printf("dry-run: on-task-done hooks: %d, timeout %s each", len(o.Cfg.OnTaskDone), cmp.Or(o.Cfg.HookTimeout, config.DefaultHookTimeout))
	}
	if len(o.Cfg.OnFail) > 0 {
		o.Log.Printf("dry-run: a task not DONE after --ITER would get --on-fail %s (escalation model %s); the dry run applies none",
			strings.Join(o.Cfg.OnFail, ","), cmp.Or(o.Cfg.EscalationModel.Value, onfail.DefaultEscalationModel))
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
		questionsBlock := o.questionsBlock(specFile)

		o.Log.Printf("=== [dry-run] task %s (iteration 1/%d) ===", specFile, o.Cfg.Iter)
		calls := []struct {
			agent  *agents.Agent
			prompt string
		}{
			{planner, plannerPrompt(specFile, specContent, "", implPlanPath, questionsBlock)},
			{worker, workerPrompt(specFile, specContent, implPlanFile, plan, "", questionsBlock)},
			{tester, testerPrompt(testerPromptInput{
				specFile: specFile, specContent: specContent, implPlanFile: implPlanFile, plan: plan,
				testCommand: dryRunTestCommand, diff: dryRunDiff, guidance: normalDiffGuidance,
				adrs: dryRunADRs, reviewPath: reviewPath, statusPath: o.Layout.StatusPath,
			})},
		}
		for _, c := range calls {
			if _, err := o.Runner.Run(ctx, o.Cfg.Dir, c.agent, c.prompt); err != nil {
				return fmt.Errorf("dry-run: %s for %s: %w", c.agent.Name, specFile, err)
			}
		}
		if o.Cfg.ADR {
			o.Log.Printf("[dry-run] once %s is DONE and merged, would run the ADR step", taskID(specFile, num))
		}
		for i, command := range o.Cfg.OnTaskDone {
			o.Log.Printf("[dry-run] once %s is DONE and merged, would run on-task-done hook %d: %s", taskID(specFile, num), i+1, command)
		}
	}

	o.Log.Print("dry-run: no state was changed")
	return nil
}

// dryRunWaves reports which tasks of queue a --parallel n run could run
// concurrently: wave 1 are the tasks with no pending predecessor in the
// dependency graph, wave k+1 those whose latest predecessor is in wave k.
// waiting are WAITING tasks --resume-waiting would queue. The merge order
// depends on which task finishes first, which a dry run can't know.
func (o *Orchestrator) dryRunWaves(queue, waiting []string, n int) error {
	preds, err := specs.PendingPredecessors(o.Layout, waiting)
	if err != nil {
		return fmt.Errorf("dry-run: read the dependency graph: %w", err)
	}
	wave := map[string]int{}
	var waveOf func(spec string, depth int) int
	waveOf = func(spec string, depth int) int {
		if w, ok := wave[spec]; ok {
			return w
		}
		w := 1
		if depth <= len(queue) {
			for _, p := range preds[spec] {
				if slices.Contains(queue, p) {
					w = max(w, waveOf(p, depth+1)+1)
				}
			}
		}
		wave[spec] = w
		return w
	}
	var waves [][]string
	for _, spec := range queue {
		w := waveOf(spec, 0)
		for len(waves) < w {
			waves = append(waves, nil)
		}
		waves[w-1] = append(waves[w-1], spec)
	}
	o.Log.Printf("dry-run: --parallel %d: tasks that could run concurrently (up to %d at a time):", n, n)
	for i, ws := range waves {
		o.Log.Printf("dry-run:   wave %d: %s", i+1, strings.Join(ws, ", "))
	}
	o.Log.Print("dry-run: merges would happen one at a time in the order tasks finish, which a dry run cannot know")
	return nil
}

// mergeByTaskNumber adds extra to queue, every spec sorted by ascending
// task number (a number that isn't an integer sorts last), stably.
func mergeByTaskNumber(queue, extra []string) []string {
	out := append(slices.Clone(queue), extra...)
	key := func(spec string) int {
		n, err := strconv.Atoi(specs.TaskNumber(spec))
		if err != nil {
			return int(^uint(0) >> 1)
		}
		return n
	}
	slices.SortStableFunc(out, func(a, b string) int { return cmp.Compare(key(a), key(b)) })
	return out
}
