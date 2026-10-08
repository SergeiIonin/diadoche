package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"diadoche/internal/gitutil"
	"diadoche/internal/runstate"
	"diadoche/internal/specs"
)

// ErrInterrupted is returned by Run when a signal stopped it (see
// Orchestrator.StopRequested). It wraps context.Canceled, so callers that
// check for cancellation see it too. Its message is diadoche's own wording,
// never a claude limit line, so diadoche-restarter doesn't take it for one.
var ErrInterrupted = fmt.Errorf("interrupted by signal: %w", context.Canceled)

// stopRequested reports whether a graceful stop was asked for: the loop
// then stops at the next point between agent calls. A parallel run's
// internal stop (see runShared.stop) counts like a signal.
func (o *Orchestrator) stopRequested() bool {
	if o.sh != nil {
		select {
		case <-o.sh.stop:
			return true
		default:
		}
	}
	if o.StopRequested == nil {
		return false
	}
	select {
	case <-o.StopRequested:
		return true
	default:
		return false
	}
}

// gracefulCtx is ctx, also cancelled once a graceful stop is requested.
// Waits that don't need to finish (a usage-limit pause) use it, so the
// first signal ends them at once; agent calls use ctx itself.
func (o *Orchestrator) gracefulCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	sctx, cancel := context.WithCancel(ctx)
	var internal <-chan struct{}
	if o.sh != nil {
		internal = o.sh.stop
	}
	if o.StopRequested != nil || internal != nil {
		go func() {
			select {
			case <-o.StopRequested:
				cancel()
			case <-internal:
				cancel()
			case <-sctx.Done():
			}
		}()
	}
	return sctx, cancel
}

// initRunState sets up run-state.json once per run, after the first queue
// sync (queue is QUEUE.md then). It is off in --dry-run (which never gets
// here, see dryrun.go) and in a test's simulated loop, so neither makes a
// real run resume from fake progress. With --fresh the saved state
// is ignored (except the WAITING tasks' records); otherwise every saved,
// unfinished task that still agrees with STATUS.md and QUEUE.md is kept to
// resume (see takeResume) and moved to the head of the queue, in the
// file's order. A record that doesn't agree is ignored with a warning: the
// spec/ files are then the fallback, as without the file.
//
// The file is runtime state, rewritten after every step, so it is kept out
// of git (.git/info/exclude): the bookkeeping commits of .claude/diadoche
// never pick it up and the tree stays clean.
func (o *Orchestrator) initRunState(ctx context.Context, queue []string) {
	if o.offline() {
		return
	}
	o.runID = runstate.NewRunID(o.now())
	o.statePath = runstate.Path(o.Layout.Root)
	o.stateOn = true
	if rel := relExcludeDirs(o.Cfg.Dir, o.statePath); len(rel) == 1 {
		if err := gitutil.EnsureInfoExclude(ctx, o.Cfg.Dir, "/"+rel[0]); err != nil {
			o.Log.Printf("couldn't add %s to .git/info/exclude: %v", rel[0], err)
		}
	}

	saved, err := runstate.Load(o.statePath)
	if err != nil {
		o.Log.Printf("warning: ignoring %s: %v", runstate.FileName, err)
		saved = nil
	}
	// The WAITING tasks' records are kept even with --fresh: those tasks
	// are not in the queue, and --resume-waiting continues them.
	if saved != nil {
		o.loadWaiting(saved.Waiting)
	}

	if o.Cfg.Fresh {
		o.Log.Printf("--fresh: ignoring any saved %s", runstate.FileName)
		o.saveState()
		return
	}

	if saved != nil {
		sh := o.shared()
		var heads []string
		for _, t := range saved.Tasks {
			if reason := o.contradiction(t, queue); reason != "" {
				o.Log.Printf("warning: ignoring %s: %s", runstate.FileName, reason)
				continue
			}
			if slices.Contains(heads, t.Spec) {
				continue
			}
			sh.resumes = append(sh.resumes, t)
			heads = append(heads, t.Spec)
		}
		if len(heads) > 0 && !slices.Equal(queue[:min(len(heads), len(queue))], heads) {
			reordered := append(slices.Clone(heads), slices.DeleteFunc(slices.Clone(queue), func(s string) bool { return slices.Contains(heads, s) })...)
			if err := o.writeQueue(reordered); err != nil {
				o.Log.Printf("warning: could not move %s to the head of %s: %v", strings.Join(heads, ", "), o.Layout.QueuePath, err)
			}
		}
		for _, t := range sh.resumes {
			iter, agent := t.Next()
			o.Log.Printf("%s: task %s (run %s) will resume at iteration %d, agent %s", runstate.FileName, t.Spec, saved.RunID, iter, agentLabel(agent))
		}
	}
	o.saveState()
}

