package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/specs"
)

const wtBranch = "diadoche/001-example"

func wtPath(layout specs.Layout) string {
	return filepath.Join(layout.Root, "worktrees", "001-example")
}

// gitFails reports whether git args in dir exits non-zero.
func gitFails(dir string, args ...string) bool {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Run() != nil
}

func statusRow(t *testing.T, layout specs.Layout, spec string) specs.Row {
	t.Helper()
	rows, err := specs.ReadStatus(layout.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Spec == spec {
			return r
		}
	}
	t.Fatalf("no STATUS.md row for %s", spec)
	return specs.Row{}
}

func TestWorktreeDoneFastForwardsIntoMainBranch(t *testing.T) {
	dir, layout := seedRepo(t)
	branchBefore := gitOut(t, dir, "symbolic-ref", "--short", "HEAD")
	var workerDir string
	runner := &scriptedRunner{
		layout:    layout,
		onPlanner: writeValidPlan,
		onWorker: func(d string) error {
			workerDir = d
			return os.WriteFile(filepath.Join(d, "output.txt"), []byte("done by worker\n"), 0o644)
		},
		markDoneOnTester: true,
	}
	o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// The worktree is already removed, so the paths can't be resolved;
	// both are built from the same dir.
	if filepath.Clean(workerDir) != wtPath(layout) {
		t.Fatalf("worker ran in %q, want the task's worktree %s", workerDir, wtPath(layout))
	}
	if !strings.Contains(runner.testerPrompts[0], "output.txt") {
		t.Errorf("tester diff should be computed in the worktree, got:\n%s", runner.testerPrompts[0])
	}
	if status, _ := specs.GetStatus(layout, "001_example.md"); status != specs.StatusDone {
		t.Fatalf("status = %s, want DONE", status)
	}

	if got := gitOut(t, dir, "symbolic-ref", "--short", "HEAD"); got != branchBefore {
		t.Errorf("current branch = %s, want %s", got, branchBefore)
	}
	if got := gitOut(t, dir, "show", "HEAD:output.txt"); got != "done by worker" {
		t.Errorf("output.txt on the main branch = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "output.txt")); got != "done by worker\n" {
		t.Errorf("output.txt in the main tree = %q", got)
	}
	subjects := gitOut(t, dir, "log", "--format=%s")
	if !strings.Contains(subjects, "\nExample\n") && !strings.HasPrefix(subjects, "Example\n") {
		t.Errorf("main branch should have the task commit titled Example, log:\n%s", subjects)
	}
	if merges := gitOut(t, dir, "rev-list", "--merges", "HEAD"); merges != "" {
		t.Errorf("expected a fast-forward, found merge commits %s", merges)
	}
	// The task commit, followed by the bookkeeping commit.
	if got := gitOut(t, dir, "log", "-2", "--format=%s"); got != "diadoche: complete 001_example.md\nExample" {
		t.Errorf("last two commits = %q, want the task commit then the bookkeeping one", got)
	}
	if files := gitOut(t, dir, "show", "--name-only", "--format=", "HEAD^"); files != "output.txt" {
		t.Errorf("task commit should carry only output.txt, has:\n%s", files)
	}

	if _, err := os.Stat(wtPath(layout)); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be removed, stat err = %v", err)
	}
	if n := strings.Count(gitOut(t, dir, "worktree", "list", "--porcelain"), "worktree "); n != 1 {
		t.Errorf("git worktree list should only have the main tree, has %d entries", n)
	}
	if gitutil.BranchExists(context.Background(), dir, wtBranch) {
		t.Errorf("branch %s should be deleted", wtBranch)
	}
	if out := gitOut(t, dir, "ls-files", ".claude/diadoche/worktrees"); out != "" {
		t.Errorf("nothing under worktrees/ may be committed, got %q", out)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("main tree should be clean, got:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(layout.SpecDir, "001_BASE")); !os.IsNotExist(err) {
		t.Errorf("001_BASE should be removed, stat err = %v", err)
	}
}

func TestWorktreeMergeConflictBlocksTask(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &scriptedRunner{
		layout:    layout,
		onPlanner: writeValidPlan,
		onWorker: func(d string) error {
			if err := os.WriteFile(filepath.Join(d, "edit.txt"), []byte("edited by worker\n"), 0o644); err != nil {
				return err
			}
			// Meanwhile the main branch moves on with a conflicting edit.
			if err := os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("edited on main\n"), 0o644); err != nil {
				return err
			}
			if out, err := exec.Command("git", "-C", dir, "commit", "-qam", "main edit").CombinedOutput(); err != nil {
				t.Errorf("commit on main: %v: %s", err, out)
			}
			return nil
		},
		markDoneOnTester: true,
	}
	o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	ctx := context.Background()

	row := statusRow(t, layout, "001_example.md")
	if row.Status != specs.StatusBlocked || row.Reason == "" {
		t.Fatalf("row = %+v, want BLOCKED with a reason", row)
	}
	if raw := readFile(t, layout.StatusPath); !strings.Contains(raw, "BLOCKED:") || !strings.Contains(raw, wtBranch) {
		t.Errorf("STATUS.md should carry the BLOCKED reason, got:\n%s", raw)
	}

	if !gitutil.IsWorktree(ctx, dir, wtPath(layout)) {
		t.Error("the worktree should be kept")
	}
	if !gitutil.BranchExists(ctx, dir, wtBranch) {
		t.Fatalf("branch %s should be kept", wtBranch)
	}
	if got := gitOut(t, dir, "show", wtBranch+":edit.txt"); got != "edited by worker" {
		t.Errorf("edit.txt on %s = %q", wtBranch, got)
	}

	if !gitFails(dir, "rev-parse", "-q", "--verify", "MERGE_HEAD") {
		t.Error("MERGE_HEAD should not exist: the merge must be aborted")
	}
	st := gitOut(t, dir, "status", "--porcelain")
	if strings.Contains(st, "UU ") || strings.Contains(st, "AA ") {
		t.Errorf("unmerged paths left in the main tree:\n%s", st)
	}
	if st != "" {
		t.Errorf("main tree should be clean, got:\n%s", st)
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edited on main\n" {
		t.Errorf("main edit.txt = %q", got)
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); msg != "diadoche: BLOCKED 001_example.md" {
		t.Errorf("last commit = %q, want the BLOCKED bookkeeping commit", msg)
	}

	_, queue, err := specs.Sync(layout)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 0 {
		t.Errorf("a BLOCKED task must not be re-queued, queue = %v", queue)
	}
}

