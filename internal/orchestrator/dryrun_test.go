package orchestrator

import (
	"bytes"
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/onfail"
	"diadoche/internal/specs"
)

// dryRunRepo is a temp git repo with two task specs, 001_a.md and 002_b.md,
// synced into STATUS.md/QUEUE.md (both QUEUED) and committed, plus one
// untracked file so `git status --porcelain` has something to compare.
func dryRunRepo(t *testing.T) (string, specs.Layout) {
	t.Helper()
	requireGit(t)
	setTestGitEnv(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := gitutil.EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	layout := specs.NewLayout(dir)
	writeAgentFixtures(t, layout.AgentsDir)
	writeFile(t, filepath.Join(layout.SpecDir, "001_a.md"), "# A\ndo a\n")
	writeFile(t, filepath.Join(layout.SpecDir, "002_b.md"), "# B\ndo b\n")
	if _, _, err := specs.Sync(layout); err != nil {
		t.Fatal(err)
	}
	if err := gitutil.CommitAll(ctx, dir, "seed"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "untracked.txt"), "not committed\n")
	return dir, layout
}

func newDryRunOrchestrator(dir string, layout specs.Layout, runner agents.Runner, logger *log.Logger) *Orchestrator {
	return &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 3, DryRun: true, MaxDiffBytes: config.DefaultMaxDiffBytes},
		Layout: layout,
		Runner: runner,
		Log:    logger,
	}
}

// treeSnapshot maps every file under dir (outside .git, plus
// .git/info/exclude) to its content.
func treeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() {
			if rel == ".git" {
				return fs.SkipDir
			}
			snap[rel+"/"] = ""
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snap[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude")); err == nil {
		snap[".git/info/exclude"] = string(b)
	}
	return snap
}

func gitSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	return map[string]string{
		"status":   gitOut(t, dir, "status", "--porcelain", "--untracked-files=all"),
		"branch":   gitOut(t, dir, "branch", "-a"),
		"worktree": gitOut(t, dir, "worktree", "list"),
		"head":     gitOut(t, dir, "rev-parse", "HEAD"),
		"stash":    gitOut(t, dir, "stash", "list"),
	}
}

func diffSnapshots(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if a, ok := after[k]; !ok {
			t.Errorf("%s: %s disappeared", what, k)
		} else if a != v {
			t.Errorf("%s: %s changed:\n%s\nwant:\n%s", what, k, a, v)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("%s: %s appeared", what, k)
		}
	}
}