// contradiction returns why the saved task t can't be resumed: it's not a
// record diadoche writes, or it disagrees with QUEUE.md or STATUS.md (e.g.
// the task is DONE by now). "" means t is good to resume.
func (o *Orchestrator) contradiction(t runstate.Task, queue []string) string {
	if err := t.Validate(); err != nil {
		return err.Error()
	}
	if t.Num != specs.TaskNumber(t.Spec) {
		return fmt.Sprintf("task number %q doesn't match %s", t.Num, t.Spec)
	}
	if !slices.Contains(queue, t.Spec) {
		return fmt.Sprintf("task %s is not in %s", t.Spec, filepath.Base(o.Layout.QueuePath))
	}
	rows, err := specs.ReadStatus(o.Layout.StatusPath)
	if err != nil {
		return fmt.Sprintf("read %s: %v", filepath.Base(o.Layout.StatusPath), err)
	}
	for _, r := range rows {
		if r.Spec != t.Spec {
			continue
		}
		switch r.Status {
		case specs.StatusQueued, specs.StatusInProgress, specs.StatusPaused:
			return ""
		}
		return fmt.Sprintf("task %s is %s in %s", t.Spec, r.Status, filepath.Base(o.Layout.StatusPath))
	}
	return fmt.Sprintf("task %s has no row in %s", t.Spec, filepath.Base(o.Layout.StatusPath))
}

// saveState writes run-state.json: the record of every active task, then
// every resume record not taken yet (so a run stopped before reaching it
// keeps it), and the WAITING tasks' records. A write failure is only a
// warning, like usage accounting: the spec/ files still let the next run
// resume.
func (o *Orchestrator) saveState() {
	if !o.stateOn {
		return
	}
	sh := o.shared()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	o.saveStateLocked()
}

// saveStateLocked is saveState for a caller holding sh.mu, so a change and
// its save are one step and concurrent saves never reorder.
func (o *Orchestrator) saveStateLocked() {
	if !o.stateOn {
		return
	}
	sh := o.sh
	s := runstate.State{RunID: o.runID, UpdatedAt: o.now().UTC()}
	if len(sh.waiting) > 0 {
		s.Waiting = maps.Clone(sh.waiting)
	}
	for _, t := range sh.active {
		s.Tasks = append(s.Tasks, *t)
	}
	for _, t := range sh.resumes {
		if !slices.ContainsFunc(s.Tasks, func(a runstate.Task) bool { return a.Spec == t.Spec }) {
			s.Tasks = append(s.Tasks, t)
		}
	}
	if err := runstate.Save(o.statePath, s); err != nil {
		o.Log.Printf("warning: could not write %s: %v", o.statePath, err)
	}
}

// beginTaskState starts the record of specFile at iteration iter, agent
// pending, with the task's base commit, (in worktree mode) worktree and
// attempt, replacing any active record of the same task.
func (o *Orchestrator) beginTaskState(specFile, num string, ws *workspace, iter int, agent string) {
	t := &runstate.Task{Spec: specFile, Num: num, Iteration: iter, Agent: agent, AgentState: runstate.AgentPending}
	if ws != nil {
		t.BaseCommit = ws.base
		if ws.worktree {
			t.Worktree = o.relPath(ws.dir)
		}
		if ws.attempt > 1 {
			t.Attempt = ws.attempt
		}
	}
	sh := o.shared()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if i := slices.IndexFunc(sh.active, func(a *runstate.Task) bool { return a.Spec == specFile }); i >= 0 {
		sh.active[i] = t
	} else {
		sh.active = append(sh.active, t)
	}
	o.cur = t
	o.saveStateLocked()
}