func TestWorktreeFailedTaskRenamesBranch(t *testing.T) {
	dir, layout := seedRepo(t)
	headBefore := gitOut(t, dir, "rev-parse", "HEAD")
	o := newTestOrchestrator(dir, layout, failingWorker(layout), config.Config{Worktree: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	ctx := context.Background()

	if status, _ := specs.GetStatus(layout, "001_example.md"); status != specs.StatusFailed {
		t.Fatalf("status = %s, want FAILED", status)
	}
	if _, err := os.Stat(wtPath(layout)); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be removed, stat err = %v", err)
	}
	if gitutil.BranchExists(ctx, dir, wtBranch) {
		t.Errorf("%s should have been renamed", wtBranch)
	}
	if !gitutil.BranchExists(ctx, dir, parkedBranch) {
		t.Fatalf("expected branch %s", parkedBranch)
	}
	if got := gitOut(t, dir, "show", parkedBranch+":edit.txt"); got != "edited by worker" {
		t.Errorf("edit.txt on %s = %q", parkedBranch, got)
	}
	if got := gitOut(t, dir, "show", parkedBranch+":new/dir/added.txt"); got != "added by worker" {
		t.Errorf("new/dir/added.txt on %s = %q", parkedBranch, got)
	}
	if !gitFails(dir, "cat-file", "-e", parkedBranch+":gone.txt") {
		t.Error("gone.txt should be deleted on the failed branch")
	}
	if out := gitOut(t, dir, "diff", "--name-only", headBefore, parkedBranch, "--", ".claude"); out != "" {
		t.Errorf("failed branch must not carry diadoche's own files, got:\n%s", out)
	}

	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edit.txt original\n" {
		t.Errorf("main edit.txt = %q, must be untouched", got)
	}
	if got := readFile(t, filepath.Join(dir, "gone.txt")); got != "gone.txt original\n" {
		t.Errorf("main gone.txt = %q, must be untouched", got)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("main tree should be clean, got:\n%s", st)
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); msg != "diadoche: FAILED 001_example.md" {
		t.Errorf("last commit = %q", msg)
	}
	report := readFile(t, filepath.Join(layout.SpecDir, "001_FAILED.md"))
	for _, want := range []string{parkedBranch, "edit.txt", "added.txt"} {
		if !strings.Contains(report, want) {
			t.Errorf("001_FAILED.md should mention %q, got:\n%s", want, report)
		}
	}
}

