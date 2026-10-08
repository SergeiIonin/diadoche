package orchestrator

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"diadoche/internal/agents"
	"diadoche/internal/onfail"
	"diadoche/internal/runlog"
	"diadoche/internal/runstate"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
	"diadoche/internal/usagelimit"
)

// The --on-fail chain (see package onfail) runs once a task's --ITER
// iterations are exhausted without a DONE verdict, and only then: a task
// cut short by an agent error, --max-call-cost or --max-cost-per-task is
// FAILED as before, and the resumable outcomes (a signal, a usage limit,
// --max-cost-per-run) leave it pending. Policies only decide what happens;
// processTask finalizes the outcome (DONE -> commit/merge and hooks,
// BLOCKED -> blockTask, split -> splitTask, FAILED -> failTask), so the
// task's final.diff is still written once, after the chain and before
// anything is committed or parked. The split policy's own agent call (the
// preprocessor's) is the one exception to "policies only decide": its new
// spec files are written during the chain, see split.go.

// Outcomes of one applied policy, as recorded in run.log.
const (
	outcomeDone        = "done"
	outcomeNotDone     = "not_done"
	outcomeBlocked     = "blocked"
	outcomeError       = "error"
	outcomeInterrupted = "interrupted"
	outcomeSplit       = "split"
	outcomeNoop        = "no_op"
)

// failPolicy applies one --on-fail policy to a task.
type failPolicy func(o *Orchestrator, ctx context.Context, in onFailInput) policyResult

// policyResult is what one policy did. outcome is one of the outcome*
// constants; detail is a short key=value note for run.log and the BLOCKED
// report; a non-nil cause (outcome error or interrupted) ends the chain and
// is handled by processTask like the same error from a normal iteration.
// split are the new specs of outcome split, in queue order.
type policyResult struct {
	outcome, detail string
	cause           error
	split           []string
}

// failPolicies are the implementations of onfail.Names. Adding a policy
// means adding its name there and its implementation here.
var failPolicies = map[string]failPolicy{
	onfail.Escalate: escalatePolicy,
	onfail.Split:    splitPolicy,
	onfail.Block:    blockPolicy,
}

// onFailInput is the task the chain is applied to.
type onFailInput struct {
	specFile, num           string
	ws                      *workspace
	planner, worker, tester *agents.Agent
	uw                      *usage.Writer
}

// appliedPolicy is one policy the chain applied, in order.
type appliedPolicy struct {
	name, outcome, detail string
}

// onFailResult is what the chain decided: done (an escalated iteration got
// DONE), blocked (the block policy ran), split (the split policy cut the
// task into these specs), or none of them — the task FAILS, with cause
// when a policy was cut short.
type onFailResult struct {
	done, blocked bool
	split         []string
	cause         error
	applied       []appliedPolicy
}

// loadEscalationAgents builds the planner and worker of an escalated
// iteration: copies of planner and worker on o.Cfg.EscalationModel (the
// default escalation model when the Config was built by hand).
func (o *Orchestrator) loadEscalationAgents(planner, worker *agents.Agent) error {
	ov := o.Cfg.EscalationModel
	if !ov.IsSet() {
		ov = agents.ModelOverride{Value: onfail.DefaultEscalationModel, Source: agents.ModelSourceDefault}
	}
	p, w := *planner, *worker
	if err := agents.ResolveModel(&p, ov); err != nil {
		return fmt.Errorf("--escalation-model: %w", err)
	}
	if err := agents.ResolveModel(&w, ov); err != nil {
		return fmt.Errorf("--escalation-model: %w", err)
	}
	o.escPlanner, o.escWorker = &p, &w
	return nil
}

