package orchestrator

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"diadoche/internal/onfail"
	"diadoche/internal/preprocessor"
	"diadoche/internal/runlog"
	"diadoche/internal/speclock"
	"diadoche/internal/specs"
)

// --on-fail split hands a task that ran out of iterations to the
// preprocessor agent — the one --preprocess uses, with its split-mode
// prompt (see preprocessor.SplitPrompt) — to be cut into 2–4 smaller tasks.
// splitPolicy makes the call and checks the new spec files; processTask
// then finalizes the split with splitTask: the original's changes are set
// aside, it becomes BLOCKED with a reason naming the new specs, and those
// are queued in its place.

// loadSplitAgent builds o.splitter, the preprocessor agent on
// --preprocessor-model, with the run's shared prompt prefix.
func (o *Orchestrator) loadSplitAgent(prefix string) error {
	a, err := preprocessor.LoadAgent(o.Layout, o.Cfg.PreprocessorModel)
	if err != nil {
		return fmt.Errorf("--on-fail split: %w", err)
	}
	a.Prefix = prefix
	o.splitter = a
	return nil
}

// splitPolicy asks the preprocessor to cut the task into smaller tasks,
// giving it the task's spec and every review of it. The call is made in
// the main tree, holding the spec lock, like --preprocess; it is logged
// and recorded in usage.jsonl like any agent call. At least two new spec
// files, numbered after every spec that existed before, make outcome
// split; they get a Split-from line naming the task. Anything else —
// the call failed, wrote no file or one, or numbered them wrong — is a
// no_op that removes whatever the call created and hands the task to the
// next policy. A resumable error (signal, usage limit, --max-cost-per-run)
// is interrupted, --max-cost-per-task an error, as for escalate.
func splitPolicy(o *Orchestrator, ctx context.Context, in onFailInput) policyResult {
	noop := func(reason string, err error) policyResult {
		msg := fmt.Sprintf("task %s: warning: --on-fail split did nothing (%s)", in.specFile, reason)
		if err != nil {
			msg += ": " + err.Error()
		}
		o.Log.Print(msg + "; trying the next policy")
		return policyResult{outcome: outcomeNoop, detail: "reason=" + reason}
	}
	if o.splitter == nil {
		if err := o.loadSplitAgent(in.planner.Prefix); err != nil {
			return noop("no_preprocessor", err)
		}
	}

	specContent, err := os.ReadFile(filepath.Join(o.Layout.SpecDir, in.specFile))
	if err != nil {
		return noop("read_spec", err)
	}
	reviews := taskReviews(o.Layout.SpecDir, in.num)

	// The spec-locked section writes new spec files into the spec
	// directory, so in a parallel run the merge owner runs it.
	var res policyResult
	if !o.onOwner(ctx, func() { res = o.splitLocked(ctx, in, specContent, reviews, noop) }) {
		return policyResult{outcome: outcomeInterrupted, cause: ErrInterrupted}
	}
	return res
}