func TestWorktreeDirtyMainTreeRefusesToStart(t *testing.T) {
	dir, layout := seedRepo(t)
	writeFile(t, filepath.Join(dir, "keep.txt"), "someone else's work\n")
	writeFile(t, filepath.Join(dir, "stray.txt"), "stray\n")

	o := newTestOrchestrator(dir, layout, neverCalledRunner{t}, config.Config{Worktree: true})
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to refuse a dirty main tree")
	}
	for _, want := range []string{"uncommitted changes", "--allow-dirty", "keep.txt", "stray.txt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
	if out := gitOut(t, dir, "stash", "list"); out != "" {
		t.Errorf("nothing may be stashed, got %q", out)
	}
	if got := readFile(t, filepath.Join(dir, "keep.txt")); got != "someone else's work\n" {
		t.Errorf("keep.txt = %q, must be left alone", got)
	}
	if _, err := os.Stat(filepath.Join(layout.Root, "worktrees")); !os.IsNotExist(err) {
		t.Errorf("no worktree may be created, stat err = %v", err)
	}
	if gitutil.BranchExists(context.Background(), dir, wtBranch) {
		t.Errorf("no task branch may be created")
	}

	t.Run("allow-dirty", func(t *testing.T) {
		runner := failingWorker(layout)
		o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true, AllowDirty: true})
		if err := o.Run(context.Background()); err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if runner.testerCalls == 0 {
			t.Error("agents should have run under --allow-dirty")
		}
		if got := readFile(t, filepath.Join(dir, "keep.txt")); got != "someone else's work\n" {
			t.Errorf("keep.txt = %q, must stay dirty", got)
		}
		if got := readFile(t, filepath.Join(dir, "stray.txt")); got != "stray\n" {
			t.Errorf("stray.txt = %q, must stay", got)
		}
	})
}

func TestWorktreeFalseKeepsInPlaceBehaviour(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		dir, layout := seedRepo(t)
		runner := failingWorker(layout)
		inner := runner.onWorker
		var workerDir string
		runner.onWorker = func(d string) error {
			workerDir = d
			return inner(d)
		}
		o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: false})
		if err := o.Run(context.Background()); err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if workerDir != dir {
			t.Errorf("worker ran in %q, want the main tree %q", workerDir, dir)
		}
		if _, err := os.Stat(filepath.Join(layout.Root, "worktrees")); !os.IsNotExist(err) {
			t.Errorf("no worktrees dir expected, stat err = %v", err)
		}
		if got := gitOut(t, dir, "show", parkedBranch+":edit.txt"); got != "edited by worker" {
			t.Errorf("edit.txt on parked branch = %q", got)
		}
		if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edit.txt original\n" {
			t.Errorf("edit.txt = %q, want it reverted by parking", got)
		}
		if _, err := os.Stat(filepath.Join(layout.SpecDir, "001_FAILED.md")); err != nil {
			t.Errorf("001_FAILED.md missing: %v", err)
		}
	})

	t.Run("done", func(t *testing.T) {
		dir, layout := seedRepo(t)
		branchBefore := gitOut(t, dir, "symbolic-ref", "--short", "HEAD")
		o := newTestOrchestrator(dir, layout, fakeRunner{layout: layout, dir: dir}, config.Config{Worktree: false})
		if err := o.Run(context.Background()); err != nil {
			t.Fatalf("Run failed: %v", err)
		}
		if msg := gitOut(t, dir, "log", "-1", "--format=%s"); msg != "diadoche: complete 001_example.md" {
			t.Errorf("last commit = %q, want the legacy commitDone message", msg)
		}
		if got := gitOut(t, dir, "symbolic-ref", "--short", "HEAD"); got != branchBefore {
			t.Errorf("current branch = %s, want %s", got, branchBefore)
		}
		if got := gitOut(t, dir, "show", "HEAD:output.txt"); got != "done by worker" {
			t.Errorf("output.txt = %q", got)
		}
		if out := gitOut(t, dir, "branch", "--list", "diadoche/*"); out != "" {
			t.Errorf("no diadoche branches expected, got %q", out)
		}
	})
}

