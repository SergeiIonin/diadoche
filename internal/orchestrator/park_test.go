package orchestrator

import (
	"context"
	"errors"
	"io"
	"log"
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

// gitOut runs git in dir and returns its trimmed output, failing the test
// on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// seedRepo creates a committed repo with keep.txt, edit.txt and gone.txt,
// plus diadoche's layout and a single task spec 001_example.md, and
// returns the layout. The working tree is clean afterwards.
func seedRepo(t *testing.T) (string, specs.Layout) {
	t.Helper()
	requireGit(t)
	setTestGitEnv(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := gitutil.EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"keep.txt", "edit.txt", "gone.txt"} {
		writeFile(t, filepath.Join(dir, f), f+" original\n")
	}
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	writeFile(t, filepath.Join(layout.SpecDir, "001_example.md"), "# Example\n")
	if err := gitutil.CommitAll(ctx, dir, "seed"); err != nil {
		t.Fatal(err)
	}
	return dir, layout
}

// failingWorker edits edit.txt, deletes gone.txt and adds new/dir/added.txt,
// and the tester never marks the task DONE.
func failingWorker(layout specs.Layout) *scriptedRunner {
	return &scriptedRunner{
		layout:    layout,
		onPlanner: writeValidPlan,
		onWorker: func(dir string) error {
			if err := os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("edited by worker\n"), 0o644); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := os.MkdirAll(filepath.Join(dir, "new", "dir"), 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "new", "dir", "added.txt"), []byte("added by worker\n"), 0o644)
		},
	}
}

func newTestOrchestrator(dir string, layout specs.Layout, runner agents.Runner, cfg config.Config) *Orchestrator {
	cfg.Dir = dir
	if cfg.Iter == 0 {
		cfg.Iter = 2
	}
	cfg.MaxDiffBytes = config.DefaultMaxDiffBytes
	return &Orchestrator{Cfg: cfg, Layout: layout, Runner: runner, Log: log.New(io.Discard, "", 0)}
}

const parkedBranch = "diadoche/failed/001-example"

func TestFailedTaskIsParkedOnItsOwnBranch(t *testing.T) {
	dir, layout := seedRepo(t)
	headBefore := gitOut(t, dir, "rev-parse", "HEAD")
	branchBefore := gitOut(t, dir, "symbolic-ref", "--short", "HEAD")

	o := newTestOrchestrator(dir, layout, failingWorker(layout), config.Config{})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if status, _ := specs.GetStatus(layout, "001_example.md"); status != specs.StatusFailed {
		t.Fatalf("status = %s, want FAILED", status)
	}

	// The parked branch holds all three kinds of change, on top of the base.
	if got := gitOut(t, dir, "rev-parse", parkedBranch+"^"); got != headBefore {
		t.Errorf("parked branch parent = %s, want the task's base %s", got, headBefore)
	}
	if got := gitOut(t, dir, "show", parkedBranch+":edit.txt"); got != "edited by worker" {
		t.Errorf("edit.txt on parked branch = %q", got)
	}
	if got := gitOut(t, dir, "show", parkedBranch+":new/dir/added.txt"); got != "added by worker" {
		t.Errorf("new/dir/added.txt on parked branch = %q", got)
	}
	if out := gitOut(t, dir, "ls-tree", "--name-only", parkedBranch, "gone.txt"); out != "" {
		t.Errorf("gone.txt should be deleted on the parked branch, ls-tree says %q", out)
	}
	if out := gitOut(t, dir, "diff", "--name-only", headBefore, parkedBranch, "--", ".claude"); out != "" {
		t.Errorf("parked branch must not carry diadoche's own files, got:\n%s", out)
	}

	// The working tree is back at the base and clean.
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edit.txt original\n" {
		t.Errorf("edit.txt = %q, want it reverted", got)
	}
	if got := readFile(t, filepath.Join(dir, "gone.txt")); got != "gone.txt original\n" {
		t.Errorf("gone.txt = %q, want it restored", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
		t.Errorf("new/ should be removed, stat err = %v", err)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("working tree not clean after parking:\n%s", st)
	}

	// Current branch unchanged; the only new commit is the bookkeeping one.
	if got := gitOut(t, dir, "symbolic-ref", "--short", "HEAD"); got != branchBefore {
		t.Errorf("current branch = %s, want %s", got, branchBefore)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD^"); got != headBefore {
		t.Errorf("HEAD^ = %s, want the original HEAD %s (exactly one bookkeeping commit)", got, headBefore)
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); msg != "diadoche: FAILED 001_example.md" {
		t.Errorf("last commit message = %q", msg)
	}
	for _, f := range strings.Split(gitOut(t, dir, "show", "--name-only", "--format=", "HEAD"), "\n") {
		if !strings.HasPrefix(f, ".claude/diadoche/") {
			t.Errorf("bookkeeping commit touched %s, outside .claude/diadoche/", f)
		}
	}
	committed := gitOut(t, dir, "show", "--name-only", "--format=", "HEAD")
	for _, want := range []string{"001_IMPL_PLAN.md", "001_FAILED.md", "STATUS.md"} {
		if !strings.Contains(committed, want) {
			t.Errorf("bookkeeping commit should include %s, has:\n%s", want, committed)
		}
	}

	report := readFile(t, filepath.Join(layout.SpecDir, "001_FAILED.md"))
	for _, want := range []string{parkedBranch, "edit.txt", "added.txt", "git checkout " + parkedBranch, "not DONE after 2 iteration(s)"} {
		if !strings.Contains(report, want) {
			t.Errorf("001_FAILED.md should mention %q, got:\n%s", want, report)
		}
	}
	if _, err := os.Stat(filepath.Join(layout.SpecDir, "001_BASE")); !os.IsNotExist(err) {
		t.Errorf("001_BASE should be removed after parking, stat err = %v", err)
	}
}