// splitLocked is splitPolicy's spec-locked section: the preprocessor's
// call and the check of the spec files it wrote.
func (o *Orchestrator) splitLocked(ctx context.Context, in onFailInput, specContent []byte, reviews []preprocessor.Review, noop func(string, error) policyResult) policyResult {
	lock, err := speclock.Acquire(o.Layout.SpecDir, lockTimeout)
	if err != nil {
		return noop("spec_lock", err)
	}
	defer lock.Release()
	before, err := specs.DiscoverTaskFiles(o.Layout.SpecDir)
	if err != nil {
		return noop("list_specs", err)
	}
	prompt := preprocessor.SplitPrompt(preprocessor.SplitRequest{
		SpecFile: in.specFile, SpecContent: string(specContent), SpecDir: o.Layout.SpecDir,
		Iterations: len(reviews), Reviews: reviews, Existing: before,
	})

	o.Log.Printf("task %s: --on-fail split: asking the preprocessor to cut it into smaller tasks (%d review(s))", in.specFile, len(reviews))
	call := agentCall{specFile: in.specFile, num: in.num, onFail: onfail.Split, workDir: o.Cfg.Dir, uw: in.uw, tally: &iterationTally{}}
	result, callErr := o.runAgentPausing(ctx, call, o.splitter, prompt)
	var paused *pauseInterruptedError
	if callErr != nil && ctx.Err() != nil && !errors.As(callErr, &paused) {
		callErr = fmt.Errorf("preprocessor call stopped: %w", ErrInterrupted)
	}

	after, listErr := specs.DiscoverTaskFiles(o.Layout.SpecDir)
	created := preprocessor.NewSpecs(before, after)
	fail := func(r policyResult) policyResult {
		for _, f := range created {
			if err := os.Remove(filepath.Join(o.Layout.SpecDir, f)); err != nil {
				o.Log.Printf("task %s: couldn't remove %s, written by the failed split: %v", in.specFile, f, err)
			} else {
				o.Log.Printf("task %s: removed %s, written by the failed split", in.specFile, f)
			}
		}
		return r
	}

	if err := o.checkBudgets(in.specFile, in.uw); err != nil {
		if resumable(ctx, err) {
			return fail(policyResult{outcome: outcomeInterrupted, cause: err})
		}
		return fail(policyResult{outcome: outcomeError, cause: err})
	}
	switch {
	case callErr != nil && resumable(ctx, callErr):
		return fail(policyResult{outcome: outcomeInterrupted, cause: callErr})
	case callErr != nil:
		return fail(noop("preprocessor_failed", callErr))
	}
	if err := o.checkCallCost("preprocessor", result); err != nil {
		return fail(noop("call_cost", err))
	}
	if listErr != nil {
		return fail(noop("list_specs", listErr))
	}
	switch len(created) {
	case 0:
		return fail(noop("no_new_specs", nil))
	case 1:
		return fail(noop("single_spec", fmt.Errorf("only %s", created[0])))
	}
	if err := checkSplitNumbers(before, created); err != nil {
		return fail(noop("bad_numbering", err))
	}

	for _, f := range created {
		if err := specs.AppendSplitFrom(filepath.Join(o.Layout.SpecDir, f), in.specFile); err != nil {
			return fail(noop("split_from", err))
		}
	}
	o.Log.Printf("task %s: --on-fail split: the preprocessor wrote %s", in.specFile, strings.Join(created, ", "))
	return policyResult{outcome: outcomeSplit, detail: "into=" + strings.Join(created, ","), split: created}
}

// checkSplitNumbers makes sure every spec in created has an integer number
// above every number in before, and no two share one.
func checkSplitNumbers(before, created []string) error {
	highest := 0
	for _, f := range before {
		if n, err := strconv.Atoi(specs.TaskNumber(f)); err == nil && n > highest {
			highest = n
		}
	}
	seen := map[int]string{}
	for _, f := range created {
		n, err := strconv.Atoi(specs.TaskNumber(f))
		switch {
		case err != nil:
			return fmt.Errorf("%s has no task number", f)
		case n <= highest:
			return fmt.Errorf("%s is not numbered after the existing specs (highest %d)", f, highest)
		case seen[n] != "":
			return fmt.Errorf("%s and %s share number %d", seen[n], f, n)
		}
		seen[n] = f
	}
	return nil
}

// taskReviews reads every spec/<num>_REVIEW_<M>.md, ordered by M.
func taskReviews(specDir, num string) []preprocessor.Review {
	matches, _ := filepath.Glob(filepath.Join(specDir, num+"_REVIEW_*.md"))
	type numbered struct {
		n    int
		path string
	}
	var found []numbered
	for _, m := range matches {
		sub := reviewRe.FindStringSubmatch(m)
		if sub == nil {
			continue
		}
		if n, err := strconv.Atoi(sub[1]); err == nil {
			found = append(found, numbered{n, m})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].n < found[j].n })
	var out []preprocessor.Review
	for _, f := range found {
		b, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		out = append(out, preprocessor.Review{Name: filepath.Base(f.path), Content: string(b)})
	}
	return out
}