// worktreeLimitRunner is limitRunner, recording where the worker ran.
type worktreeLimitRunner struct {
	limitRunner
	workerDir *string
}

func (r worktreeLimitRunner) Run(ctx context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	if a.Name == "worker" {
		*r.workerDir = dir
	}
	return r.limitRunner.Run(ctx, dir, a, prompt)
}

func TestWorktreeResumesExistingWorktree(t *testing.T) {
	dir, layout := seedRepo(t)
	var firstDir string
	o := newTestOrchestrator(dir, layout, worktreeLimitRunner{limitRunner{layout: layout}, &firstDir}, config.Config{Worktree: true})
	if err := o.Run(context.Background()); err == nil {
		t.Fatal("expected Run to stop on a usage limit")
	}
	if status, _ := specs.GetStatus(layout, "001_example.md"); status != specs.StatusInProgress {
		t.Errorf("status = %s, want IN_PROGRESS", status)
	}
	if got := readFile(t, filepath.Join(wtPath(layout), "edit.txt")); got != "edited by worker\n" {
		t.Errorf("worktree edit.txt = %q, the change must stay in the worktree", got)
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edit.txt original\n" {
		t.Errorf("main edit.txt = %q, must be untouched", got)
	}

	// The bookkeeping changes the interrupted run left in the main tree
	// (STATUS.md, plan, _BASE) are under .claude/diadoche and don't count
	// as a dirty tree.
	runner := &scriptedRunner{layout: layout, markDoneOnTester: true}
	o = newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("resumed Run failed: %v", err)
	}
	if runner.testerCalls != 1 || !strings.Contains(runner.testerPrompts[0], "edited by worker") {
		t.Fatalf("resumed tester should see the change made before the limit, calls=%d", runner.testerCalls)
	}
	if status, _ := specs.GetStatus(layout, "001_example.md"); status != specs.StatusDone {
		t.Errorf("status = %s, want DONE", status)
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edited by worker\n" {
		t.Errorf("main edit.txt = %q, want the merged change", got)
	}
	if gitutil.BranchExists(context.Background(), dir, wtBranch) {
		t.Errorf("branch %s should be deleted after the merge", wtBranch)
	}
}

func TestWorktreeRepoWithoutCommitsFallsBackToInPlace(t *testing.T) {
	requireGit(t)
	setTestGitEnv(t)
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	writeFile(t, filepath.Join(layout.SpecDir, "001_example.md"), "# Example\n")
	var workerDir string
	runner := &scriptedRunner{
		layout:    layout,
		onPlanner: writeValidPlan,
		onWorker: func(d string) error {
			workerDir = d
			return os.WriteFile(filepath.Join(d, "output.txt"), []byte("x\n"), 0o644)
		},
		markDoneOnTester: true,
	}
	o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if workerDir != dir {
		t.Errorf("worker ran in %q, want the main tree", workerDir)
	}
	if status, _ := specs.GetStatus(layout, "001_example.md"); status != specs.StatusDone {
		t.Errorf("status = %s, want DONE", status)
	}
}

func TestTaskTitle(t *testing.T) {
	for _, tc := range []struct{ content, want string }{
		{"# 006. Run each task in its own git worktree\n\nbody", "Run each task in its own git worktree"},
		{"# Example\n", "Example"},
		{"\n\n# 12) Foo\n", "Foo"},
		{"## 7: Bar baz", "Bar baz"},
		{"", "diadoche: complete 006_x.md"},
		{"# 006.\n", "diadoche: complete 006_x.md"},
	} {
		if got := taskTitle([]byte(tc.content), "006_x.md"); got != tc.want {
			t.Errorf("taskTitle(%q) = %q, want %q", tc.content, got, tc.want)
		}
	}
}