func TestDryRunLeavesStateAndGitUntouched(t *testing.T) {
	dir, layout := dryRunRepo(t)
	filesBefore, gitBefore := treeSnapshot(t, dir), gitSnapshot(t, dir)
	if gitBefore["status"] == "" {
		t.Fatal("fixture: git status is empty, the comparison would be meaningless")
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	o := newDryRunOrchestrator(dir, layout, agents.DryRunRunner{Log: logger}, logger)
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	diffSnapshots(t, "files", filesBefore, treeSnapshot(t, dir))
	diffSnapshots(t, "git", gitBefore, gitSnapshot(t, dir))
	for _, spec := range []string{"001_a.md", "002_b.md"} {
		if st, err := specs.GetStatus(layout, spec); err != nil || st != specs.StatusQueued {
			t.Errorf("%s status = %q, %v; want QUEUED", spec, st, err)
		}
	}
	if !strings.Contains(buf.String(), "dry-run: no state was changed") {
		t.Errorf("log lacks the final dry-run line:\n%s", buf.String())
	}
}

// promptRecorder records every agent call in order.
type promptRecorder struct {
	agents  []string
	prompts []string
}

func (r *promptRecorder) Run(_ context.Context, _ string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	r.agents = append(r.agents, a.Name)
	r.prompts = append(r.prompts, prompt)
	return agents.RunResult{}, nil
}

func TestDryRunPrintsEveryAgentPromptPerTask(t *testing.T) {
	dir, layout := dryRunRepo(t)
	rec := &promptRecorder{}
	if err := newDryRunOrchestrator(dir, layout, rec, log.New(&bytes.Buffer{}, "", 0)).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got, want := strings.Join(rec.agents, " "), "planner worker tester planner worker tester"; got != want {
		t.Fatalf("agent calls = %q, want %q", got, want)
	}
	for i, task := range []struct{ spec, num string }{{"001_a.md", "001"}, {"002_b.md", "002"}} {
		planner, worker, tester := rec.prompts[3*i], rec.prompts[3*i+1], rec.prompts[3*i+2]
		for _, p := range []string{planner, worker, tester} {
			if !strings.Contains(p, "Task spec ("+task.spec+")") {
				t.Errorf("prompt for %s lacks its task spec:\n%s", task.spec, p)
			}
		}
		if !strings.Contains(planner, "Write (or update) the implementation plan to") || !strings.Contains(planner, task.num+"_IMPL_PLAN.md") {
			t.Errorf("planner prompt for %s:\n%s", task.spec, planner)
		}
		if !strings.Contains(worker, dryRunPlan(task.num+"_IMPL_PLAN.md")) || !strings.Contains(worker, "Implement the plan in this repository.") {
			t.Errorf("worker prompt for %s:\n%s", task.spec, worker)
		}
		for _, want := range []string{dryRunPlan(task.num + "_IMPL_PLAN.md"), dryRunTestCommand, dryRunDiff, task.num + "_REVIEW_1.md"} {
			if !strings.Contains(tester, want) {
				t.Errorf("tester prompt for %s lacks %q:\n%s", task.spec, want, tester)
			}
		}
	}

	// The production runner prints them.
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	if err := newDryRunOrchestrator(dir, layout, agents.DryRunRunner{Log: logger}, logger).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"agent=planner", "agent=worker", "agent=tester", "Task spec (001_a.md)", "Task spec (002_b.md)", dryRunPlaceholder} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q", want)
		}
	}
}

func TestDryRunIncludesUnsyncedTaskWithoutWritingStatus(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	writeAgentFixtures(t, layout.AgentsDir)
	writeFile(t, filepath.Join(layout.SpecDir, "001_new.md"), "# New\n")

	rec := &promptRecorder{}
	if err := newDryRunOrchestrator(dir, layout, rec, log.New(&bytes.Buffer{}, "", 0)).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(rec.agents, " "); got != "planner worker tester" {
		t.Errorf("agent calls = %q, want one round for 001_new.md", got)
	}
	for _, p := range []string{layout.StatusPath, layout.QueuePath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s exists after a dry run (stat err = %v)", p, err)
		}
	}
}

func TestDryRunSkipsDoneAndFailedTasks(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	writeAgentFixtures(t, layout.AgentsDir)
	for _, f := range []string{"001_done.md", "002_failed.md", "003_queued.md"} {
		writeFile(t, filepath.Join(layout.SpecDir, f), "# "+f+"\n")
	}
	if err := specs.WriteStatus(layout.StatusPath, []specs.Row{
		{Num: "001", Spec: "001_done.md", Status: specs.StatusDone},
		{Num: "002", Spec: "002_failed.md", Status: specs.StatusFailed},
		{Num: "003", Spec: "003_queued.md", Status: specs.StatusQueued},
	}); err != nil {
		t.Fatal(err)
	}

	rec := &promptRecorder{}
	if err := newDryRunOrchestrator(dir, layout, rec, log.New(&bytes.Buffer{}, "", 0)).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.prompts) != 3 {
		t.Fatalf("got %d agent calls, want 3", len(rec.prompts))
	}
	for _, p := range rec.prompts {
		if !strings.Contains(p, "Task spec (003_queued.md)") {
			t.Errorf("prompt is not for 003_queued.md:\n%s", p)
		}
	}
}