// applyOnFail applies o.Cfg.OnFail in order to a task whose iterations ran
// out, recording every applied policy as an on_fail_policy event, until
// one of them ends the task or fails; a policy that did nothing (no_op, a
// split that failed) hands the task on to the next. A chain that runs out
// leaves the task to FAIL.
func (o *Orchestrator) applyOnFail(ctx context.Context, in onFailInput) onFailResult {
	o.Log.Printf("task %s: not DONE after %d iteration(s); applying --on-fail %s", in.specFile, o.Cfg.Iter, strings.Join(o.Cfg.OnFail, ","))
	var res onFailResult
	for i, name := range o.Cfg.OnFail {
		if o.stopRequested() {
			res.cause = ErrInterrupted
			return res
		}
		policy := failPolicies[name]
		var r policyResult
		if policy == nil {
			// config.Parse rejects unknown names; a hand-built Config may not.
			r = policyResult{outcome: outcomeError, cause: fmt.Errorf("--on-fail: unknown policy %q", name)}
		} else {
			r = policy(o, ctx, in)
		}
		outcome, detail, cause := r.outcome, r.detail, r.cause
		res.applied = append(res.applied, appliedPolicy{name: name, outcome: outcome, detail: detail})
		fields := []runlog.Field{runlog.F("policy", name), runlog.F("step", i+1), runlog.F("outcome", outcome)}
		if detail != "" {
			fields = append(fields, runlog.F("detail", detail))
		}
		o.event(in.specFile, 0, "", "on_fail_policy", fields...)
		o.Log.Printf("task %s: --on-fail %s: %s", in.specFile, name, outcome)
		switch {
		case outcome == outcomeDone:
			res.done = true
			return res
		case outcome == outcomeBlocked:
			res.blocked = true
			return res
		case outcome == outcomeSplit:
			res.split = r.split
			return res
		case cause != nil:
			res.cause = cause
			return res
		}
	}
	return res
}

// escalatePolicy runs one more complete planner -> worker -> tester
// iteration, numbered --ITER+1, with planner and worker on the escalation
// model and tester on its own. It is a normal iteration in every other
// respect: same inputs (the last review included), plan check, red-green
// check, limit pauses, budgets and records.
func escalatePolicy(o *Orchestrator, ctx context.Context, in onFailInput) policyResult {
	if o.escPlanner == nil || o.escWorker == nil {
		if err := o.loadEscalationAgents(in.planner, in.worker); err != nil {
			return policyResult{outcome: outcomeError, cause: err}
		}
	}
	iter := o.Cfg.Iter + 1
	detail := fmt.Sprintf("model=%s iter=%d", o.escPlanner.Model, iter)
	specContent, err := os.ReadFile(filepath.Join(o.Layout.SpecDir, in.specFile))
	if err != nil {
		return policyResult{outcome: outcomeError, detail: detail, cause: fmt.Errorf("read spec file: %w", err)}
	}
	o.setAgentState(iter, runstate.Agents[0], runstate.AgentPending)
	done, err := o.runIteration(ctx, iterationInput{
		specFile: in.specFile, num: in.num, specContent: specContent, ws: in.ws, iter: iter,
		escalated: true, planner: o.escPlanner, worker: o.escWorker, tester: in.tester, uw: in.uw,
	})
	switch {
	case done:
		reason := fmt.Sprintf("on-fail: escalate succeeded (planner/worker on %s)", o.escPlanner.Model)
		if err := specs.SetStatusReason(o.Layout, in.specFile, specs.StatusDone, reason); err != nil {
			o.Log.Printf("task %s: couldn't record the escalation in STATUS.md: %v", in.specFile, err)
		}
		return policyResult{outcome: outcomeDone, detail: detail}
	case err != nil && resumable(ctx, err):
		return policyResult{outcome: outcomeInterrupted, detail: detail, cause: err}
	case err != nil:
		return policyResult{outcome: outcomeError, detail: detail, cause: err}
	}
	return policyResult{outcome: outcomeNotDone, detail: detail}
}

// resumable reports whether err leaves the task pending rather than
// FAILED (see processTask): a signal, a pause interrupted by one, a usage
// limit or --max-cost-per-run.
func resumable(ctx context.Context, err error) bool {
	var paused *pauseInterruptedError
	return errors.As(err, &paused) || errors.Is(err, ErrInterrupted) || ctx.Err() != nil ||
		errors.Is(err, ErrRunBudgetExceeded) || usagelimit.Mentions(err.Error())
}