func TestFailedTaskOverCallCostIsParked(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := failingWorker(layout)
	runner.workerCost = 10
	o := newTestOrchestrator(dir, layout, runner, config.Config{MaxCallCostUSD: 1})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !gitutil.BranchExists(context.Background(), dir, parkedBranch) {
		t.Fatalf("expected branch %s", parkedBranch)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("working tree not clean after parking:\n%s", st)
	}
	if report := readFile(t, filepath.Join(layout.SpecDir, "001_FAILED.md")); !strings.Contains(report, "--max-call-cost") {
		t.Errorf("001_FAILED.md should carry the failure reason, got:\n%s", report)
	}
}

func TestFailedTaskWithoutChangesCreatesNoBranch(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &scriptedRunner{layout: layout, onPlanner: writeValidPlan}
	o := newTestOrchestrator(dir, layout, runner, config.Config{})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if out := gitOut(t, dir, "branch", "--list", "diadoche/failed/*"); out != "" {
		t.Errorf("no branch expected, got %q", out)
	}
	if report := readFile(t, filepath.Join(layout.SpecDir, "001_FAILED.md")); !strings.Contains(report, "no branch was created") {
		t.Errorf("001_FAILED.md should say no branch was created, got:\n%s", report)
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); msg != "diadoche: FAILED 001_example.md" {
		t.Errorf("last commit message = %q", msg)
	}
}

