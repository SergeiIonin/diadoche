package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"diadoche/internal/gitutil"
	"diadoche/internal/runlog"
	"diadoche/internal/specs"
)

// fatalError stops Run altogether instead of moving on to the next task:
// the working tree isn't in a state the next task can safely start from
// (a dirty tree, a FAILED task whose changes couldn't be parked, a task
// worktree that couldn't be set up or cleaned up), the claude account hit
// a usage limit the in-loop pause didn't catch, --max-cost-per-run was
// exceeded, or the run was interrupted while a task was paused.
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

// workspace is a task's git state: where it runs, the commit its changes
// are diffed against, and what must never be parked with them.
type workspace struct {
	// dir is where the task's agents run and its diff is computed: the
	// project root, or in worktree mode the task's own worktree.
	dir string

	// worktree is set in worktree mode, where branch
	// (diadoche/<NNN>-<slug>) is the task's branch checked out in dir.
	worktree bool
	branch   string

	// base is the commit (or, before the repo's first commit, git's empty
	// tree) HEAD pointed at when the task first started. Persisted in
	// baseFile, so a task resumed after a restart keeps diffing against
	// its own starting point, not whatever HEAD is now.
	base     string
	baseFile string

	// exclude are diadoche's own directories (relative to the project
	// root, normally ".claude/diadoche" and ".claude/agents"): its
	// bookkeeping and agent config, which worker never touches and which
	// are never part of a task's diff or parked changes. In place,
	// QUESTIONS.md is one of them too: it belongs to every task.
	exclude []string

	// preexisting are the files that were already changed when the task
	// started under --allow-dirty. They belong to someone else, so parking
	// leaves them alone.
	preexisting map[string]bool

	// mergeCommit is the commit a DONE task's changes landed as on the
	// run's branch (the merge or fast-forward tip, or the in-place commit).
	// Set by finishDoneWorktree/commitDone, read by the on-task-done hooks.
	mergeCommit string
}

func (w *workspace) removeBaseFile() { _ = os.Remove(w.baseFile) }

// prepareWorkspace sets up specFile's workspace before it runs. A task
// starting from scratch records HEAD as its base in spec/<num>_BASE, and
// refuses to start on a working tree that already has uncommitted changes
// outside diadoche's own directories (unless --allow-dirty): if the task
// FAILED, those changes would be parked along with the task's own. A task
// resuming after a restart reads its base back from spec/<num>_BASE and
// skips the check, because the changes in the tree are its own.
func (o *Orchestrator) prepareWorkspace(ctx context.Context, specFile, num string) (*workspace, error) {
	ws := &workspace{
		dir:         o.Cfg.Dir,
		baseFile:    filepath.Join(o.Layout.SpecDir, num+"_BASE"),
		exclude:     relExcludeDirs(o.Cfg.Dir, o.Layout.Root, o.Layout.AgentsDir, o.Layout.QuestionsPath),
		preexisting: map[string]bool{},
	}

	b, err := os.ReadFile(ws.baseFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", ws.baseFile, err)
	}
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); err == nil && strings.TrimSpace(lines[0]) != "" {
		ws.base = strings.TrimSpace(lines[0])
		for _, p := range lines[1:] {
			if p = strings.TrimSpace(p); p != "" {
				ws.preexisting[p] = true
			}
		}
		o.Log.Printf("%s: resuming with the diff base recorded at its first start (%s)", specFile, ws.base)
		return ws, nil
	}

	ws.base, err = gitutil.BaseCommit(ctx, o.Cfg.Dir)
	if err != nil {
		return nil, fmt.Errorf("git base commit: %w", err)
	}

	// A task with a plan or reviews on disk but no _BASE file was started
	// by a diadoche that didn't record one yet: it's a resume all the same.
	if !o.hasPriorProgress(num) {
		dirty, err := gitutil.ChangedPathsExcluding(ctx, o.Cfg.Dir, ws.base, ws.exclude...)
		if err != nil {
			return nil, err
		}
		if len(dirty) > 0 {
			if !o.Cfg.AllowDirty {
				return nil, fmt.Errorf("not starting %s: the working tree has uncommitted changes outside %s; "+
					"if the task FAILED they would be parked along with its own. Commit or stash them, or rerun with --allow-dirty:\n  %s",
					specFile, strings.Join(ws.exclude, " and "), strings.Join(dirty, "\n  "))
			}
			o.Log.Printf("%s: --allow-dirty: %d file(s) were already changed before the task started; they won't be parked if it FAILS", specFile, len(dirty))
			for _, p := range dirty {
				ws.preexisting[p] = true
			}
		}
	}

	content := ws.base + "\n"
	for _, p := range sortedKeys(ws.preexisting) {
		content += p + "\n"
	}
	if err := os.WriteFile(ws.baseFile, []byte(content), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", ws.baseFile, err)
	}
	return ws, nil
}

