package orchestrator

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"diadoche/internal/gitutil"
	"diadoche/internal/questions"
	"diadoche/internal/runlog"
	"diadoche/internal/runstate"
	"diadoche/internal/speclock"
	"diadoche/internal/specs"
)

// An agent that hits a question it can't decide alone (see package
// questions) writes it to QUESTIONS.md and, when the decision isn't
// reversible, puts NEEDS_HUMAN on a line of its own in its final answer.
// That answer sets the task aside as WAITING without spending an
// iteration, much like a usage-limit pause: its run-state.json record is
// kept, exactly as it was before the call, under "waiting"; its changes
// are kept (worktree and branch, or parked on diadoche/waiting/<NNN>-<slug>
// in place); spec/NNN_WAITING.md says where. The run moves on to the next
// independent task — tasks depending on a WAITING one are skipped and stay
// QUEUED. Nothing picks a WAITING task up again until a human answered and
// reran diadoche with --resume-waiting, which re-queues it; it then
// continues at the iteration and agent that asked, and planner and worker
// get the task's questions and answers in their prompts.

// waitingBranchPrefix prefixes the branch a WAITING task's changes are
// parked on with --worktree=false.
const waitingBranchPrefix = "diadoche/waiting/"

// parkedBranchLabel starts the line of spec/NNN_WAITING.md naming the
// branch an in-place task's changes were parked on; read back on resume.
const parkedBranchLabel = "Parked-branch:"

// needsHumanError is a planner or worker answer that ended with the
// NEEDS_HUMAN marker. Its message is diadoche's own wording, never a claude
// limit line.
type needsHumanError struct {
	agent string
	iter  int
}

func (e *needsHumanError) Error() string {
	return fmt.Sprintf("%s needs a human decision (%s)", e.agent, questions.Marker)
}

// asksHuman reports whether agent's answer output asks for a human: only
// planner and worker are told about the marker.
func asksHuman(agent, output string) bool {
	return (agent == "planner" || agent == "worker") && questions.NeedsHuman(output)
}

// taskQuestions are specFile's entries in QUESTIONS.md; an unreadable file
// is a warning and no entries.
func (o *Orchestrator) taskQuestions(specFile string) []questions.Entry {
	entries, err := questions.Load(o.Layout.QuestionsPath)
	if err != nil {
		o.Log.Printf("warning: %s: couldn't read %s: %v", specFile, o.Layout.QuestionsPath, err)
		return nil
	}
	return questions.ForTask(entries, specFile)
}

// questionsBlock is the open-questions part of specFile's planner and
// worker prompts (see questions.PromptBlock).
func (o *Orchestrator) questionsBlock(specFile string) string {
	return questions.PromptBlock(o.questionsPathForPrompt(), o.taskQuestions(specFile))
}

// questionsPathForPrompt is QUESTIONS.md's absolute path: in worktree mode
// the agent runs in the task's worktree, and the questions belong in the
// main repository root.
func (o *Orchestrator) questionsPathForPrompt() string {
	if abs, err := filepath.Abs(o.Layout.QuestionsPath); err == nil {
		return abs
	}
	return o.Layout.QuestionsPath
}