func TestDirtyTreeRefusesToStartTask(t *testing.T) {
	dir, layout := seedRepo(t)
	writeFile(t, filepath.Join(dir, "edit.txt"), "someone else's work\n")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "also theirs\n")

	runner := failingWorker(layout)
	o := newTestOrchestrator(dir, layout, runner, config.Config{})
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("expected Run to refuse a dirty tree")
	}
	for _, want := range []string{"edit.txt", "untracked.txt", "--allow-dirty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
	if runner.testerCalls != 0 {
		t.Errorf("no agent should have run")
	}
	if status, _ := specs.GetStatus(layout, "001_example.md"); status != specs.StatusQueued {
		t.Errorf("status = %s, want QUEUED (task never started)", status)
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "someone else's work\n" {
		t.Errorf("edit.txt = %q, must be left alone", got)
	}
}

func TestAllowDirtyNeverParksForeignFiles(t *testing.T) {
	dir, layout := seedRepo(t)
	writeFile(t, filepath.Join(dir, "keep.txt"), "someone else's work\n")
	writeFile(t, filepath.Join(dir, "theirs.txt"), "also theirs\n")

	o := newTestOrchestrator(dir, layout, failingWorker(layout), config.Config{AllowDirty: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if got := readFile(t, filepath.Join(dir, "keep.txt")); got != "someone else's work\n" {
		t.Errorf("keep.txt = %q, foreign change must be untouched", got)
	}
	if got := readFile(t, filepath.Join(dir, "theirs.txt")); got != "also theirs\n" {
		t.Errorf("theirs.txt = %q, foreign file must be untouched", got)
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edit.txt original\n" {
		t.Errorf("edit.txt = %q, the task's own change should be parked", got)
	}
	changed := gitOut(t, dir, "diff", "--name-only", "HEAD^", parkedBranch)
	if strings.Contains(changed, "keep.txt") || strings.Contains(changed, "theirs.txt") {
		t.Errorf("parked branch must not carry foreign files, has:\n%s", changed)
	}
	if !strings.Contains(changed, "edit.txt") {
		t.Errorf("parked branch should carry edit.txt, has:\n%s", changed)
	}
}

// limitRunner's worker changes a file, then its tester call fails the way
// the claude CLI does once the account's usage window is exhausted.
type limitRunner struct{ layout specs.Layout }

func (r limitRunner) Run(_ context.Context, dir string, a *agents.Agent, _ string) (agents.RunResult, error) {
	switch a.Name {
	case "planner":
		return agents.RunResult{}, writeValidPlan(r.layout.SpecDir)
	case "worker":
		return agents.RunResult{}, os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("edited by worker\n"), 0o644)
	}
	return agents.RunResult{}, errors.New(`agent tester: exit status 1: {"is_error":true,"result":"You've hit your session limit · resets 7pm (UTC)"}`)
}

func TestUsageLimitParksNothingAndTaskResumes(t *testing.T) {
	dir, layout := seedRepo(t)
	headBefore := gitOut(t, dir, "rev-parse", "HEAD")

	o := newTestOrchestrator(dir, layout, limitRunner{layout: layout}, config.Config{})
	if err := o.Run(context.Background()); err == nil {
		t.Fatal("expected Run to stop on a usage limit")
	}
	if status, _ := specs.GetStatus(layout, "001_example.md"); status != specs.StatusInProgress {
		t.Errorf("status = %s, want IN_PROGRESS", status)
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edited by worker\n" {
		t.Errorf("edit.txt = %q, the task's changes must stay in the tree", got)
	}
	if out := gitOut(t, dir, "branch", "--list", "diadoche/failed/*"); out != "" {
		t.Errorf("nothing should be parked, got branch %q", out)
	}
	if _, err := os.Stat(filepath.Join(layout.SpecDir, "001_FAILED.md")); !os.IsNotExist(err) {
		t.Errorf("001_FAILED.md must not be written, stat err = %v", err)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved to %s", got)
	}

	if got := strings.TrimSpace(readFile(t, filepath.Join(layout.SpecDir, "001_BASE"))); got != headBefore {
		t.Errorf("001_BASE = %q, want the base recorded at the first start %s", got, headBefore)
	}

	// An unrelated commit lands while diadoche is down; the resumed task
	// must still diff against its own recorded base, and its changes in the
	// tree must not trip the dirty-tree check.
	writeFile(t, filepath.Join(dir, "other.txt"), "other\n")
	gitOut(t, dir, "add", "other.txt")
	gitOut(t, dir, "commit", "-q", "-m", "unrelated")

	runner := &scriptedRunner{layout: layout, markDoneOnTester: true}
	o = newTestOrchestrator(dir, layout, runner, config.Config{})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("resumed Run failed: %v", err)
	}
	if runner.testerCalls != 1 {
		t.Fatalf("testerCalls = %d, want 1", runner.testerCalls)
	}
	if !strings.Contains(runner.testerPrompts[0], "edited by worker") {
		t.Errorf("resumed tester should see the task's own change, got:\n%s", runner.testerPrompts[0])
	}
	if !strings.Contains(runner.testerPrompts[0], "other.txt") {
		t.Errorf("resumed tester should diff against the recorded base, not the new HEAD, got:\n%s", runner.testerPrompts[0])
	}
	if status, _ := specs.GetStatus(layout, "001_example.md"); status != specs.StatusDone {
		t.Errorf("status = %s, want DONE", status)
	}
	if _, err := os.Stat(filepath.Join(layout.SpecDir, "001_BASE")); !os.IsNotExist(err) {
		t.Errorf("001_BASE should be removed once DONE, stat err = %v", err)
	}
}

// TestParkingNeverRevertsCommitsMadeAfterTheBase: a commit that lands
// between the task's start and its failure (here: made by the worker
// itself) stays in place; only the still-uncommitted change is parked.
func TestParkingNeverRevertsCommitsMadeAfterTheBase(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &scriptedRunner{
		layout:    layout,
		onPlanner: writeValidPlan,
		onWorker: func(dir string) error {
			if err := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("committed\n"), 0o644); err != nil {
				return err
			}
			if out, err := exec.Command("git", "-C", dir, "add", "committed.txt").CombinedOutput(); err != nil {
				return errors.New(string(out))
			}
			if out, err := exec.Command("git", "-C", dir, "commit", "-q", "-m", "worker commit").CombinedOutput(); err != nil {
				return errors.New(string(out))
			}
			return os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("edited by worker\n"), 0o644)
		},
	}
	o := newTestOrchestrator(dir, layout, runner, config.Config{Iter: 1})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if got := readFile(t, filepath.Join(dir, "committed.txt")); got != "committed\n" {
		t.Errorf("committed.txt = %q, a committed file must never be reverted", got)
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edit.txt original\n" {
		t.Errorf("edit.txt = %q, the uncommitted change should be parked", got)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("working tree not clean after parking:\n%s", st)
	}
	changed := gitOut(t, dir, "diff", "--name-only", parkedBranch+"^", parkedBranch)
	if changed != "edit.txt" {
		t.Errorf("parked branch should carry only edit.txt, has:\n%s", changed)
	}
}