func TestPromptsIncludeReviewFeedbackOnlyWhenGiven(t *testing.T) {
	const feedback = "Feedback from the previous review"
	spec := []byte("# Spec\n")
	if p := plannerPrompt("001_a.md", spec, "", "/x/001_IMPL_PLAN.md", ""); strings.Contains(p, feedback) {
		t.Errorf("planner prompt without a review has feedback:\n%s", p)
	}
	if p := plannerPrompt("001_a.md", spec, "fix X", "/x/001_IMPL_PLAN.md", ""); !strings.Contains(p, feedback+", address every point:\nfix X\n") {
		t.Errorf("planner prompt lacks the review:\n%s", p)
	}
	if p := workerPrompt("001_a.md", spec, "001_IMPL_PLAN.md", "plan", "", ""); strings.Contains(p, feedback) {
		t.Errorf("worker prompt without a review has feedback:\n%s", p)
	}
	if p := workerPrompt("001_a.md", spec, "001_IMPL_PLAN.md", "plan", "fix X", ""); !strings.Contains(p, feedback+", address every point:\nfix X\n") {
		t.Errorf("worker prompt lacks the review:\n%s", p)
	}
}

func TestDryRunListsOnTaskDoneHooksWithoutRunningThem(t *testing.T) {
	dir, layout := dryRunRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	filesBefore, gitBefore := treeSnapshot(t, dir), gitSnapshot(t, dir)

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	o := newDryRunOrchestrator(dir, layout, agents.DryRunRunner{Log: logger}, logger)
	o.Cfg.OnTaskDone = []string{"touch " + marker, "echo second"}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, want := range []string{
		"once 001-a is DONE and merged, would run on-task-done hook 1: touch " + marker,
		"once 001-a is DONE and merged, would run on-task-done hook 2: echo second",
		"once 002-b is DONE and merged, would run on-task-done hook 1: touch " + marker,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("dry-run log lacks %q:\n%s", want, buf.String())
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("--dry-run ran a hook (stat err = %v)", err)
	}
	diffSnapshots(t, "files", filesBefore, treeSnapshot(t, dir))
	diffSnapshots(t, "git", gitBefore, gitSnapshot(t, dir))
}

// With an --on-fail chain, --dry-run still applies no policy and changes
// no status; it only names the chain a real run would apply.
func TestDryRunWithOnFailChainChangesNothing(t *testing.T) {
	dir, layout := dryRunRepo(t)
	filesBefore, gitBefore := treeSnapshot(t, dir), gitSnapshot(t, dir)

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	rec := &promptRecorder{}
	o := newDryRunOrchestrator(dir, layout, rec, logger)
	o.Cfg.OnFail = []string{"escalate", "block"}
	o.Cfg.EscalationModel = agents.ModelOverride{Value: "claude-opus-5-5", Source: agents.ModelSourceFlag}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	diffSnapshots(t, "files", filesBefore, treeSnapshot(t, dir))
	diffSnapshots(t, "git", gitBefore, gitSnapshot(t, dir))
	for _, spec := range []string{"001_a.md", "002_b.md"} {
		if st, err := specs.GetStatus(layout, spec); err != nil || st != specs.StatusQueued {
			t.Errorf("%s status = %q, %v; want QUEUED", spec, st, err)
		}
	}
	for _, f := range []string{"001_BLOCKED.md", "002_BLOCKED.md", "001_FAILED.md"} {
		if fileExists(filepath.Join(layout.SpecDir, f)) {
			t.Errorf("dry run wrote %s", f)
		}
	}
	if want := "planner,worker,tester,planner,worker,tester"; strings.Join(rec.agents, ",") != want {
		t.Errorf("dry run calls = %v, want only the iteration-1 prompts of each task (%s)", rec.agents, want)
	}
	if !strings.Contains(buf.String(), "--on-fail escalate,block (escalation model claude-opus-5-5); the dry run applies none") {
		t.Errorf("log does not name the on-fail chain:\n%s", buf.String())
	}
}

