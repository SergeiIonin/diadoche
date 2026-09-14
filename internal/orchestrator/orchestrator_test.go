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
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/specs"
)

// fakeRunner simulates a real planner/worker/tester: the worker writes a
// file change, and the tester marks the task DONE in STATUS.md, exactly
// like a real tester agent would via its own tool calls.
type fakeRunner struct {
	layout specs.Layout
	dir    string
}

func (r fakeRunner) Run(_ context.Context, workDir string, a *agents.Agent, _ string) (string, error) {
	switch a.Name {
	case "worker":
		if err := os.WriteFile(filepath.Join(workDir, "output.txt"), []byte("done by worker\n"), 0o644); err != nil {
			return "", err
		}
	case "tester":
		rows, err := specs.ReadStatus(r.layout.StatusPath)
		if err != nil {
			return "", err
		}
		for i := range rows {
			rows[i].Status = specs.StatusDone
		}
		if err := specs.WriteStatus(r.layout.StatusPath, rows); err != nil {
			return "", err
		}
	}
	return "", nil
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
}

func TestRunCommitsWhenTaskBecomesDone(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()

	t.Setenv("GIT_AUTHOR_NAME", "diadoche-test")
	t.Setenv("GIT_AUTHOR_EMAIL", "diadoche-test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "diadoche-test")
	t.Setenv("GIT_COMMITTER_EMAIL", "diadoche-test@example.com")

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"planner", "worker", "tester"} {
		content := "---\nname: " + name + "\nmodel: test-model\n---\nbe the " + name
		if err := os.WriteFile(filepath.Join(layout.AgentsDir, name+".md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: dir, Iter: 1}
	o := &Orchestrator{
		Cfg:    cfg,
		Layout: layout,
		Runner: fakeRunner{layout: layout, dir: dir},
		Log:    log.New(io.Discard, "", 0),
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("expected git repo to be initialized: %v", err)
	}

	status, err := specs.GetStatus(layout, "001_example.md")
	if err != nil {
		t.Fatal(err)
	}
	if status != specs.StatusDone {
		t.Fatalf("status = %s, want DONE", status)
	}

	out, err := exec.Command("git", "-C", dir, "log", "--oneline").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "001_example.md") {
		t.Fatalf("expected commit message to mention the task file, got: %q", out)
	}
}

// alwaysFailRunner simulates a systemic failure (e.g. a usage-limited claude
// CLI): every call for the named agent errors out, exactly like
// exec.Command returning a non-zero exit.
type alwaysFailRunner struct {
	failOn string
}

func (r alwaysFailRunner) Run(_ context.Context, _ string, a *agents.Agent, _ string) (string, error) {
	if a.Name == r.failOn {
		return "", errors.New("simulated: usage limit reached")
	}
	return "", nil
}

// TestRunDoesNotLoopForeverOnRepeatedAgentFailures guards against the bug
// where a task left IN_PROGRESS after an agent invocation error got
// re-queued by every subsequent sync's crash-recovery pass, forever, within
// the same still-running process (observed in practice when the claude CLI
// started failing mid-run, e.g. on a usage limit).
func TestRunDoesNotLoopForeverOnRepeatedAgentFailures(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"planner", "worker", "tester"} {
		content := "---\nname: " + name + "\nmodel: test-model\n---\nbe the " + name
		if err := os.WriteFile(filepath.Join(layout.AgentsDir, name+".md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"001_a.md", "002_b.md", "003_c.md", "004_d.md"} {
		if err := os.WriteFile(filepath.Join(layout.SpecDir, f), []byte("# task\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Config{Dir: dir, Iter: 1, DryRun: true} // DryRun: skip git, it's irrelevant here
	o := &Orchestrator{
		Cfg:    cfg,
		Layout: layout,
		Runner: alwaysFailRunner{failOn: "planner"},
		Log:    log.New(io.Discard, "", 0),
	}

	done := make(chan error, 1)
	go func() { done <- o.Run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Run to return an error after repeated agent failures, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s — it looks like it's spinning forever")
	}

	rows, err := specs.ReadStatus(layout.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("expected STATUS.md to have rows for the discovered tasks")
	}
	for _, r := range rows {
		// QUEUED is fine here (a task Run never got to before the circuit
		// breaker tripped); IN_PROGRESS is exactly the bug this guards
		// against — a task abandoned mid-attempt would be re-queued forever
		// by the next sync's crash-recovery pass, in this very process.
		if r.Status == specs.StatusInProgress {
			t.Errorf("task %s left IN_PROGRESS after Run gave up — would be re-queued forever by the next sync", r.Spec)
		}
	}
}

// recordingRunner counts calls per agent name and keeps the last prompt seen
// for each, so a test can assert what processTask actually did (e.g. that
// planner was skipped, or which iteration's review a prompt referenced).
// The tester call always marks the task DONE, like a real tester agent
// deciding the work is finished.
type recordingRunner struct {
	layout  specs.Layout
	calls   map[string]int
	prompts map[string]string
}

func newRecordingRunner(layout specs.Layout) *recordingRunner {
	return &recordingRunner{layout: layout, calls: map[string]int{}, prompts: map[string]string{}}
}

func (r *recordingRunner) Run(_ context.Context, _ string, a *agents.Agent, prompt string) (string, error) {
	r.calls[a.Name]++
	r.prompts[a.Name] = prompt
	if a.Name == "tester" {
		rows, err := specs.ReadStatus(r.layout.StatusPath)
		if err != nil {
			return "", err
		}
		for i := range rows {
			rows[i].Status = specs.StatusDone
		}
		if err := specs.WriteStatus(r.layout.StatusPath, rows); err != nil {
			return "", err
		}
	}
	return "", nil
}

// TestProcessTaskResumesAtCorrectIterationAndReusesPlan simulates a crash
// right after iteration 1 fully completed (tester reviewed it and said "not
// done yet") but before iteration 2's planner ran. On restart, diadoche must
// pick up at iteration 2 — not iteration 1 again — and reuse the existing
// implementation plan instead of regenerating it.
func TestProcessTaskResumesAtCorrectIterationAndReusesPlan(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"planner", "worker", "tester"} {
		content := "---\nname: " + name + "\nmodel: test-model\n---\nbe the " + name
		if err := os.WriteFile(filepath.Join(layout.AgentsDir, name+".md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	specFile := "001_example.md"
	if err := os.WriteFile(filepath.Join(layout.SpecDir, specFile), []byte("# Example\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_IMPL_PLAN.md"), []byte("existing plan from before the crash"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_REVIEW_1.md"), []byte("needs more work: add X"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := newRecordingRunner(layout)
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 3, DryRun: true},
		Layout: layout,
		Runner: runner,
		Log:    log.New(io.Discard, "", 0),
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runner.calls["planner"] != 0 {
		t.Errorf("planner called %d times, want 0 (existing plan should be reused)", runner.calls["planner"])
	}
	if runner.calls["worker"] != 1 {
		t.Errorf("worker called %d times, want 1", runner.calls["worker"])
	}
	if runner.calls["tester"] != 1 {
		t.Errorf("tester called %d times, want 1", runner.calls["tester"])
	}

	if !strings.Contains(runner.prompts["tester"], "001_REVIEW_2.md") {
		t.Errorf("tester prompt should target 001_REVIEW_2.md (iteration 2, not restarted at 1); got: %s", runner.prompts["tester"])
	}
	if !strings.Contains(runner.prompts["worker"], "needs more work: add X") {
		t.Errorf("worker prompt should carry the review left from before the crash; got: %s", runner.prompts["worker"])
	}

	status, err := specs.GetStatus(layout, specFile)
	if err != nil {
		t.Fatal(err)
	}
	if status != specs.StatusDone {
		t.Fatalf("status = %s, want DONE", status)
	}
}
