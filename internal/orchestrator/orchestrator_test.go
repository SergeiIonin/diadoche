package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"diadoche/internal/gitutil"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
)

// testAgentModel is the model every test fixture agent definition uses. It
// must be one agents.KnownModels recognizes, since Orchestrator.Run
// validates every agent's resolved model before running anything.
const testAgentModel = "claude-opus-5"

func writeAgentFixtures(t *testing.T, agentsDir string) {
	t.Helper()
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"planner", "worker", "tester"} {
		content := "---\nname: " + name + "\nmodel: " + testAgentModel + "\n---\nbe the " + name
		if err := os.WriteFile(filepath.Join(agentsDir, name+".md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
}

func setTestGitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "diadoche-test")
	t.Setenv("GIT_AUTHOR_EMAIL", "diadoche-test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "diadoche-test")
	t.Setenv("GIT_COMMITTER_EMAIL", "diadoche-test@example.com")
}

// validPlanFixture is IMPL_PLAN.md content satisfying implplan's required
// contract (non-empty Files and Test Command sections).
const validPlanFixture = "## Files\n- some/file.go\n\n## Test Command\ngo test ./...\n"

// fakeRunner simulates a real planner/worker/tester: the worker writes a
// file change, and the tester marks the task DONE in STATUS.md, exactly
// like a real tester agent would via its own tool calls.
type fakeRunner struct {
	layout specs.Layout
	dir    string
}

func (r fakeRunner) Run(_ context.Context, workDir string, a *agents.Agent, _ string) (agents.RunResult, error) {
	switch a.Name {
	case "planner":
		num := "001"
		if err := os.WriteFile(filepath.Join(r.layout.SpecDir, num+"_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644); err != nil {
			return agents.RunResult{}, err
		}
	case "worker":
		if err := os.WriteFile(filepath.Join(workDir, "output.txt"), []byte("done by worker\n"), 0o644); err != nil {
			return agents.RunResult{}, err
		}
	case "tester":
		rows, err := specs.ReadStatus(r.layout.StatusPath)
		if err != nil {
			return agents.RunResult{}, err
		}
		for i := range rows {
			rows[i].Status = specs.StatusDone
		}
		if err := specs.WriteStatus(r.layout.StatusPath, rows); err != nil {
			return agents.RunResult{}, err
		}
	}
	return agents.RunResult{}, nil
}

func TestRunCommitsWhenTaskBecomesDone(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	setTestGitEnv(t)

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: dir, Iter: 1, MaxDiffBytes: config.DefaultMaxDiffBytes}
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

// alwaysFailRunner simulates a systemic failure (e.g. a misconfigured
// claude CLI): every call for the named agent errors out, exactly like
// exec.Command returning a non-zero exit.
type alwaysFailRunner struct {
	failOn string
}

func (r alwaysFailRunner) Run(_ context.Context, _ string, a *agents.Agent, _ string) (agents.RunResult, error) {
	if a.Name == r.failOn {
		return agents.RunResult{}, errors.New("simulated: claude exited with status 1")
	}
	return agents.RunResult{}, nil
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
	writeAgentFixtures(t, layout.AgentsDir)
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

func (r *recordingRunner) Run(_ context.Context, _ string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	r.calls[a.Name]++
	r.prompts[a.Name] = prompt
	if a.Name == "tester" {
		rows, err := specs.ReadStatus(r.layout.StatusPath)
		if err != nil {
			return agents.RunResult{}, err
		}
		for i := range rows {
			rows[i].Status = specs.StatusDone
		}
		if err := specs.WriteStatus(r.layout.StatusPath, rows); err != nil {
			return agents.RunResult{}, err
		}
	}
	return agents.RunResult{}, nil
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
	writeAgentFixtures(t, layout.AgentsDir)
	specFile := "001_example.md"
	if err := os.WriteFile(filepath.Join(layout.SpecDir, specFile), []byte("# Example\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644); err != nil {
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

// TestProcessTaskDoesNotReuseAnInvalidPreCrashPlan guards the interaction
// between the restart/resume logic and the plan-contract validation added
// in 09_optimize_agents.md: a plan left on disk from before that validation
// existed (or from a run that somehow skipped it) must not be blindly
// reused just because it's non-empty.
func TestProcessTaskDoesNotReuseAnInvalidPreCrashPlan(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No Files/Test Command sections: contract-invalid.
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_IMPL_PLAN.md"), []byte("some old free-form plan"), 0o644); err != nil {
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
	if runner.calls["planner"] == 0 {
		t.Error("planner should have been called at least once — the pre-existing plan is contract-invalid and must not be reused as-is")
	}
}

// ---- model resolution (09_optimize_agents.md, subtask 1) ----

type neverCalledRunner struct{ t *testing.T }

func (r neverCalledRunner) Run(context.Context, string, *agents.Agent, string) (agents.RunResult, error) {
	r.t.Fatal("Runner.Run must never be called when a model override fails validation at startup")
	return agents.RunResult{}, nil
}

func TestRunFailsFastOnUnknownModelOverride(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: dir, Iter: 1, DryRun: true,
		PlannerModel: agents.ModelOverride{Value: "not-a-real-model", Source: agents.ModelSourceFlag}}
	o := &Orchestrator{
		Cfg:    cfg,
		Layout: layout,
		Runner: neverCalledRunner{t: t},
		Log:    log.New(io.Discard, "", 0),
	}

	if err := o.Run(context.Background()); err == nil {
		t.Fatal("expected Run to fail on an unknown --planner-model override")
	}
}

func TestRunLogsResolvedModels(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf strings.Builder
	cfg := config.Config{Dir: dir, Iter: 1, DryRun: true,
		WorkerModel: agents.ModelOverride{Value: "claude-sonnet-5", Source: agents.ModelSourceFlag}}
	o := &Orchestrator{
		Cfg:    cfg,
		Layout: layout,
		Runner: newRecordingRunner(layout),
		Log:    log.New(&logBuf, "", 0),
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	got := logBuf.String()
	if !strings.Contains(got, "models:") || !strings.Contains(got, "planner="+testAgentModel+" (frontmatter)") {
		t.Errorf("expected log to report the planner model and its source; got:\n%s", got)
	}
	if !strings.Contains(got, "worker=claude-sonnet-5 (flag)") {
		t.Errorf("expected log to report the --worker-model override and its source, got:\n%s", got)
	}
}

func TestRunFailsFastOnEmptyModelOverride(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: dir, Iter: 1, DryRun: true,
		PlannerModel: agents.ModelOverride{Value: "", Source: agents.ModelSourceFlag}}
	o := &Orchestrator{
		Cfg:    cfg,
		Layout: layout,
		Runner: neverCalledRunner{t: t},
		Log:    log.New(io.Discard, "", 0),
	}

	if err := o.Run(context.Background()); err == nil {
		t.Fatal("expected Run to fail on an explicitly empty --planner-model override")
	}
}

// TestRunUsesBuiltInDefaultModelsWhenNoOverride is the "no flags" half of
// the spec's acceptance criteria: with no project agent files and no
// overrides, every agent runs on its built-in default.
func TestRunUsesBuiltInDefaultModelsWhenNoOverride(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Deliberately no writeAgentFixtures: nothing in .claude/agents.
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf strings.Builder
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 1, DryRun: true},
		Layout: layout,
		Runner: newRecordingRunner(layout),
		Log:    log.New(&logBuf, "", 0),
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	got := logBuf.String()
	for _, want := range []string{
		"planner=claude-opus-5-5 (built-in default)",
		"worker=claude-opus-5-5 (built-in default)",
		"tester=claude-sonnet-5-5 (built-in default)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected log to contain %q; got:\n%s", want, got)
		}
	}
}

// ---- usage accounting (002_usage_accounting.md) ----

// usageRunner returns a fully-parsed RunResult for every call (as if every
// claude invocation succeeded and its JSON output parsed cleanly), while
// still driving the task to DONE like fakeRunner. Token counts differ per
// agent so a test can tell the recorded lines apart.
type usageRunner struct {
	layout specs.Layout
}

// usageFixtures is what usageRunner reports for each agent: the expected
// content of that agent's usage.jsonl line.
var usageFixtures = map[string]agents.RunResult{
	"planner": {Usage: agents.Usage{InputTokens: 10, OutputTokens: 5, CacheCreationInputTokens: 100, CacheReadInputTokens: 1000}, TotalCostUSD: 0.001, DurationMS: 1, ExitCode: 0, Parsed: true},
	"worker":  {Usage: agents.Usage{InputTokens: 20, OutputTokens: 10, CacheCreationInputTokens: 200, CacheReadInputTokens: 2000}, TotalCostUSD: 0.002, DurationMS: 2, ExitCode: 0, Parsed: true},
	"tester":  {Usage: agents.Usage{InputTokens: 30, OutputTokens: 15, CacheCreationInputTokens: 300, CacheReadInputTokens: 3000}, TotalCostUSD: 0.003, DurationMS: 3, ExitCode: 0, Parsed: true},
}

func (r usageRunner) Run(_ context.Context, workDir string, a *agents.Agent, _ string) (agents.RunResult, error) {
	result := usageFixtures[a.Name]
	switch a.Name {
	case "planner":
		if err := os.WriteFile(filepath.Join(r.layout.SpecDir, "001_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644); err != nil {
			return result, err
		}
	case "worker":
		if err := os.WriteFile(filepath.Join(workDir, "output.txt"), []byte("hi\n"), 0o644); err != nil {
			return result, err
		}
	case "tester":
		rows, err := specs.ReadStatus(r.layout.StatusPath)
		if err != nil {
			return result, err
		}
		for i := range rows {
			rows[i].Status = specs.StatusDone
		}
		if err := specs.WriteStatus(r.layout.StatusPath, rows); err != nil {
			return result, err
		}
	}
	return result, nil
}

func TestRunWritesUsageJSONLAndPrintsSummary(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	setTestGitEnv(t)

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf strings.Builder
	cfg := config.Config{Dir: dir, Iter: 1, MaxDiffBytes: config.DefaultMaxDiffBytes}
	o := &Orchestrator{
		Cfg:    cfg,
		Layout: layout,
		Runner: usageRunner{layout: layout},
		Log:    log.New(&logBuf, "", 0),
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	usagePath := filepath.Join(layout.Root, "usage.jsonl")
	data, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatalf("expected %s to exist: %v", usagePath, err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 { // planner, worker, tester
		t.Fatalf("usage.jsonl has %d lines, want 3; content:\n%s", len(lines), data)
	}
	seen := map[string]bool{}
	var sumIn, sumOut, sumCacheWrite, sumCacheRead int
	var sumCost float64
	for _, line := range lines {
		var rec usage.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line not valid JSON: %v: %s", err, line)
		}
		want, ok := usageFixtures[rec.Agent]
		if !ok {
			t.Fatalf("unexpected agent %q in usage.jsonl", rec.Agent)
		}
		if seen[rec.Agent] {
			t.Errorf("agent %s recorded more than once", rec.Agent)
		}
		seen[rec.Agent] = true

		if rec.Task != "001_example.md" || rec.Iteration != 1 || rec.Model != testAgentModel {
			t.Errorf("unexpected task/iteration/model in record: %+v", rec)
		}
		if rec.Time.IsZero() {
			t.Errorf("record for %s has a zero time", rec.Agent)
		}
		if !rec.Parsed || rec.ExitCode != 0 {
			t.Errorf("record for %s: parsed=%v exit_code=%d, want parsed=true exit_code=0", rec.Agent, rec.Parsed, rec.ExitCode)
		}
		if rec.InputTokens != want.Usage.InputTokens || rec.OutputTokens != want.Usage.OutputTokens ||
			rec.CacheCreationInputTokens != want.Usage.CacheCreationInputTokens ||
			rec.CacheReadInputTokens != want.Usage.CacheReadInputTokens ||
			rec.CostUSD != want.TotalCostUSD || rec.DurationMS != want.DurationMS {
			t.Errorf("record for %s = %+v, does not match the runner's result %+v", rec.Agent, rec, want)
		}

		sumIn += rec.InputTokens
		sumOut += rec.OutputTokens
		sumCacheWrite += rec.CacheCreationInputTokens
		sumCacheRead += rec.CacheReadInputTokens
		sumCost += rec.CostUSD
	}
	if len(seen) != 3 {
		t.Fatalf("recorded agents = %v, want planner, worker and tester", seen)
	}

	// The printed summary must be exactly the sum of the usage.jsonl lines.
	out := logBuf.String()
	cacheShare := float64(sumCacheRead) / float64(sumIn+sumCacheWrite+sumCacheRead) * 100
	for _, want := range []string{
		"usage summary",
		"3 call(s)",
		fmt.Sprintf("%d input / %d output tokens", sumIn, sumOut),
		fmt.Sprintf("(+%d cache-write / %d cache-read)", sumCacheWrite, sumCacheRead),
		fmt.Sprintf("$%.4f", sumCost),
		fmt.Sprintf("cache-read share of all input tokens: %.1f%% (%d of %d)", cacheShare, sumCacheRead, sumIn+sumCacheWrite+sumCacheRead),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("end-of-run summary missing %q; got:\n%s", want, out)
		}
	}
}

// unparseableUsageRunner never returns real usage data (Parsed stays
// false), simulating a claude CLI whose --output-format json output
// couldn't be read — usage accounting must degrade to a warning, not a
// failed run.
type unparseableUsageRunner struct {
	layout specs.Layout
}

func (r unparseableUsageRunner) Run(_ context.Context, workDir string, a *agents.Agent, _ string) (agents.RunResult, error) {
	switch a.Name {
	case "planner":
		if err := os.WriteFile(filepath.Join(r.layout.SpecDir, "001_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644); err != nil {
			return agents.RunResult{}, err
		}
	case "worker":
		if err := os.WriteFile(filepath.Join(workDir, "output.txt"), []byte("hi\n"), 0o644); err != nil {
			return agents.RunResult{}, err
		}
	case "tester":
		rows, err := specs.ReadStatus(r.layout.StatusPath)
		if err != nil {
			return agents.RunResult{}, err
		}
		for i := range rows {
			rows[i].Status = specs.StatusDone
		}
		if err := specs.WriteStatus(r.layout.StatusPath, rows); err != nil {
			return agents.RunResult{}, err
		}
	}
	return agents.RunResult{}, nil // Parsed: false
}

func TestRunSurvivesUnparseableUsageOutput(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	setTestGitEnv(t)

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logBuf strings.Builder
	cfg := config.Config{Dir: dir, Iter: 1, MaxDiffBytes: config.DefaultMaxDiffBytes}
	o := &Orchestrator{
		Cfg:    cfg,
		Layout: layout,
		Runner: unparseableUsageRunner{layout: layout},
		Log:    log.New(&logBuf, "", 0),
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run should not fail just because usage JSON didn't parse: %v", err)
	}
	status, err := specs.GetStatus(layout, "001_example.md")
	if err != nil {
		t.Fatal(err)
	}
	if status != specs.StatusDone {
		t.Fatalf("status = %s, want DONE", status)
	}
	if !strings.Contains(logBuf.String(), "could not parse usage") {
		t.Errorf("expected a warning about unparseable usage; got:\n%s", logBuf.String())
	}

	data, err := os.ReadFile(filepath.Join(layout.Root, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 { // planner, worker, tester — one zeroed line each
		t.Fatalf("usage.jsonl has %d lines, want 3; content:\n%s", len(lines), data)
	}
	for _, line := range lines {
		var rec usage.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line not valid JSON: %v: %s", err, line)
		}
		if rec.Parsed {
			t.Errorf("record for %s has parsed=true, want false", rec.Agent)
		}
		if rec.InputTokens != 0 || rec.OutputTokens != 0 || rec.CostUSD != 0 {
			t.Errorf("record for %s should be zeroed, got %+v", rec.Agent, rec)
		}
	}
}

// failingUsageRunner fails every call for the named agent, reporting the
// claude CLI's non-zero exit status — a failed call must still be recorded,
// with its exit code.
type failingUsageRunner struct {
	failOn string
}

func (r failingUsageRunner) Run(_ context.Context, _ string, a *agents.Agent, _ string) (agents.RunResult, error) {
	if a.Name == r.failOn {
		return agents.RunResult{ExitCode: 1, DurationMS: 1}, errors.New("simulated: claude exited with status 1")
	}
	return agents.RunResult{}, nil
}

func TestRunRecordsFailedCallsWithTheirExitCode(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	setTestGitEnv(t)

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	for _, f := range []string{"001_a.md", "002_b.md", "003_c.md"} {
		if err := os.WriteFile(filepath.Join(layout.SpecDir, f), []byte("# task\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var logBuf strings.Builder
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 1, MaxDiffBytes: config.DefaultMaxDiffBytes},
		Layout: layout,
		Runner: failingUsageRunner{failOn: "planner"},
		Log:    log.New(&logBuf, "", 0),
	}
	if err := o.Run(context.Background()); err == nil {
		t.Fatal("expected Run to abort after repeated planner failures")
	}

	data, err := os.ReadFile(filepath.Join(layout.Root, "usage.jsonl"))
	if err != nil {
		t.Fatalf("expected a usage line even for failed calls: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 {
		t.Fatal("expected at least one usage record")
	}
	for _, line := range lines {
		var rec usage.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line not valid JSON: %v: %s", err, line)
		}
		if rec.Agent != "planner" {
			t.Errorf("recorded agent = %q, want planner (the only agent that ran)", rec.Agent)
		}
		if rec.ExitCode != 1 {
			t.Errorf("record for %s has exit_code=%d, want 1", rec.Agent, rec.ExitCode)
		}
	}
}

// ---- planner IMPL_PLAN.md contract (09_optimize_agents.md, subtask 3) ----

// planContractRunner writes an invalid (missing required sections) plan on
// its first planner call and a valid one from the second call on, so tests
// can assert the retry-within-ITER-budget behavior.
type planContractRunner struct {
	layout        specs.Layout
	plannerCalls  int
	workerCalls   int
	testerCalls   int
	alwaysInvalid bool
}

func (r *planContractRunner) Run(_ context.Context, _ string, a *agents.Agent, _ string) (agents.RunResult, error) {
	switch a.Name {
	case "planner":
		r.plannerCalls++
		content := validPlanFixture
		if r.alwaysInvalid || r.plannerCalls == 1 {
			content = "just some free-form notes, no required sections"
		}
		if err := os.WriteFile(filepath.Join(r.layout.SpecDir, "001_IMPL_PLAN.md"), []byte(content), 0o644); err != nil {
			return agents.RunResult{}, err
		}
	case "worker":
		r.workerCalls++
	case "tester":
		r.testerCalls++
		rows, err := specs.ReadStatus(r.layout.StatusPath)
		if err != nil {
			return agents.RunResult{}, err
		}
		for i := range rows {
			rows[i].Status = specs.StatusDone
		}
		if err := specs.WriteStatus(r.layout.StatusPath, rows); err != nil {
			return agents.RunResult{}, err
		}
	}
	return agents.RunResult{}, nil
}

func TestProcessTaskRetriesPlannerWithinITERBudgetWhenPlanInvalid(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	specFile := "001_example.md"
	if err := os.WriteFile(filepath.Join(layout.SpecDir, specFile), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &planContractRunner{layout: layout}
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 3, DryRun: true},
		Layout: layout,
		Runner: runner,
		Log:    log.New(io.Discard, "", 0),
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runner.plannerCalls != 2 {
		t.Errorf("plannerCalls = %d, want 2 (one rejected, one accepted)", runner.plannerCalls)
	}
	if runner.workerCalls != 1 || runner.testerCalls != 1 {
		t.Errorf("workerCalls=%d testerCalls=%d, want 1 and 1 (only after the plan became valid)", runner.workerCalls, runner.testerCalls)
	}

	reviewPath := filepath.Join(layout.SpecDir, "001_REVIEW_1.md")
	note, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatalf("expected a contract-violation review to be written to %s: %v", reviewPath, err)
	}
	if !strings.Contains(string(note), "Files") || !strings.Contains(string(note), "Test Command") {
		t.Errorf("expected the rejection note to mention the required sections; got: %s", note)
	}

	status, err := specs.GetStatus(layout, specFile)
	if err != nil {
		t.Fatal(err)
	}
	if status != specs.StatusDone {
		t.Fatalf("status = %s, want DONE", status)
	}
}

func TestProcessTaskFailsTaskIfPlanNeverBecomesValid(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	specFile := "001_example.md"
	if err := os.WriteFile(filepath.Join(layout.SpecDir, specFile), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &planContractRunner{layout: layout, alwaysInvalid: true}
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 2, DryRun: true},
		Layout: layout,
		Runner: runner,
		Log:    log.New(io.Discard, "", 0),
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runner.workerCalls != 0 || runner.testerCalls != 0 {
		t.Errorf("workerCalls=%d testerCalls=%d, want 0 and 0 — an invalid plan must never reach worker/tester", runner.workerCalls, runner.testerCalls)
	}
	status, err := specs.GetStatus(layout, specFile)
	if err != nil {
		t.Fatal(err)
	}
	if status != specs.StatusFailed {
		t.Fatalf("status = %s, want FAILED", status)
	}
}

// ---- diff-scoped tester (09_optimize_agents.md, subtask 4) ----

// scriptedRunner drives planner/worker/tester through test-supplied
// callbacks, for scenarios that need real repository side effects (writing
// specific file contents) beyond what the other fakes offer.
type scriptedRunner struct {
	layout           specs.Layout
	onPlanner        func(specDir string) error
	onWorker         func(dir string) error
	testerCalls      int
	testerPrompts    []string
	markDoneOnTester bool

	// plannerCost/workerCost/testerCost, when non-zero, are returned as a
	// Parsed RunResult's TotalCostUSD for the matching agent's call — lets
	// a test drive --max-call-cost for one agent without affecting others.
	plannerCost, workerCost, testerCost float64
}

func (r *scriptedRunner) Run(_ context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	cost := map[string]float64{"planner": r.plannerCost, "worker": r.workerCost, "tester": r.testerCost}[a.Name]
	result := agents.RunResult{TotalCostUSD: cost, Parsed: cost != 0}
	switch a.Name {
	case "planner":
		if r.onPlanner != nil {
			if err := r.onPlanner(r.layout.SpecDir); err != nil {
				return result, err
			}
		}
	case "worker":
		if r.onWorker != nil {
			if err := r.onWorker(dir); err != nil {
				return result, err
			}
		}
	case "tester":
		r.testerCalls++
		r.testerPrompts = append(r.testerPrompts, prompt)
		if r.markDoneOnTester {
			rows, err := specs.ReadStatus(r.layout.StatusPath)
			if err != nil {
				return result, err
			}
			for i := range rows {
				rows[i].Status = specs.StatusDone
			}
			if err := specs.WriteStatus(r.layout.StatusPath, rows); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func writeValidPlan(specDir string) error {
	return os.WriteFile(filepath.Join(specDir, "001_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644)
}

func TestProcessTaskSkipsTesterOnEmptyDiff(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	setTestGitEnv(t)

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	specFile := "001_example.md"
	if err := os.WriteFile(filepath.Join(layout.SpecDir, specFile), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &scriptedRunner{
		layout:    layout,
		onPlanner: writeValidPlan,
		onWorker:  func(string) error { return nil }, // worker "does nothing": no diff at all
	}
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 2, MaxDiffBytes: config.DefaultMaxDiffBytes},
		Layout: layout,
		Runner: runner,
		Log:    log.New(io.Discard, "", 0),
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runner.testerCalls != 0 {
		t.Errorf("testerCalls = %d, want 0 — an empty diff must never reach tester", runner.testerCalls)
	}
	status, err := specs.GetStatus(layout, specFile)
	if err != nil {
		t.Fatal(err)
	}
	if status != specs.StatusFailed {
		t.Fatalf("status = %s, want FAILED (no iteration could ever pass with an empty diff)", status)
	}
}

func TestProcessTaskCallsTesterWithDiffWhenWorkerAddsFile(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	setTestGitEnv(t)

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	specFile := "001_example.md"
	if err := os.WriteFile(filepath.Join(layout.SpecDir, specFile), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &scriptedRunner{
		layout:    layout,
		onPlanner: writeValidPlan,
		onWorker: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "new_feature.go"), []byte("package main\n"), 0o644)
		},
		markDoneOnTester: true,
	}
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 1, MaxDiffBytes: config.DefaultMaxDiffBytes},
		Layout: layout,
		Runner: runner,
		Log:    log.New(io.Discard, "", 0),
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runner.testerCalls != 1 {
		t.Fatalf("testerCalls = %d, want 1", runner.testerCalls)
	}
	prompt := runner.testerPrompts[0]
	if !strings.Contains(prompt, "Untracked files") || !strings.Contains(prompt, "new_feature.go") {
		t.Errorf("expected tester prompt to list the new untracked file; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "go test ./...") {
		t.Errorf("expected tester prompt to carry the plan's test command; got:\n%s", prompt)
	}
}

func TestProcessTaskTruncatesOversizedDiffForTester(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	setTestGitEnv(t)
	ctx := context.Background()

	if err := gitutil.EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	initialContent := strings.Repeat("line\n", 50)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(initialContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := gitutil.CommitAll(ctx, dir, "seed big.txt"); err != nil {
		t.Fatal(err)
	}

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	specFile := "001_example.md"
	if err := os.WriteFile(filepath.Join(layout.SpecDir, specFile), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const sentinel = "UNIQUE_SENTINEL_XYZ"
	runner := &scriptedRunner{
		layout:    layout,
		onPlanner: writeValidPlan, // plan's Files list is "some/file.go" — never big.txt
		onWorker: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "big.txt"), []byte(sentinel+"\n"+strings.Repeat("changed line\n", 50)), 0o644)
		},
		markDoneOnTester: true,
	}
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 1, MaxDiffBytes: 20}, // tiny threshold: forces truncation
		Layout: layout,
		Runner: runner,
		Log:    log.New(io.Discard, "", 0),
	}
	if err := o.Run(ctx); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runner.testerCalls != 1 {
		t.Fatalf("testerCalls = %d, want 1", runner.testerCalls)
	}
	prompt := runner.testerPrompts[0]
	if !strings.Contains(prompt, "exceeded --max-diff-bytes") {
		t.Errorf("expected tester prompt to note the diff was truncated; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "big.txt") {
		t.Errorf("expected the diff --stat summary to still mention big.txt; got:\n%s", prompt)
	}
	if strings.Contains(prompt, sentinel) {
		t.Errorf("expected the full diff body (with the sentinel) to be dropped once truncated; got:\n%s", prompt)
	}
}

// ---- per-call budget (--max-call-minutes / --max-call-cost) ----

func TestProcessTaskFailsTaskWhenWorkerCallExceedsMaxCallCost(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	setTestGitEnv(t)

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	specFile := "001_example.md"
	if err := os.WriteFile(filepath.Join(layout.SpecDir, specFile), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &scriptedRunner{
		layout:     layout,
		onPlanner:  writeValidPlan,
		onWorker:   func(dir string) error { return os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644) },
		workerCost: 10, // worker's call "costs" $10
	}
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 3, MaxDiffBytes: config.DefaultMaxDiffBytes, MaxCallCostUSD: 1},
		Layout: layout,
		Runner: runner,
		Log:    log.New(io.Discard, "", 0),
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// worker's $10 call trips the $1 cap before tester is ever reached, and
	// the task fails right away instead of burning iteration 2's budget too.
	if runner.testerCalls != 0 {
		t.Errorf("testerCalls = %d, want 0 — the cost cap should stop the task before tester runs", runner.testerCalls)
	}
	status, err := specs.GetStatus(layout, specFile)
	if err != nil {
		t.Fatal(err)
	}
	if status != specs.StatusFailed {
		t.Fatalf("status = %s, want FAILED", status)
	}
}

func TestProcessTaskDoesNotFailATaskThatReachedDoneEvenIfTesterCallWasExpensive(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	setTestGitEnv(t)

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	specFile := "001_example.md"
	if err := os.WriteFile(filepath.Join(layout.SpecDir, specFile), []byte("# Example\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &scriptedRunner{
		layout:           layout,
		onPlanner:        writeValidPlan,
		onWorker:         func(dir string) error { return os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o644) },
		markDoneOnTester: true,
		testerCost:       10, // tester's call "costs" $10; planner/worker stay cheap
	}
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 1, MaxDiffBytes: config.DefaultMaxDiffBytes, MaxCallCostUSD: 1},
		Layout: layout,
		Runner: runner,
		Log:    log.New(io.Discard, "", 0),
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// planner/worker are cheap and pass unnoticed, so the task reaches
	// tester, which marks it DONE — an expensive-but-successful tester call
	// must not retroactively fail a task that just completed.
	status, err := specs.GetStatus(layout, specFile)
	if err != nil {
		t.Fatal(err)
	}
	if status != specs.StatusDone {
		t.Fatalf("status = %s, want DONE (completed work must not be discarded over cost after the fact)", status)
	}
}

// fixedUsageRunner reports a fixed, parseable usage for every call.
type fixedUsageRunner struct{}

func (fixedUsageRunner) Run(context.Context, string, *agents.Agent, string) (agents.RunResult, error) {
	return agents.RunResult{
		Usage:        agents.Usage{InputTokens: 100, OutputTokens: 20},
		TotalCostUSD: 0.01,
		DurationMS:   1000,
		Parsed:       true,
	}, nil
}

func TestRunAgentLogsStartAndTokensWithIterationTally(t *testing.T) {
	var buf strings.Builder
	o := &Orchestrator{
		Cfg:    config.Config{Iter: 3},
		Runner: fixedUsageRunner{},
		Log:    log.New(&buf, "", 0),
	}
	call := agentCall{specFile: "001_x.md", iter: 2, tally: &iterationTally{}}

	for _, name := range []string{"planner", "worker"} {
		if _, err := o.runAgent(context.Background(), call, &agents.Agent{Name: name, Model: "m"}, "p"); err != nil {
			t.Fatal(err)
		}
	}

	out := buf.String()
	for _, want := range []string{
		"001_x.md iter 2/3: [planner] started (model=m)",
		"001_x.md iter 2/3: [planner] finished",
		"tokens: 120",
		"iteration so far: 120 tokens, $0.0100",
		"001_x.md iter 2/3: [worker] started",
		"iteration so far: 240 tokens, $0.0200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q; got:\n%s", want, out)
		}
	}
}

// slowRunner takes d to return, long enough for several heartbeats.
type slowRunner struct{ d time.Duration }

func (r slowRunner) Run(context.Context, string, *agents.Agent, string) (agents.RunResult, error) {
	time.Sleep(r.d)
	return agents.RunResult{}, nil
}

func TestRunAgentLogsHeartbeatWhileAgentRuns(t *testing.T) {
	var buf strings.Builder
	o := &Orchestrator{
		Cfg:               config.Config{Iter: 1},
		Runner:            slowRunner{d: 120 * time.Millisecond},
		Log:               log.New(&buf, "", 0),
		HeartbeatInterval: 20 * time.Millisecond,
	}
	call := agentCall{specFile: "001_x.md", iter: 1, tally: &iterationTally{}}
	if _, err := o.runAgent(context.Background(), call, &agents.Agent{Name: "worker"}, "p"); err != nil {
		t.Fatal(err)
	}

	out := buf.String()
	if !strings.Contains(out, "001_x.md iter 1/1: [worker] still running") {
		t.Fatalf("expected a heartbeat line; got:\n%s", out)
	}
	if strings.LastIndex(out, "still running") > strings.Index(out, "[worker] finished") {
		t.Fatalf("heartbeat logged after the agent finished; got:\n%s", out)
	}
}
