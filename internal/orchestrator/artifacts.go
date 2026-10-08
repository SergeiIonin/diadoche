package orchestrator

import (
	"context"
	"path/filepath"

	"diadoche/internal/agents"
	"diadoche/internal/decisions"
	"diadoche/internal/gitutil"
	"diadoche/internal/runlog"
)

// runsDirName is the directory under .claude/diadoche holding one
// directory per run: its run.log and its tasks' artifacts (see runlog).
const runsDirName = "runs"

// openRun creates this run's directory and run.log, and keeps the runs
// directory out of git. Nothing is written in --dry-run (which never
// gets here, see dryrun.go) or in a test's simulated loop. A failure is
// only a warning: the run goes on without run.log and artifacts.
func (o *Orchestrator) openRun(ctx context.Context) {
	if o.offline() {
		return
	}
	runsDir := filepath.Join(o.Layout.Root, runsDirName)
	r, err := runlog.Open(runsDir, o.now())
	if err != nil {
		o.Log.Printf("warning: run log disabled: %v", err)
		return
	}
	o.run = r
	if rel := relExcludeDirs(o.Cfg.Dir, runsDir); len(rel) == 1 {
		if err := gitutil.EnsureInfoExclude(ctx, o.Cfg.Dir, "/"+rel[0]+"/"); err != nil {
			o.Log.Printf("couldn't add %s to .git/info/exclude: %v", rel[0], err)
		}
	}
	o.Log.Printf("run log: %s", o.relPath(r.LogPath()))
}

// event writes one line to run.log and echoes it to the log as
// "event: <line>". Its fields are diadoche's own wording only — never
// claude's output or error text, which diadoche-restarter could take for a
// limit line.
func (o *Orchestrator) event(task string, iter int, agent, name string, fields ...runlog.Field) {
	line := runlog.Format(o.now(), runlog.Event{Task: task, Iter: iter, Agent: agent, Name: name, Fields: fields})
	if err := o.run.Write(line); err != nil {
		o.Log.Printf("warning: couldn't write run.log: %v", err)
	}
	o.Log.Print("event: " + line)
}

// artifactWarn reports an artifact that couldn't be written; the run goes
// on regardless.
func (o *Orchestrator) artifactWarn(specFile, what string, err error) {
	o.Log.Printf("warning: %s: couldn't write run artifact %s: %v", specFile, what, err)
	o.event(specFile, 0, "", "warning", runlog.F("what", what), runlog.F("error", err.Error()))
}

// saveArtifact writes data as the task's artifact name.
func (o *Orchestrator) saveArtifact(specFile, num, name string, data []byte) {
	if err := o.run.WriteArtifact(taskID(specFile, num), name, data); err != nil {
		o.artifactWarn(specFile, name, err)
	}
}

// copyArtifact copies the file src as the task's artifact name; a missing
// src is skipped.
func (o *Orchestrator) copyArtifact(specFile, num, name, src string) {
	if err := o.run.CopyArtifact(taskID(specFile, num), name, src); err != nil {
		o.artifactWarn(specFile, name, err)
	}
}

// saveCallOutput keeps an agent call's raw stdout and stderr as
// iter-<N>-<agent>[-k].stdout/.stderr.
func (o *Orchestrator) saveCallOutput(c agentCall, a *agents.Agent, r agents.RunResult) {
	if o.run == nil {
		return
	}
	name := o.run.CallName(taskID(c.specFile, c.num), c.iter, a.Name)
	o.saveArtifact(c.specFile, c.num, name+".stdout", []byte(r.Stdout))
	o.saveArtifact(c.specFile, c.num, name+".stderr", []byte(r.Stderr))
}

// writeFinalDiff saves the task's changes since its base commit as
// final.diff: the tracked diff outside diadoche's own directories, plus a
// new-file patch for each untracked file. It runs whatever the task's
// outcome, before anything commits, merges or parks them, and even after
// the run's ctx was cancelled.
func (o *Orchestrator) writeFinalDiff(ctx context.Context, specFile, num string, ws *workspace) {
	if ws == nil || o.run == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	diff, err := gitutil.DiffSinceExcluding(ctx, ws.dir, ws.base, ws.exclude...)
	if err != nil {
		o.artifactWarn(specFile, "final.diff", err)
	}
	untracked, err := gitutil.UntrackedFiles(ctx, ws.dir)
	if err != nil {
		o.artifactWarn(specFile, "final.diff", err)
	}
	added, err := gitutil.DiffUntracked(ctx, ws.dir, excludeUnder(untracked, ws.exclude))
	if err != nil {
		o.artifactWarn(specFile, "final.diff", err)
	}
	o.saveArtifact(specFile, num, "final.diff", []byte(diff+added))
}

// recordDecisions parses the ## Decisions section of the DONE iteration's
// worker answer and saves it as the task's decisions.json — written for
// every DONE task, as [] when worker reported none. A missing section is
// only a warning, never a task failure. workerRan is false when a resumed
// run skipped worker, whose answer is then not available. Nothing is
// written in --dry-run or a simulated loop. The event carries only a count
// and a file name, never decision text.
func (o *Orchestrator) recordDecisions(specFile, num string, iter int, output string, workerRan bool) {
	if o.offline() {
		return
	}
	var ds []decisions.Decision
	if !workerRan {
		o.Log.Printf("warning: %s: worker's answer of iteration %d is not available (resumed after worker); recording no decisions", specFile, iter)
		o.event(specFile, iter, "worker", "decisions_missing", runlog.F("reason", "resumed"))
	} else {
		var found bool
		ds, found = decisions.Parse(output)
		if found {
			o.event(specFile, iter, "worker", "decisions", runlog.F("count", len(ds)), runlog.F("file", decisions.FileName))
		} else {
			o.Log.Printf("warning: %s: worker did not report its decisions (no %q section in its answer)", specFile, decisions.Heading)
			o.event(specFile, iter, "worker", "decisions_missing")
		}
	}
	data, err := decisions.Marshal(ds)
	if err != nil {
		o.artifactWarn(specFile, decisions.FileName, err)
		return
	}
	if o.run == nil {
		return
	}
	id := taskID(specFile, num)
	if err := o.run.WriteArtifact(id, decisions.FileName, data); err != nil {
		o.artifactWarn(specFile, decisions.FileName, err)
		return
	}
	if o.decisions == nil {
		o.decisions = map[string]string{}
	}
	o.decisions[specFile] = filepath.Join(o.run.TaskDir(id), decisions.FileName)
}

// DecisionsPath is the decisions.json this run wrote for specFile, under
// .claude/diadoche/runs/<run-id>/<task-id>/ — outside any worktree, so it
// stays valid after the task's merge or commit. It is "" when the task has
// no such file in this run: --dry-run, not DONE, or the write failed.
func (o *Orchestrator) DecisionsPath(specFile string) string {
	return o.decisions[specFile]
}