// setAgentState records that agent of iteration iter is now in state.
func (o *Orchestrator) setAgentState(iter int, agent, state string) {
	if o.cur == nil {
		return
	}
	sh := o.shared()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	o.cur.Iteration, o.cur.Agent, o.cur.AgentState = iter, agent, state
	o.saveStateLocked()
}

// closeTaskState ends the current task's record: it's DONE, FAILED or
// BLOCKED, so there's nothing left to resume. A WAITING task's record is
// not closed but moved to the WAITING records (see waitTask).
func (o *Orchestrator) closeTaskState() {
	sh := o.shared()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	o.dropCurLocked()
	o.saveStateLocked()
}

// dropCurLocked removes the current task's record from the active ones;
// the caller holds sh.mu.
func (o *Orchestrator) dropCurLocked() {
	if o.cur != nil {
		cur := o.cur
		o.sh.active = slices.DeleteFunc(o.sh.active, func(a *runstate.Task) bool { return a == cur })
	}
	o.cur = nil
}

// takeResume returns and clears specFile's resume record, else the record
// specFile kept while it was WAITING (re-queued by --resume-waiting, or by
// hand), if any.
func (o *Orchestrator) takeResume(specFile string) *runstate.Task {
	sh := o.shared()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if i := slices.IndexFunc(sh.resumes, func(r runstate.Task) bool { return r.Spec == specFile }); i >= 0 {
		r := sh.resumes[i]
		sh.resumes = slices.Delete(sh.resumes, i, i+1)
		return &r
	}
	if w, ok := sh.waiting[specFile]; ok {
		delete(sh.waiting, specFile)
		o.saveStateLocked()
		return &w
	}
	return nil
}

// seedBaseFromResume writes spec/<num>_BASE from the resumed record when
// it's missing, so preparing the workspace treats the task as a resume
// with its own base (which also skips the dirty-tree check: the changes in
// the tree are the task's own).
func (o *Orchestrator) seedBaseFromResume(num string, r *runstate.Task) {
	if r.BaseCommit == "" {
		return
	}
	path := filepath.Join(o.Layout.SpecDir, num+"_BASE")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if err := os.WriteFile(path, []byte(r.BaseCommit+"\n"), 0o644); err != nil {
		o.Log.Printf("warning: could not restore %s from %s: %v", path, runstate.FileName, err)
	}
}

// checkResumedWorkspace warns when the prepared workspace isn't the one
// run-state.json recorded; spec/<num>_BASE (what parking uses) wins.
func (o *Orchestrator) checkResumedWorkspace(specFile string, ws *workspace, r *runstate.Task) {
	if ws == nil {
		return
	}
	if r.BaseCommit != "" && ws.base != r.BaseCommit {
		o.Log.Printf("warning: %s: base commit %s differs from %s's %s; continuing with %s", specFile, ws.base, runstate.FileName, r.BaseCommit, ws.base)
	}
	wt := ""
	if ws.worktree {
		wt = o.relPath(ws.dir)
	}
	if wt != r.Worktree {
		o.Log.Printf("warning: %s: worktree %q differs from %s's %q; continuing with %q", specFile, wt, runstate.FileName, r.Worktree, wt)
	}
}

// resumePoint describes where the next run continues the current task,
// for the log line of an interrupted task.
func (o *Orchestrator) resumePoint() string {
	if o.cur == nil {
		return "from the spec/ files"
	}
	iter, agent := o.cur.Next()
	return fmt.Sprintf("at iteration %d, agent %s (%s)", iter, agentLabel(agent), runstate.FileName)
}

// agentLabel names a resume agent for the log; "" is an iteration's start.
func agentLabel(agent string) string {
	if agent == "" {
		return "start"
	}
	return agent
}