// splitTask finalizes a task --on-fail split cut into parts: its changes
// are set aside so the parts start from a clean base (with --worktree
// committed on its branch, the worktree removed and the branch renamed to
// diadoche/split/<NNN>-<slug>; in place parked on that branch), it becomes
// BLOCKED with a reason naming the parts, the parts are queued in its
// place (see specs.ApplySplit), and spec/<num>_BLOCKED.md explains it all.
// Tasks depending on it are not blocked: the parts do its work. The
// returned error means the changes couldn't be preserved, after which the
// next task can't safely start.
func (o *Orchestrator) splitTask(ctx context.Context, specFile, num string, ws *workspace, applied []appliedPolicy, parts []string) error {
	var where blockedChanges
	if ws != nil {
		message := "diadoche: parked split " + specFile
		if ws.worktree {
			branch, stat, restorable, err := o.retireWorktree(ctx, specFile, num, ws, splitBranchPrefix, message)
			if err != nil {
				return err
			}
			where = blockedChanges{branch: branch, stat: stat, restorable: restorable, retired: true}
		} else {
			branch, stat, restorable, err := o.parkChanges(ctx, specFile, num, ws, splitBranchPrefix, message)
			if err != nil {
				return err
			}
			where = blockedChanges{branch: branch, stat: stat, restorable: restorable}
		}
	}

	reportPath := filepath.Join(o.Layout.SpecDir, num+"_BLOCKED.md")
	iters := o.Cfg.Iter
	names := make([]string, len(applied))
	for i, p := range applied {
		names[i] = p.name
		if p.name == onfail.Escalate {
			iters++
		}
	}
	reason := fmt.Sprintf("split into %s after %d iteration(s); on-fail policies applied: %s (see %s)",
		strings.Join(parts, ", "), iters, strings.Join(names, ", "), o.relPath(reportPath))

	var queue []string
	lock, err := speclock.Acquire(o.Layout.SpecDir, lockTimeout)
	if err == nil {
		sh := o.shared()
		sh.specMu.Lock()
		queue, err = specs.ApplySplit(o.Layout, specFile, reason, parts)
		sh.specMu.Unlock()
		lock.Release()
	}
	if err != nil {
		// The parts are on disk, so the next sync still queues them.
		o.Log.Printf("task %s: couldn't queue the split in STATUS.md/QUEUE.md: %v", specFile, err)
		if err := o.setStatusReason(specFile, specs.StatusBlocked, reason); err != nil {
			o.Log.Printf("task %s: also failed to record BLOCKED status: %v", specFile, err)
		}
	}
	o.event(specFile, 0, "", "status", runlog.F("status", specs.StatusBlocked), runlog.F("reason", "split"))
	o.event(specFile, 0, "", "split", runlog.F("into", strings.Join(parts, ",")), runlog.F("branch", cmp.Or(where.branch, "none")))
	o.Log.Printf("task %s: BLOCKED: %s", specFile, reason)
	o.Log.Printf("task %s: %s queued next, in its place", specFile, strings.Join(parts, ", "))
	if where.branch != "" {
		o.Log.Printf("task %s: its changes are on branch %s", specFile, where.branch)
	}

	if err := os.WriteFile(reportPath, []byte(o.splitReport(specFile, num, reason, applied, parts, queue, where, ws != nil)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", reportPath, err)
	}
	if ws != nil {
		ws.removeBaseFile()
		o.commitBookkeeping(ctx, specFile, "diadoche: split "+specFile)
	}
	return nil
}

// splitReport renders spec/<num>_BLOCKED.md for a task --on-fail split
// cut into parts; queue is the queue right after the split.
func (o *Orchestrator) splitReport(specFile, num, reason string, applied []appliedPolicy, parts, queue []string, where blockedChanges, hasWorkspace bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: BLOCKED (split)\n\n", specFile)
	fmt.Fprintf(&b, "Reason: %s\n\n", reason)

	b.WriteString("On-fail policies applied, in order:\n")
	for i, p := range applied {
		fmt.Fprintf(&b, "%d. %s: %s", i+1, p.name, p.outcome)
		if p.detail != "" {
			fmt.Fprintf(&b, " (%s)", p.detail)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("It was split into these tasks, which run next, in place of this task, from a clean base:\n")
	for _, p := range parts {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	b.WriteString("\n")
	if len(queue) > 0 {
		fmt.Fprintf(&b, "Queue after the split: %s\n\n", strings.Join(queue, ", "))
	}

	if reviews := taskReviews(o.Layout.SpecDir, num); len(reviews) > 0 {
		b.WriteString("Reviews the split was based on:\n")
		for _, r := range reviews {
			fmt.Fprintf(&b, "- %s\n", o.relPath(filepath.Join(o.Layout.SpecDir, r.Name)))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Reviews the split was based on: none\n\n")
	}

	writeChanges(&b, where, hasWorkspace)

	b.WriteString("To resume: nothing to do — the split-off tasks carry on. To retry this task instead, set its row in STATUS.md back to QUEUED " +
		"and the split-off tasks' rows to something that isn't pending (e.g. BLOCKED), and remove their Split-from lines if you keep them.\n")
	return b.String()
}