// blockPolicy decides the task is BLOCKED; processTask carries it out
// (see blockTask), after the task's final.diff is written.
func blockPolicy(*Orchestrator, context.Context, onFailInput) policyResult {
	return policyResult{outcome: outcomeBlocked}
}

// blockTask finalizes a task the --on-fail chain BLOCKED: STATUS.md says
// so with the policies applied, its changes are preserved (with --worktree
// committed on its branch, worktree and branch kept, like a merge
// conflict; in place parked on diadoche/blocked/<NNN>-<slug>, as the next
// task needs a clean tree, but without NNN_FAILED.md), every task that
// depends on it is BLOCKED too, and spec/<num>_BLOCKED.md explains it all.
// The returned error means the changes couldn't be preserved, after which
// the next task can't safely start.
func (o *Orchestrator) blockTask(ctx context.Context, specFile, num string, ws *workspace, applied []appliedPolicy) error {
	reportPath := filepath.Join(o.Layout.SpecDir, num+"_BLOCKED.md")
	names := make([]string, len(applied))
	for i, p := range applied {
		names[i] = p.name
	}
	reason := fmt.Sprintf("not DONE after %d iteration(s); on-fail policies applied: %s (see %s)",
		o.Cfg.Iter, strings.Join(names, ", "), o.relPath(reportPath))
	if err := specs.SetStatusReason(o.Layout, specFile, specs.StatusBlocked, reason); err != nil {
		o.Log.Printf("task %s: also failed to record BLOCKED status: %v", specFile, err)
	} else {
		o.event(specFile, 0, "", "status", runlog.F("status", specs.StatusBlocked), runlog.F("reason", "on_fail"))
	}
	o.Log.Printf("task %s: BLOCKED: %s", specFile, reason)

	var where blockedChanges
	if ws != nil {
		if ws.worktree {
			stat, err := o.blockWorktree(ctx, specFile, ws)
			if err != nil {
				return err
			}
			where = blockedChanges{branch: ws.branch, worktree: o.relPath(ws.dir), stat: stat}
			o.Log.Printf("task %s: its worktree %s and branch %s are kept", specFile, where.worktree, ws.branch)
			o.event(specFile, 0, "", "block_preserved", runlog.F("branch", ws.branch), runlog.F("worktree", where.worktree))
		} else {
			branch, stat, restorable, err := o.parkChanges(ctx, specFile, num, ws, blockedBranchPrefix, "diadoche: parked BLOCKED "+specFile)
			if err != nil {
				return err
			}
			where = blockedChanges{branch: branch, stat: stat, restorable: restorable}
			o.event(specFile, 0, "", "block_preserved", runlog.F("branch", cmp.Or(branch, "none")))
		}
	}

	deps, err := specs.BlockDependents(o.Layout, specFile)
	if err != nil {
		o.Log.Printf("task %s: couldn't block the tasks that depend on it: %v", specFile, err)
	}
	for _, d := range deps {
		o.Log.Printf("task %s: BLOCKED: %s", d.Spec, d.Reason)
		o.event(d.Spec, 0, "", "status", runlog.F("status", specs.StatusBlocked), runlog.F("reason", "dependency"),
			runlog.F("blocked_by", blockerOf(d.Reason, specFile)))
	}

	if err := os.WriteFile(reportPath, []byte(o.blockedReport(specFile, num, reason, applied, where, ws != nil, deps)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", reportPath, err)
	}
	if ws != nil {
		ws.removeBaseFile()
		o.commitBookkeeping(ctx, specFile, "diadoche: BLOCKED "+specFile)
	}
	return nil
}

// blockerOf is the spec a dependency-BLOCKED row's reason names (its
// direct blocker), or fallback.
func blockerOf(reason, fallback string) string {
	if rest, ok := strings.CutPrefix(reason, "depends on "); ok {
		if spec, _, ok := strings.Cut(rest, ","); ok {
			return spec
		}
	}
	return fallback
}

// blockedChanges is where a BLOCKED task's changes are: its kept worktree
// and branch, its branch renamed after its worktree was removed (retired,
// --on-fail split with --worktree), or (in place) the branch they were
// parked on; branch is "" when there were none.
type blockedChanges struct {
	branch, worktree, stat string
	retired                bool
	restorable             []string
}

// blockedReport renders spec/<num>_BLOCKED.md.
func (o *Orchestrator) blockedReport(specFile, num, reason string, applied []appliedPolicy, where blockedChanges, hasWorkspace bool, deps []specs.Row) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: BLOCKED\n\n", specFile)
	fmt.Fprintf(&b, "Reason: %s\n\n", reason)

	fmt.Fprintf(&b, "Iterations: %d regular", o.Cfg.Iter)
	for _, p := range applied {
		if p.name == onfail.Escalate {
			fmt.Fprintf(&b, ", plus escalated iteration %d", o.Cfg.Iter+1)
			if o.escPlanner != nil {
				fmt.Fprintf(&b, " (planner/worker on %s)", o.escPlanner.Model)
			}
		}
	}
	b.WriteString(".\n\n")

	b.WriteString("On-fail policies applied, in order:\n")
	for i, p := range applied {
		fmt.Fprintf(&b, "%d. %s: %s", i+1, p.name, p.outcome)
		if p.detail != "" {
			fmt.Fprintf(&b, " (%s)", p.detail)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")

	if review := lastReview(o.Layout.SpecDir, num); review != "" {
		fmt.Fprintf(&b, "Last review: %s\n\n", o.relPath(filepath.Join(o.Layout.SpecDir, review)))
	} else {
		b.WriteString("Last review: none\n\n")
	}

	writeChanges(&b, where, hasWorkspace)

	if len(deps) > 0 {
		b.WriteString("Blocked along with it (they depend on it):\n")
		for _, d := range deps {
			fmt.Fprintf(&b, "- %s: %s\n", d.Spec, d.Reason)
		}
		b.WriteString("\n")
	}

	b.WriteString("To resume: fix what the last review points out (or the spec), then set the task's row in STATUS.md back to QUEUED")
	if where.worktree != "" {
		b.WriteString("; the next run continues in the kept worktree")
	}
	b.WriteString(". Tasks blocked along with it need the same.\n")
	return b.String()
}

// writeChanges is the "where are the task's changes" paragraph of a
// BLOCKED report.
func writeChanges(b *strings.Builder, where blockedChanges, hasWorkspace bool) {
	switch {
	case !hasWorkspace:
		b.WriteString("No changes were preserved (simulated run).\n\n")
		return
	case where.worktree != "":
		fmt.Fprintf(b, "The task's changes are committed on its branch `%s`; its worktree %s is kept.\n\n", where.branch, where.worktree)
		if strings.TrimSpace(where.stat) != "" {
			fmt.Fprintf(b, "```\n%s```\n\n", where.stat)
		}
		return
	case where.retired && strings.TrimSpace(where.stat) == "":
		fmt.Fprintf(b, "The task made no code changes; its branch was kept as `%s` anyway and its worktree was removed.\n\n", where.branch)
		return
	case where.retired:
		fmt.Fprintf(b, "The task's changes were committed on its branch, renamed to `%s`; its worktree was removed and the main working tree was never touched.\n\n", where.branch)
	case where.branch == "":
		b.WriteString("The task made no code changes, so no branch was created.\n\n")
		return
	default:
		fmt.Fprintf(b, "Code changes were parked on branch `%s`; the working tree was reverted to the task's starting point.\n\n", where.branch)
	}
	fmt.Fprintf(b, "```\n%s```\n\n", where.stat)
	if len(where.restorable) > 0 && len(where.restorable) <= 20 {
		fmt.Fprintf(b, "To bring the changes back: `git checkout %s -- %s` (or `git merge %s`, which also replays deletions).\n\n",
			where.branch, strings.Join(where.restorable, " "), where.branch)
	} else {
		fmt.Fprintf(b, "To bring the changes back: `git merge %s`.\n\n", where.branch)
	}
}