func TestDryRunWithSplitInChainCallsNoPreprocessor(t *testing.T) {
	dir, layout := dryRunRepo(t)
	filesBefore, gitBefore := treeSnapshot(t, dir), gitSnapshot(t, dir)
	specsBefore, _ := specs.DiscoverTaskFiles(layout.SpecDir)

	var buf bytes.Buffer
	o := newDryRunOrchestrator(dir, layout, &promptRecorder{}, log.New(&buf, "", 0))
	rec := o.Runner.(*promptRecorder)
	o.Cfg.OnFail = []string{onfail.Escalate, onfail.Split, onfail.Block}
	o.Cfg.EscalationModel = agents.ModelOverride{Value: "claude-opus-5-5", Source: agents.ModelSourceFlag}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	diffSnapshots(t, "files", filesBefore, treeSnapshot(t, dir))
	diffSnapshots(t, "git", gitBefore, gitSnapshot(t, dir))
	if specsAfter, _ := specs.DiscoverTaskFiles(layout.SpecDir); strings.Join(specsAfter, ",") != strings.Join(specsBefore, ",") {
		t.Errorf("spec files %v -> %v; a dry run writes no spec", specsBefore, specsAfter)
	}
	if slices.Contains(rec.agents, "preprocessor") {
		t.Errorf("dry run called the preprocessor: %v", rec.agents)
	}
	if !strings.Contains(buf.String(), "--on-fail escalate,split,block") || !strings.Contains(buf.String(), "applies none") {
		t.Errorf("log does not name the on-fail chain:\n%s", buf.String())
	}
}

func TestDryRunIgnoresNeedsHumanAnswer(t *testing.T) {
	dir, layout := dryRunRepo(t)
	statusBefore := readFile(t, layout.StatusPath)
	asker := funcRunner(func(context.Context, string, *agents.Agent, string) (agents.RunResult, error) {
		return agents.RunResult{Output: "NEEDS_HUMAN\n"}, nil
	})
	if err := newDryRunOrchestrator(dir, layout, asker, log.New(&bytes.Buffer{}, "", 0)).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := readFile(t, layout.StatusPath); got != statusBefore {
		t.Errorf("STATUS.md changed:\n%s\nwant:\n%s", got, statusBefore)
	}
	if fileExists(layout.QuestionsPath) {
		t.Error("QUESTIONS.md written by a dry run")
	}
}

func TestDryRunResumeWaitingShowsAnswersAndChangesNothing(t *testing.T) {
	dir, layout := dryRunRepo(t)
	if err := specs.SetStatusReason(layout, "001_a.md", specs.StatusWaiting, "planner needs a human decision: answer in QUESTIONS.md"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, layout.QuestionsPath, "## Question 001-1: Storage\nTask: 001_a.md\nQuestion: Memory or disk?\nOptions:\n- A: memory\n- B: disk\nDefault: A\nWhy: simpler\nReversible: no\nAnswer: disk, please\n")
	statusBefore, questionsBefore := readFile(t, layout.StatusPath), readFile(t, layout.QuestionsPath)

	rec := &promptRecorder{}
	var buf bytes.Buffer
	o := newDryRunOrchestrator(dir, layout, rec, log.New(&buf, "", 0))
	o.Cfg.ResumeWaiting = true
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.prompts) != 6 || !strings.Contains(rec.prompts[0], "Task spec (001_a.md)") {
		t.Fatalf("want 001_a.md (WAITING) then 002_b.md walked, got %d prompts:\n%v", len(rec.prompts), rec.agents)
	}
	if !strings.Contains(rec.prompts[0], "Answer: disk, please") || !strings.Contains(rec.prompts[0], "Memory or disk?") {
		t.Errorf("001's planner prompt lacks the question and answer:\n%s", rec.prompts[0])
	}
	if !strings.Contains(buf.String(), "would resume WAITING task(s): 001_a.md") {
		t.Errorf("log lacks the WAITING tasks:\n%s", buf.String())
	}
	if got := readFile(t, layout.StatusPath); got != statusBefore {
		t.Errorf("STATUS.md changed:\n%s", got)
	}
	if got := readFile(t, layout.QuestionsPath); got != questionsBefore {
		t.Errorf("QUESTIONS.md changed:\n%s", got)
	}
}