// hasPriorProgress reports whether task num already ran before (its plan
// or a review is on disk) and hasn't been parked since: a FAILED task, or a
// BLOCKED one re-queued by hand, starts over from a clean tree. (A WAITING
// task parked in place keeps its progress: its parked changes are
// re-applied on resume, see restoreWaiting.)
func (o *Orchestrator) hasPriorProgress(num string) bool {
	if fileExists(filepath.Join(o.Layout.SpecDir, num+"_FAILED.md")) ||
		fileExists(filepath.Join(o.Layout.SpecDir, num+"_BLOCKED.md")) {
		return false
	}
	if fileExists(filepath.Join(o.Layout.SpecDir, num+"_IMPL_PLAN.md")) {
		return true
	}
	return lastReview(o.Layout.SpecDir, num) != ""
}

// failTask marks specFile FAILED and parks its changes (see park, or
// failWorktree in worktree mode); cause
// (nil when the iteration budget ran out) is recorded in _FAILED.md. A
// broken STATUS.md write is only logged: it mustn't stop the parking, and
// leaving a task IN_PROGRESS within a still-running process would make
// every later sync re-queue it forever. The returned error is a parking
// failure, after which the next task can't safely start.
func (o *Orchestrator) failTask(ctx context.Context, specFile, num string, ws *workspace, cause error) error {
	// A task failed by --max-cost-per-task says so in STATUS.md, so it isn't
	// mistaken for one that simply ran out of iterations.
	reason := ""
	var budget *budgetError
	if errors.As(cause, &budget) && budget.task {
		reason = cause.Error()
	}
	if err := specs.SetStatusReason(o.Layout, specFile, specs.StatusFailed, reason); err != nil {
		o.Log.Printf("task %s: also failed to record FAILED status: %v", specFile, err)
	} else {
		fields := []runlog.Field{runlog.F("status", specs.StatusFailed)}
		if reason != "" {
			fields = append(fields, runlog.F("reason", reason))
		}
		o.event(specFile, 0, "", "status", fields...)
	}
	if ws == nil { // --dry-run
		return nil
	}
	if ws.worktree {
		return o.failWorktree(ctx, specFile, num, ws, cause)
	}
	return o.park(ctx, specFile, num, ws, cause)
}

