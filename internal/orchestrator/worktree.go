package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"diadoche/internal/gitutil"
	"diadoche/internal/runlog"
	"diadoche/internal/specs"
)

// worktreesDirName is the directory under .claude/diadoche holding one
// git worktree per task in worktree mode.
const worktreesDirName = "worktrees"

// worktreePath is where task taskID's worktree lives.
func (o *Orchestrator) worktreePath(id string) string {
	return filepath.Join(o.Layout.Root, worktreesDirName, id)
}

// prepareWorktreeRun runs once per run in worktree mode, before any agent
// is loaded: it falls back to in-place mode in a repo without commits
// (there is nothing to branch from), keeps the worktrees out of the main
// tree's git status, and refuses to start on a dirty main tree.
func (o *Orchestrator) prepareWorktreeRun(ctx context.Context) error {
	if !gitutil.HasCommits(ctx, o.Cfg.Dir) {
		o.Log.Printf("the repository has no commits yet, and per-task worktrees need one to branch from; running this time with --worktree=false")
		o.Cfg.Worktree = false
		return nil
	}
	if rel := relExcludeDirs(o.Cfg.Dir, filepath.Join(o.Layout.Root, worktreesDirName)); len(rel) == 1 {
		if err := gitutil.EnsureInfoExclude(ctx, o.Cfg.Dir, "/"+rel[0]+"/"); err != nil {
			o.Log.Printf("couldn't add %s to .git/info/exclude: %v", rel[0], err)
		}
	}
	return o.checkMainTreeClean(ctx)
}

// checkMainTreeClean refuses to start a worktree-mode run while the main
// working tree has uncommitted changes outside diadoche's own directories:
// every DONE task is merged into the current branch, which mustn't trip
// over (or mix with) someone else's work. Nothing is ever stashed.
func (o *Orchestrator) checkMainTreeClean(ctx context.Context) error {
	exclude := relExcludeDirs(o.Cfg.Dir, o.Layout.Root, o.Layout.AgentsDir)
	head, err := gitutil.BaseCommit(ctx, o.Cfg.Dir)
	if err != nil {
		return fmt.Errorf("git base commit: %w", err)
	}
	dirty, err := gitutil.ChangedPathsExcluding(ctx, o.Cfg.Dir, head, exclude...)
	if err != nil {
		return err
	}
	if len(dirty) == 0 {
		return nil
	}
	if o.Cfg.AllowDirty {
		o.Log.Printf("--allow-dirty: the main working tree has %d uncommitted file(s) outside %s; starting anyway", len(dirty), strings.Join(exclude, " and "))
		return nil
	}
	return fmt.Errorf("the main working tree has uncommitted changes outside %s; with --worktree each task's changes are merged into the current branch, so start from a clean tree. "+
		"Commit them (diadoche never stashes), or rerun with --allow-dirty (or --worktree=false):\n  %s",
		strings.Join(exclude, " and "), strings.Join(dirty, "\n  "))
}