// waitTask sets a task whose agent answered NEEDS_HUMAN aside as WAITING:
// STATUS.md says so (pointing at QUESTIONS.md), its run-state.json record
// moves to the "waiting" map, its changes are preserved (committed on its
// branch with the worktree and NNN_BASE kept, or parked in place) and
// spec/NNN_WAITING.md explains it all. The returned error means the changes
// couldn't be preserved, after which the next task can't safely start.
func (o *Orchestrator) waitTask(ctx context.Context, specFile, num string, ws *workspace, nh *needsHumanError) error {
	reportPath := filepath.Join(o.Layout.SpecDir, num+"_WAITING.md")
	reason := fmt.Sprintf("%s needs a human decision in iteration %d: answer in %s, then rerun with --resume-waiting (see %s)",
		nh.agent, nh.iter, questions.FileName, o.relPath(reportPath))
	if err := o.setStatusReason(specFile, specs.StatusWaiting, reason); err != nil {
		o.Log.Printf("task %s: also failed to record WAITING status: %v", specFile, err)
	} else {
		o.event(specFile, nh.iter, nh.agent, "status", runlog.F("status", specs.StatusWaiting), runlog.F("reason", "needs_human"))
	}
	entries := o.taskQuestions(specFile)
	open := 0
	for _, e := range entries {
		if !e.Answered() {
			open++
		}
	}
	o.event(specFile, nh.iter, nh.agent, "waiting", runlog.F("agent", nh.agent), runlog.F("iteration", nh.iter),
		runlog.F("questions", questions.FileName), runlog.F("open", open))
	if len(entries) == 0 {
		o.Log.Printf("warning: task %s: %s answered %s, but %s has no question for it", specFile, nh.agent, questions.Marker, questions.FileName)
	}

	sh := o.shared()
	sh.mu.Lock()
	if o.stateOn && o.cur != nil {
		rec := *o.cur
		if ws != nil && !ws.worktree {
			// Parked in place: the resumed task starts from a fresh base
			// with the parked changes re-applied on top.
			rec.BaseCommit = ""
		}
		if sh.waiting == nil {
			sh.waiting = map[string]runstate.Task{}
		}
		sh.waiting[specFile] = rec
	}
	o.dropCurLocked()
	o.saveStateLocked()
	sh.mu.Unlock()

	where := "The task made no code changes (simulated run)."
	if ws != nil {
		if ws.worktree {
			stat, err := o.blockWorktree(ctx, specFile, ws, "diadoche: WAITING "+specFile)
			if err != nil {
				return err
			}
			wt := o.relPath(ws.dir)
			where = fmt.Sprintf("The task's changes are committed on its branch `%s`; its worktree %s and its spec/%s_BASE are kept, and --resume-waiting continues there.", ws.branch, wt, num)
			if strings.TrimSpace(stat) != "" {
				where += fmt.Sprintf("\n\n```\n%s```", stat)
			}
			o.Log.Printf("task %s: its worktree %s and branch %s are kept", specFile, wt, ws.branch)
			o.event(specFile, 0, "", "waiting_preserved", runlog.F("branch", ws.branch), runlog.F("worktree", wt))
		} else {
			branch, stat, _, err := o.parkChanges(ctx, specFile, num, ws, waitingBranchPrefix, "diadoche: parked WAITING "+specFile)
			if err != nil {
				return err
			}
			ws.removeBaseFile()
			if branch == "" {
				where = "The task made no code changes, so nothing was parked."
			} else {
				where = fmt.Sprintf("Code changes were parked on branch `%s`; the working tree was reverted, and --resume-waiting brings them back.\n%s %s\n\n```\n%s```",
					branch, parkedBranchLabel, branch, stat)
			}
			o.event(specFile, 0, "", "waiting_preserved", runlog.F("branch", cmp.Or(branch, "none")))
		}
	}

	if err := os.WriteFile(reportPath, []byte(waitingReport(specFile, reason, nh, entries, where)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", reportPath, err)
	}
	if ws != nil {
		o.commitBookkeeping(ctx, specFile, "diadoche: WAITING "+specFile)
	}
	o.Log.Printf("task %s: WAITING: %s", specFile, reason)
	return nil
}

// waitingReport renders spec/NNN_WAITING.md.
func waitingReport(specFile, reason string, nh *needsHumanError, entries []questions.Entry, where string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: WAITING\n\n", specFile)
	fmt.Fprintf(&b, "Reason: %s\n\n", reason)
	fmt.Fprintf(&b, "Asked-by: %s\nIteration: %d\n\n", nh.agent, nh.iter)
	if len(entries) == 0 {
		fmt.Fprintf(&b, "No question for this task was found in %s.\n\n", questions.FileName)
	} else {
		fmt.Fprintf(&b, "Questions in %s:\n", questions.FileName)
		for _, e := range entries {
			state := "unanswered"
			if e.Answered() {
				state = "answered"
			}
			fmt.Fprintf(&b, "- %s: %s (%s)\n", e.ID, e.Summary, state)
		}
		b.WriteString("\n")
	}
	b.WriteString(where + "\n\n")
	fmt.Fprintf(&b, "To resume: answer in %s (after \"Answer:\"), then run diadoche --resume-waiting.\n", questions.FileName)
	return b.String()
}

// resumeWaitingTasks is --resume-waiting: every WAITING task goes back to
// QUEUED (under the spec lock), before the run's first queue sync.
func (o *Orchestrator) resumeWaitingTasks() error {
	lock, err := speclock.Acquire(o.Layout.SpecDir, lockTimeout)
	if err != nil {
		return fmt.Errorf("acquire spec lock: %w", err)
	}
	defer lock.Release()
	sh := o.shared()
	sh.specMu.Lock()
	resumed, err := specs.ResumeWaiting(o.Layout)
	sh.specMu.Unlock()
	if err != nil {
		return fmt.Errorf("--resume-waiting: %w", err)
	}
	if len(resumed) == 0 {
		o.Log.Print("--resume-waiting: no WAITING tasks")
		return nil
	}
	entries, err := questions.Load(o.Layout.QuestionsPath)
	if err != nil {
		o.Log.Printf("warning: couldn't read %s: %v", o.Layout.QuestionsPath, err)
	}
	for _, spec := range resumed {
		mine := questions.ForTask(entries, spec)
		answered := 0
		for _, e := range mine {
			if e.Answered() {
				answered++
			}
		}
		o.Log.Printf("--resume-waiting: %s is QUEUED again (%d question(s) in %s, %d answered)", spec, len(mine), questions.FileName, answered)
		o.event(spec, 0, "", "resume_waiting", runlog.F("questions", len(mine)), runlog.F("answered", answered))
		o.event(spec, 0, "", "status", runlog.F("status", specs.StatusQueued), runlog.F("reason", "resume_waiting"))
	}
	return nil
}

// loadWaiting takes over the saved records of WAITING tasks, logging where
// each task re-queued by --resume-waiting continues.
func (o *Orchestrator) loadWaiting(saved map[string]runstate.Task) {
	waiting := maps.Clone(saved)
	for spec, rec := range waiting {
		if rec.Spec != spec || rec.Validate() != nil {
			o.Log.Printf("warning: ignoring the WAITING record of %s in %s: not a record diadoche writes", spec, runstate.FileName)
			delete(waiting, spec)
			continue
		}
		if status, err := o.getStatus(spec); err == nil && status != specs.StatusWaiting {
			iter, agent := rec.Next()
			o.Log.Printf("%s: WAITING task %s will resume at iteration %d, agent %s", runstate.FileName, spec, iter, agentLabel(agent))
		}
	}
	sh := o.shared()
	sh.mu.Lock()
	sh.waiting = waiting
	sh.mu.Unlock()
}

// restoreWaiting brings a task that was WAITING back to where it stopped:
// in place, the changes parked on the branch named in spec/NNN_WAITING.md
// are re-applied to the working tree (a conflict is only a warning: the
// task then continues on a clean tree, the branch stays). The report is
// removed either way.
func (o *Orchestrator) restoreWaiting(ctx context.Context, specFile, num string, ws *workspace) {
	reportPath := filepath.Join(o.Layout.SpecDir, num+"_WAITING.md")
	b, err := os.ReadFile(reportPath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		o.Log.Printf("warning: %s: couldn't read %s: %v", specFile, reportPath, err)
		return
	}
	if ws != nil && !ws.worktree {
		if branch := parkedBranchOf(b); branch != "" {
			if err := gitutil.ApplyCommit(ctx, o.Cfg.Dir, branch); err != nil {
				o.Log.Printf("warning: %s: couldn't re-apply the changes parked on %s (%v); continuing on a clean tree, the branch is kept", specFile, branch, err)
				o.event(specFile, 0, "", "waiting_restore_failed", runlog.F("branch", branch))
			} else {
				o.Log.Printf("%s: re-applied the changes parked on %s", specFile, branch)
				o.event(specFile, 0, "", "waiting_restored", runlog.F("branch", branch))
			}
		}
	}
	if err := os.Remove(reportPath); err != nil {
		o.Log.Printf("warning: %s: couldn't remove %s: %v", specFile, reportPath, err)
	}
}

// parkedBranchOf is the branch on report's Parked-branch line, or "".
func parkedBranchOf(report []byte) string {
	for line := range strings.SplitSeq(string(report), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), parkedBranchLabel); ok {
			return strings.Trim(strings.TrimSpace(rest), "`")
		}
	}
	return ""
}