// park moves a FAILED task's code changes out of the way, so the next task
// starts from a clean tree without losing them: every path changed since
// ws.base (except diadoche's own directories and ws.preexisting) is
// committed on top of ws.base to a new diadoche/failed/<NNN>-<slug> branch
// — HEAD and the current branch stay where they are — then reverted in the
// working tree.
// spec/<num>_FAILED.md records where the changes went, and the task's
// bookkeeping is committed to the current branch as "diadoche: FAILED".
func (o *Orchestrator) park(ctx context.Context, specFile, num string, ws *workspace, cause error) error {
	branch, stat, restorable, err := o.parkChanges(ctx, specFile, num, ws, failedBranchPrefix, "diadoche: parked FAILED "+specFile)
	if err != nil {
		return err
	}

	failedPath := filepath.Join(o.Layout.SpecDir, num+"_FAILED.md")
	if err := os.WriteFile(failedPath, []byte(o.failedReport(specFile, num, branch, stat, restorable, cause, false)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", failedPath, err)
	}
	ws.removeBaseFile()

	// The code is already safe on its branch, so a bookkeeping commit that
	// can't be made (e.g. .claude/diadoche is gitignored) is only logged.
	if err := gitutil.CommitPaths(ctx, o.Cfg.Dir, "diadoche: FAILED "+specFile, o.bookkeepingPaths()...); err != nil {
		o.Log.Printf("task %s: couldn't commit its bookkeeping files: %v", specFile, err)
	}
	return nil
}

// parkChanges commits every path changed since ws.base (except diadoche's
// own directories and ws.preexisting) on top of ws.base to a new
// <branchPrefix><NNN>-<slug> branch, with message, then reverts them in the
// working tree; HEAD and the current branch stay where they are. branch is
// "" when there was nothing to park; restorable are the parked files a
// checkout can bring back (not deletions).
func (o *Orchestrator) parkChanges(ctx context.Context, specFile, num string, ws *workspace, branchPrefix, message string) (branch, stat string, restorable []string, err error) {
	changed, err := gitutil.ChangedPathsExcluding(ctx, o.Cfg.Dir, ws.base, ws.exclude...)
	if err != nil {
		return "", "", nil, err
	}
	// HEAD normally still is ws.base: diadoche commits only DONE tasks.
	// If something else committed meanwhile (the worker itself, or a
	// commit made while diadoche was down), only what's still uncommitted
	// is the task's to park, and the tree is reverted to HEAD, never
	// behind it.
	head, err := gitutil.BaseCommit(ctx, o.Cfg.Dir)
	if err != nil {
		return "", "", nil, err
	}
	uncommitted := map[string]bool{}
	if head != ws.base {
		sinceHead, err := gitutil.ChangedPathsExcluding(ctx, o.Cfg.Dir, head, ws.exclude...)
		if err != nil {
			return "", "", nil, err
		}
		for _, p := range sinceHead {
			uncommitted[p] = true
		}
	}
	var paths []string
	for _, p := range changed {
		if !ws.preexisting[p] && (head == ws.base || uncommitted[p]) {
			paths = append(paths, p)
		}
	}

	if len(paths) > 0 {
		branch = o.uniqueBranchName(ctx, branchPrefix, specFile, num)
		commit, err := gitutil.ParkPaths(ctx, o.Cfg.Dir, ws.base, branch, message, paths)
		if err != nil {
			return "", "", nil, err
		}
		if stat, err = gitutil.DiffStat(ctx, o.Cfg.Dir, ws.base, commit); err != nil {
			return "", "", nil, err
		}
		for _, p := range paths {
			if fileExists(filepath.Join(o.Cfg.Dir, filepath.FromSlash(p))) {
				restorable = append(restorable, p)
			}
		}
		if err := gitutil.RestorePaths(ctx, o.Cfg.Dir, head, paths); err != nil {
			return "", "", nil, fmt.Errorf("revert parked files (they're safe on branch %s): %w", branch, err)
		}
		o.Log.Printf("task %s: parked %d changed file(s) on branch %s", specFile, len(paths), branch)
		o.event(specFile, 0, "", "park", runlog.F("branch", branch), runlog.F("files", len(paths)))
	} else {
		o.Log.Printf("task %s: no code changes to park, no branch created", specFile)
		o.event(specFile, 0, "", "park", runlog.F("branch", "none"))
	}
	return branch, stat, restorable, nil
}

// failedReport renders spec/<num>_FAILED.md. inWorktree says the task ran
// in its own worktree, whose branch was renamed to branch.
func (o *Orchestrator) failedReport(specFile, num, branch, stat string, restorable []string, cause error, inWorktree bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: FAILED\n\n", specFile)
	if cause != nil {
		fmt.Fprintf(&b, "Reason: %s\n\n", firstLine(cause.Error()))
	} else {
		fmt.Fprintf(&b, "Reason: not DONE after %d iteration(s).\n\n", o.Cfg.Iter)
	}

	if review := lastReview(o.Layout.SpecDir, num); review != "" {
		fmt.Fprintf(&b, "Last review: %s\n\n", o.relPath(filepath.Join(o.Layout.SpecDir, review)))
	} else {
		b.WriteString("Last review: none\n\n")
	}

	if branch == "" {
		b.WriteString("The task made no code changes, so no branch was created.\n")
		return b.String()
	}
	if inWorktree {
		if strings.TrimSpace(stat) == "" {
			fmt.Fprintf(&b, "The task made no code changes; its branch was kept as `%s` anyway.\n", branch)
			return b.String()
		}
		fmt.Fprintf(&b, "The task's changes were committed on its branch, renamed to `%s`; its worktree was removed and the main working tree was never touched.\n\n", branch)
	} else {
		fmt.Fprintf(&b, "Code changes were parked on branch `%s`; the working tree was reverted to the task's starting point.\n\n", branch)
	}
	fmt.Fprintf(&b, "```\n%s```\n\n", stat)
	if len(restorable) > 0 && len(restorable) <= 20 {
		fmt.Fprintf(&b, "To bring the changes back: `git checkout %s -- %s` (or `git merge %s`, which also replays deletions).\n",
			branch, strings.Join(restorable, " "), branch)
	} else {
		fmt.Fprintf(&b, "To bring the changes back: `git merge %s`.\n", branch)
	}
	return b.String()
}

var unsafeRefChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// taskID returns <num>-<slug> for specFile (just <num> without a slug),
// safe to use in a branch name or as a directory name.
func taskID(specFile, num string) string {
	slug := strings.TrimSuffix(specFile, filepath.Ext(specFile))
	slug = strings.TrimLeft(strings.TrimPrefix(slug, num), "_-")
	slug = strings.Trim(unsafeRefChars.ReplaceAllString(slug, "-"), ".-")
	if slug == "" {
		return num
	}
	return num + "-" + slug
}

// failedBranchPrefix, blockedBranchPrefix and splitBranchPrefix prefix the
// branches a FAILED task's changes, with --worktree=false a BLOCKED task's
// changes, and a task split by --on-fail split's changes are parked on
// (waitingBranchPrefix, in waiting.go, a WAITING task's in place).
const (
	failedBranchPrefix  = "diadoche/failed/"
	blockedBranchPrefix = "diadoche/blocked/"
	splitBranchPrefix   = "diadoche/split/"
)

// failedBranchName returns diadoche/failed/<num>-<slug>, see
// uniqueBranchName.
func (o *Orchestrator) failedBranchName(ctx context.Context, specFile, num string) string {
	return o.uniqueBranchName(ctx, failedBranchPrefix, specFile, num)
}

// uniqueBranchName returns <prefix><num>-<slug>, with a -2, -3... suffix if
// a branch of that name already exists (a task that FAILED again after
// being retried), so earlier parked work is never overwritten.
func (o *Orchestrator) uniqueBranchName(ctx context.Context, prefix, specFile, num string) string {
	name := prefix + taskID(specFile, num)
	candidate := name
	for n := 2; gitutil.BranchExists(ctx, o.Cfg.Dir, candidate); n++ {
		candidate = fmt.Sprintf("%s-%d", name, n)
	}
	return candidate
}

var reviewRe = regexp.MustCompile(`_REVIEW_(\d+)\.md$`)

// lastReview returns the file name of task num's highest-numbered review,
// or "" if it has none.
func lastReview(specDir, num string) string {
	matches, _ := filepath.Glob(filepath.Join(specDir, num+"_REVIEW_*.md"))
	best, bestN := "", 0
	for _, m := range matches {
		sub := reviewRe.FindStringSubmatch(m)
		if sub == nil {
			continue
		}
		if n, err := strconv.Atoi(sub[1]); err == nil && n > bestN {
			best, bestN = filepath.Base(m), n
		}
	}
	return best
}

func (o *Orchestrator) relPath(p string) string {
	if rel, err := filepath.Rel(o.Cfg.Dir, p); err == nil {
		return filepath.ToSlash(rel)
	}
	return p
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