// prepareWorktree sets up specFile's worktree and branch, or picks up the
// ones a previous, interrupted run left behind (the base then comes from
// spec/<num>_BASE, or the merge base with the current branch).
func (o *Orchestrator) prepareWorktree(ctx context.Context, specFile, num string) (*workspace, error) {
	id := taskID(specFile, num)
	ws := &workspace{
		dir:         o.worktreePath(id),
		worktree:    true,
		branch:      "diadoche/" + id,
		baseFile:    filepath.Join(o.Layout.SpecDir, num+"_BASE"),
		exclude:     relExcludeDirs(o.Cfg.Dir, o.Layout.Root, o.Layout.AgentsDir),
		preexisting: map[string]bool{},
	}

	resuming := true
	switch {
	case gitutil.IsWorktree(ctx, o.Cfg.Dir, ws.dir):
		o.Log.Printf("%s: resuming in existing worktree %s", specFile, o.relPath(ws.dir))
	case gitutil.BranchExists(ctx, o.Cfg.Dir, ws.branch):
		if err := gitutil.WorktreeAdd(ctx, o.Cfg.Dir, ws.dir, ws.branch, ""); err != nil {
			return nil, err
		}
		o.Log.Printf("%s: resuming on existing branch %s in a new worktree %s", specFile, ws.branch, o.relPath(ws.dir))
	default:
		resuming = false
		base, err := gitutil.BaseCommit(ctx, o.Cfg.Dir)
		if err != nil {
			return nil, fmt.Errorf("git base commit: %w", err)
		}
		if err := gitutil.WorktreeAdd(ctx, o.Cfg.Dir, ws.dir, ws.branch, base); err != nil {
			return nil, err
		}
		ws.base = base
		o.Log.Printf("%s: working in worktree %s on branch %s", specFile, o.relPath(ws.dir), ws.branch)
	}

	if resuming {
		b, err := os.ReadFile(ws.baseFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read %s: %w", ws.baseFile, err)
		}
		ws.base, _, _ = strings.Cut(strings.TrimSpace(string(b)), "\n")
		ws.base = strings.TrimSpace(ws.base)
		if ws.base == "" {
			if ws.base, err = gitutil.MergeBase(ctx, o.Cfg.Dir, "HEAD", ws.branch); err != nil {
				return nil, err
			}
		}
	}

	if err := os.WriteFile(ws.baseFile, []byte(ws.base+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", ws.baseFile, err)
	}
	return ws, nil
}

// finishDoneWorktree commits a DONE task's changes on its branch, titled
// with the task's heading, and merges the branch into the current one.
// The worktree and branch are then removed. A merge that fails (a
// conflict, typically) is aborted and the task becomes BLOCKED instead,
// keeping its worktree and branch for a human to sort out; that is not an
// error for the run, which moves on to the next task.
func (o *Orchestrator) finishDoneWorktree(ctx context.Context, specFile string, ws *workspace) error {
	specContent, err := os.ReadFile(filepath.Join(o.Layout.SpecDir, specFile))
	if err != nil {
		return fmt.Errorf("read spec file: %w", err)
	}
	committed, err := gitutil.CommitAllExcluding(ctx, ws.dir, taskTitle(specContent, specFile), ws.exclude...)
	if err != nil {
		return fmt.Errorf("commit %s on %s: %w", specFile, ws.branch, err)
	}

	if committed {
		target := gitutil.CurrentBranch(ctx, o.Cfg.Dir)
		ff, err := gitutil.Merge(ctx, o.Cfg.Dir, ws.branch, "Merge branch '"+ws.branch+"'")
		var mErr *gitutil.MergeError
		if errors.As(err, &mErr) {
			reason := fmt.Sprintf("merge of %s into %s failed (%s); worktree kept at %s", ws.branch, target, mErr.Error(), o.relPath(ws.dir))
			if err := specs.SetStatusReason(o.Layout, specFile, specs.StatusBlocked, reason); err != nil {
				o.Log.Printf("task %s: also failed to record BLOCKED status: %v", specFile, err)
			} else {
				o.event(specFile, 0, "", "status", runlog.F("status", specs.StatusBlocked), runlog.F("reason", "merge_failed"))
			}
			o.event(specFile, 0, "", "merge_blocked", runlog.F("branch", ws.branch), runlog.F("into", target))
			o.Log.Printf("task %s: BLOCKED: %s; the worktree and branch %s are kept for a human to resolve", specFile, reason, ws.branch)
			ws.removeBaseFile()
			o.commitBookkeeping(ctx, specFile, "diadoche: BLOCKED "+specFile)
			return nil
		}
		if err != nil {
			return fmt.Errorf("merge %s: %w", ws.branch, err)
		}
		how := "merge commit"
		if ff {
			how = "fast-forward"
		}
		o.Log.Printf("task %s: merged %s into %s (%s)", specFile, ws.branch, target, how)
		if !ff {
			how = "merge"
		}
		o.event(specFile, 0, "", "merge", runlog.F("branch", ws.branch), runlog.F("into", target), runlog.F("how", how))
	} else {
		o.Log.Printf("task %s: no code changes to merge", specFile)
		o.event(specFile, 0, "", "merge", runlog.F("branch", ws.branch), runlog.F("how", "none"))
	}
	// Read before commitBookkeeping adds its own commit on top.
	if head, err := gitutil.BaseCommit(ctx, o.Cfg.Dir); err != nil {
		o.Log.Printf("warning: task %s: couldn't read the merge commit: %v", specFile, err)
	} else {
		ws.mergeCommit = head
	}

	if err := gitutil.WorktreeRemove(ctx, o.Cfg.Dir, ws.dir); err != nil {
		return &fatalError{fmt.Errorf("task %s: remove worktree: %w", specFile, err)}
	}
	if err := gitutil.DeleteBranch(ctx, o.Cfg.Dir, ws.branch); err != nil {
		return &fatalError{fmt.Errorf("task %s: delete branch: %w", specFile, err)}
	}
	ws.removeBaseFile()
	o.commitBookkeeping(ctx, specFile, "diadoche: complete "+specFile)
	return nil
}

// failWorktree is park for worktree mode: the FAILED task's changes are
// committed on its branch, the worktree is removed, and the branch is
// renamed to diadoche/failed/<NNN>-<slug>. The main working tree is never
// touched. The returned error means the worktree couldn't be cleaned up.
func (o *Orchestrator) failWorktree(ctx context.Context, specFile, num string, ws *workspace, cause error) error {
	if _, err := gitutil.CommitAllExcluding(ctx, ws.dir, "diadoche: parked FAILED "+specFile, ws.exclude...); err != nil {
		return err
	}
	if err := gitutil.WorktreeRemove(ctx, o.Cfg.Dir, ws.dir); err != nil {
		return err
	}
	branch := o.failedBranchName(ctx, specFile, num)
	if err := gitutil.RenameBranch(ctx, o.Cfg.Dir, ws.branch, branch); err != nil {
		return err
	}
	stat, err := gitutil.DiffStat(ctx, o.Cfg.Dir, ws.base, branch)
	if err != nil {
		return err
	}
	restorable, err := gitutil.ChangedPathsBetween(ctx, o.Cfg.Dir, ws.base, branch)
	if err != nil {
		return err
	}
	o.Log.Printf("task %s: worktree removed, its branch renamed to %s", specFile, branch)
	o.event(specFile, 0, "", "park", runlog.F("branch", branch))

	failedPath := filepath.Join(o.Layout.SpecDir, num+"_FAILED.md")
	if err := os.WriteFile(failedPath, []byte(o.failedReport(specFile, num, branch, stat, restorable, cause, true)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", failedPath, err)
	}
	ws.removeBaseFile()
	o.commitBookkeeping(ctx, specFile, "diadoche: FAILED "+specFile)
	return nil
}

// commitBookkeeping commits diadoche's own files (.claude/diadoche) in the
// main working tree. The task's code is already safe on its branch, so a
// commit that can't be made (e.g. .claude/diadoche is gitignored) is only
// logged.
func (o *Orchestrator) commitBookkeeping(ctx context.Context, specFile, message string) {
	root := relExcludeDirs(o.Cfg.Dir, o.Layout.Root)
	if err := gitutil.CommitPaths(ctx, o.Cfg.Dir, message, root...); err != nil {
		o.Log.Printf("task %s: couldn't commit its bookkeeping files: %v", specFile, err)
	}
}

// taskNumberPrefixRe matches a task number at the start of a spec heading,
// with the punctuation after it ("006. ", "12) ", "7: ").
var taskNumberPrefixRe = regexp.MustCompile(`^\d+\s*[.):\-]?\s*`)

// taskTitle is the commit message of a DONE task in worktree mode: the
// spec's first non-empty line without the leading "#"s and task number,
// falling back to "diadoche: complete <spec>".
func taskTitle(specContent []byte, specFile string) string {
	for line := range strings.SplitSeq(string(specContent), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "# ")
		line = strings.TrimSpace(taskNumberPrefixRe.ReplaceAllString(line, ""))
		if line != "" {
			return line
		}
		break
	}
	return "diadoche: complete " + specFile
}
