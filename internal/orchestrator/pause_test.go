package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/specs"
	"diadoche/internal/usagelimit"
)

// limitJSON is the claude CLI's JSON result for a call refused with text.
func limitJSON(text string) string {
	return fmt.Sprintf(`{"type":"result","subtype":"success","is_error":true,"result":%q}`, text)
}

const (
	sessionLimitText = "You've hit your session limit"
	rateLimitText    = `API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your rate limit"}}`
)

// pauseRunner is a fake claude: for agent refuseAgent it answers the first
// len(refusals) calls with a limit refusal (exit 1, error text like
// agents.CLIRunner's), afterwards — and for every other agent — it acts
// like a real planner/worker/tester for the task named in the prompt.
type pauseRunner struct {
	layout      specs.Layout
	refuseAgent string
	refusals    []string // claude result text of each refusal, in order

	// failWorker, when set, is an ordinary (non-limit) worker failure.
	failWorker error

	// cost is the Parsed cost of each call of the agent.
	cost map[string]float64

	calls map[string]int
	// workerSawStatus is the task's STATUS.md status when worker ran.
	workerSawStatus []string
}

func newPauseRunner(layout specs.Layout) *pauseRunner {
	return &pauseRunner{layout: layout, calls: map[string]int{}, cost: map[string]float64{}}
}

func (r *pauseRunner) Run(_ context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	r.calls[a.Name]++
	if a.Name == r.refuseAgent && r.calls[a.Name] <= len(r.refusals) {
		out := limitJSON(r.refusals[r.calls[a.Name]-1])
		return agents.RunResult{ExitCode: 1, Output: out}, fmt.Errorf("agent %s: exit status 1: %s", a.Name, out)
	}
	cost := r.cost[a.Name]
	result := agents.RunResult{TotalCostUSD: cost, Parsed: cost != 0}

	specFile, _, _ := strings.Cut(strings.TrimPrefix(prompt, "Task spec ("), ")")
	num := specs.TaskNumber(specFile)
	switch a.Name {
	case "planner":
		if err := os.WriteFile(filepath.Join(r.layout.SpecDir, num+"_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644); err != nil {
			return result, err
		}
	case "worker":
		status, _ := specs.GetStatus(r.layout, specFile)
		r.workerSawStatus = append(r.workerSawStatus, status)
		if r.failWorker != nil {
			return agents.RunResult{ExitCode: 1, Output: "panic: boom"}, r.failWorker
		}
		if err := os.WriteFile(filepath.Join(dir, num+"_output.txt"), []byte("done by worker\n"), 0o644); err != nil {
			return result, err
		}
	case "tester":
		if err := specs.SetStatus(r.layout, specFile, specs.StatusDone); err != nil {
			return result, err
		}
	}
	return result, nil
}

// interruptPause is an Orchestrator.Sleep for a run stopped (SIGINT) while
// waiting out a limit.
func interruptPause(context.Context, time.Duration) error { return context.Canceled }

// fakeSleep records every pause instead of waiting; onSleep, when set,
// runs first and its error (if any) is returned.
type fakeSleep struct {
	sleeps  []time.Duration
	onSleep func(ctx context.Context, d time.Duration) error
}

func (s *fakeSleep) Sleep(ctx context.Context, d time.Duration) error {
	s.sleeps = append(s.sleeps, d)
	if s.onSleep != nil {
		return s.onSleep(ctx, d)
	}
	return nil
}

// newPauseFixture sets up a project with the given task specs and an
// Orchestrator over runner whose log goes to the returned buffer.
func newPauseFixture(t *testing.T, cfg config.Config, specFiles ...string) (*Orchestrator, *pauseRunner, *fakeSleep, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	for _, f := range specFiles {
		if err := os.WriteFile(filepath.Join(layout.SpecDir, f), []byte("# Example\ndo the thing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg.Dir = dir
	cfg.MaxDiffBytes = config.DefaultMaxDiffBytes
	runner := newPauseRunner(layout)
	sleeper := &fakeSleep{}
	var buf bytes.Buffer
	o := &Orchestrator{
		Cfg:    cfg,
		Layout: layout,
		Runner: runner,
		Log:    log.New(&buf, "", 0),
		Sleep:  sleeper.Sleep,
	}
	return o, runner, sleeper, &buf
}

// newSimulatedPauseFixture is newPauseFixture whose Run goes through the
// whole loop without git, usage.jsonl, run state or run artifacts.
func newSimulatedPauseFixture(t *testing.T, cfg config.Config, specFiles ...string) (*Orchestrator, *pauseRunner, *fakeSleep, *bytes.Buffer) {
	t.Helper()
	o, runner, sleeper, buf := newPauseFixture(t, cfg, specFiles...)
	o.simulate = true
	return o, runner, sleeper, buf
}

// assertNoRestarterTrigger checks that no log line would make
// diadoche-restarter kill diadoche.
func assertNoRestarterTrigger(t *testing.T, logText string) {
	t.Helper()
	for line := range strings.SplitSeq(logText, "\n") {
		if usagelimit.IsLimitLine(line) {
			t.Errorf("log line looks like a claude limit error to diadoche-restarter: %q", line)
		}
	}
}

func TestLimitRefusalsPauseWithoutSpendingAnIteration(t *testing.T) {
	o, runner, sleeper, buf := newSimulatedPauseFixture(t, config.Config{Iter: 3}, "001_example.md")
	runner.refuseAgent = "planner"
	runner.refusals = []string{sessionLimitText, sessionLimitText}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runner.calls["planner"] != 3 || runner.calls["worker"] != 1 || runner.calls["tester"] != 1 {
		t.Errorf("calls = %v, want planner 3, worker 1, tester 1", runner.calls)
	}
	if want := []time.Duration{5 * time.Minute, 10 * time.Minute}; fmt.Sprint(sleeper.sleeps) != fmt.Sprint(want) {
		t.Errorf("sleeps = %v, want %v", sleeper.sleeps, want)
	}
	logText := buf.String()
	if !strings.Contains(logText, "iteration 1/3") || strings.Contains(logText, "iteration 2/3") {
		t.Errorf("expected exactly one iteration to be spent, log:\n%s", logText)
	}
	if got := strings.Count(logText, "paused: claude usage window exhausted"); got != 2 {
		t.Errorf("logged %d pauses, want 2; log:\n%s", got, logText)
	}
	if st := statusRow(t, o.Layout, "001_example.md").Status; st != specs.StatusDone {
		t.Errorf("status = %s, want DONE", st)
	}
	if _, err := os.Stat(filepath.Join(o.Layout.SpecDir, "001_FAILED.md")); err == nil {
		t.Error("001_FAILED.md written for a task that only paused")
	}
	assertNoRestarterTrigger(t, logText)
}

func TestRateLimitIsPausedToo(t *testing.T) {
	o, runner, sleeper, buf := newSimulatedPauseFixture(t, config.Config{Iter: 1}, "001_example.md")
	runner.refuseAgent = "worker"
	runner.refusals = []string{rateLimitText}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if runner.calls["worker"] != 2 || runner.calls["planner"] != 1 {
		t.Errorf("calls = %v, want worker 2, planner 1", runner.calls)
	}
	if fmt.Sprint(sleeper.sleeps) != fmt.Sprint([]time.Duration{5 * time.Minute}) {
		t.Errorf("sleeps = %v, want [5m]", sleeper.sleeps)
	}
	if !strings.Contains(buf.String(), "paused: claude rate limited") {
		t.Errorf("rate-limit pause not logged:\n%s", buf.String())
	}
	if st := statusRow(t, o.Layout, "001_example.md").Status; st != specs.StatusDone {
		t.Errorf("status = %s, want DONE", st)
	}
	assertNoRestarterTrigger(t, buf.String())
}

// sequenceRunner refuses (time-less session limit) or succeeds according
// to script, one entry per call.
type sequenceRunner struct {
	script []bool // true = refuse
	n      int
}

func (r *sequenceRunner) Run(context.Context, string, *agents.Agent, string) (agents.RunResult, error) {
	refuse := r.script[r.n]
	r.n++
	if refuse {
		out := limitJSON(sessionLimitText)
		return agents.RunResult{ExitCode: 1, Output: out}, errors.New("agent planner: exit status 1: " + out)
	}
	return agents.RunResult{}, nil
}

func TestPauseBackoffDoublesUpToAnHourAndResetsAfterSuccess(t *testing.T) {
	layout := specs.NewLayout(t.TempDir())
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	runner := &sequenceRunner{script: []bool{true, true, true, true, true, true, false, true, false}}
	sleeper := &fakeSleep{}
	o := &Orchestrator{
		Cfg:    config.Config{Iter: 1},
		Layout: layout,
		Runner: runner,
		Log:    log.New(io.Discard, "", 0),
		Sleep:  sleeper.Sleep,
	}
	call := agentCall{specFile: "001_example.md", iter: 1, tally: &iterationTally{}}
	a := &agents.Agent{Name: "planner"}
	for range 2 {
		if _, err := o.runAgentPausing(context.Background(), call, a, "p"); err != nil {
			t.Fatal(err)
		}
	}
	m := time.Minute
	want := []time.Duration{5 * m, 10 * m, 20 * m, 40 * m, 60 * m, 60 * m, 5 * m}
	if fmt.Sprint(sleeper.sleeps) != fmt.Sprint(want) {
		t.Errorf("sleeps = %v, want %v", sleeper.sleeps, want)
	}
}

func TestPauseWaitsUntilTheResetTimeFromTheReply(t *testing.T) {
	now := time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)
	o, runner, sleeper, buf := newSimulatedPauseFixture(t, config.Config{Iter: 1}, "001_example.md")
	o.Now = func() time.Time { return now }
	runner.refuseAgent = "planner"
	runner.refusals = []string{sessionLimitText + " · resets 7pm (UTC)"}
	var pausedRow specs.Row
	sleeper.onSleep = func(context.Context, time.Duration) error {
		pausedRow = statusRow(t, o.Layout, "001_example.md")
		return nil
	}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if fmt.Sprint(sleeper.sleeps) != fmt.Sprint([]time.Duration{4 * time.Hour}) {
		t.Errorf("sleeps = %v, want [4h] (until the reset time, not the backoff)", sleeper.sleeps)
	}
	if pausedRow.Status != specs.StatusPaused || !strings.Contains(pausedRow.Reason, "2026-10-08T19:00:00Z") {
		t.Errorf("STATUS.md row during the pause = %+v, want PAUSED with the reset time", pausedRow)
	}
	if fmt.Sprint(runner.workerSawStatus) != fmt.Sprint([]string{specs.StatusInProgress}) {
		t.Errorf("status after the resumed call = %v, want IN_PROGRESS", runner.workerSawStatus)
	}
	if st := statusRow(t, o.Layout, "001_example.md").Status; st != specs.StatusDone {
		t.Errorf("final status = %s, want DONE", st)
	}
	assertNoRestarterTrigger(t, buf.String())
}

func TestPauseWaitsUntilTheLegacyEpochResetTime(t *testing.T) {
	const ts = 1790000000
	o, runner, sleeper, _ := newSimulatedPauseFixture(t, config.Config{Iter: 1}, "001_example.md")
	o.Now = func() time.Time { return time.Unix(ts, 0).Add(-90 * time.Minute) }
	runner.refuseAgent = "tester"
	runner.refusals = []string{fmt.Sprintf("Claude AI usage limit reached|%d", ts)}

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if fmt.Sprint(sleeper.sleeps) != fmt.Sprint([]time.Duration{90 * time.Minute}) {
		t.Errorf("sleeps = %v, want [1h30m]", sleeper.sleeps)
	}
	if runner.calls["tester"] != 2 {
		t.Errorf("tester calls = %d, want 2", runner.calls["tester"])
	}
}

func TestCancelDuringPauseLeavesTaskPaused(t *testing.T) {
	o, runner, sleeper, buf := newSimulatedPauseFixture(t, config.Config{Iter: 2}, "001_example.md")
	runner.refuseAgent = "planner"
	runner.refusals = []string{sessionLimitText}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeper.onSleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}

	err := o.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if runner.calls["planner"] != 1 || runner.calls["worker"] != 0 {
		t.Errorf("calls = %v, want only the refused planner call", runner.calls)
	}
	if st := statusRow(t, o.Layout, "001_example.md").Status; st != specs.StatusPaused {
		t.Errorf("status = %s, want PAUSED", st)
	}
	if _, err := os.Stat(filepath.Join(o.Layout.SpecDir, "001_FAILED.md")); err == nil {
		t.Error("001_FAILED.md written for an interrupted pause")
	}
	_, queue, err := specs.Sync(o.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(queue) != "[001_example.md]" {
		t.Errorf("queue after sync = %v, want the paused task", queue)
	}
	assertNoRestarterTrigger(t, buf.String())
}

func TestDefaultPauseSleepStopsOnCancel(t *testing.T) {
	o := &Orchestrator{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := o.sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleep error = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("cancelled sleep took %s", d)
	}
	if err := o.sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("short sleep: %v", err)
	}
}

func TestOrdinaryAgentErrorStillSpendsTheIteration(t *testing.T) {
	o, runner, sleeper, _ := newSimulatedPauseFixture(t, config.Config{Iter: 2}, "001_example.md")
	runner.failWorker = errors.New("agent worker: exit status 1: panic: boom")

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(sleeper.sleeps) != 0 {
		t.Errorf("sleeps = %v, want none for an ordinary error", sleeper.sleeps)
	}
	if runner.calls["worker"] != 1 {
		t.Errorf("worker calls = %d, want 1", runner.calls["worker"])
	}
	if st := statusRow(t, o.Layout, "001_example.md").Status; st != specs.StatusFailed {
		t.Errorf("status = %s, want FAILED", st)
	}
}

func TestMaxCostPerTaskFailsTheTask(t *testing.T) {
	requireGit(t)
	setTestGitEnv(t)
	o, runner, _, _ := newPauseFixture(t, config.Config{Iter: 3, MaxCostPerTaskUSD: 1}, "001_example.md")
	// $0.90 spent on this task by a previous run.
	prev := `{"time":"2026-10-07T10:00:00Z","task":"001_example.md","agent":"worker","iteration":1,"cost_usd":0.9,"parsed":true}` + "\n"
	if err := os.WriteFile(filepath.Join(o.Layout.Root, "usage.jsonl"), []byte(prev), 0o644); err != nil {
		t.Fatal(err)
	}
	runner.cost["planner"] = 0.2

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if runner.calls["worker"] != 0 {
		t.Errorf("worker calls = %d, want 0 — the task budget is checked right after planner", runner.calls["worker"])
	}
	row := statusRow(t, o.Layout, "001_example.md")
	if row.Status != specs.StatusFailed || !strings.Contains(row.Reason, "budget") {
		t.Errorf("STATUS.md row = %+v, want FAILED with a budget reason", row)
	}
	report, err := os.ReadFile(filepath.Join(o.Layout.SpecDir, "001_FAILED.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "budget") {
		t.Errorf("001_FAILED.md does not mention the budget:\n%s", report)
	}
}

func TestMaxCostPerRunStopsAfterTheCurrentCall(t *testing.T) {
	requireGit(t)
	setTestGitEnv(t)
	o, runner, _, buf := newPauseFixture(t, config.Config{Iter: 3, MaxCostPerRunUSD: 1}, "001_example.md", "002_other.md")
	runner.cost["planner"] = 1.5

	err := o.Run(context.Background())
	if !errors.Is(err, ErrRunBudgetExceeded) {
		t.Fatalf("Run error = %v, want ErrRunBudgetExceeded", err)
	}
	if runner.calls["planner"] != 1 || runner.calls["worker"] != 0 {
		t.Errorf("calls = %v, want only one planner call", runner.calls)
	}
	if st := statusRow(t, o.Layout, "001_example.md").Status; st != specs.StatusInProgress {
		t.Errorf("001 status = %s, want IN_PROGRESS (pending)", st)
	}
	if st := statusRow(t, o.Layout, "002_other.md").Status; st != specs.StatusQueued {
		t.Errorf("002 status = %s, want QUEUED", st)
	}
	if _, err := os.Stat(filepath.Join(o.Layout.SpecDir, "001_FAILED.md")); err == nil {
		t.Error("001_FAILED.md written for a task stopped by the run budget")
	}
	if !strings.Contains(buf.String(), "run budget exhausted") {
		t.Errorf("run budget stop not logged:\n%s", buf.String())
	}
	_, queue, err := specs.Sync(o.Layout)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(queue) != "[001_example.md 002_other.md]" {
		t.Errorf("queue after sync = %v, want both tasks", queue)
	}
}

func TestMaxCostPerRunFinishesATaskThatReachedDone(t *testing.T) {
	requireGit(t)
	setTestGitEnv(t)
	o, runner, _, _ := newPauseFixture(t, config.Config{Iter: 3, MaxCostPerRunUSD: 1}, "001_example.md", "002_other.md")
	runner.cost["tester"] = 1.5

	err := o.Run(context.Background())
	if !errors.Is(err, ErrRunBudgetExceeded) {
		t.Fatalf("Run error = %v, want ErrRunBudgetExceeded", err)
	}
	if st := statusRow(t, o.Layout, "001_example.md").Status; st != specs.StatusDone {
		t.Errorf("001 status = %s, want DONE", st)
	}
	if st := statusRow(t, o.Layout, "002_other.md").Status; st != specs.StatusQueued {
		t.Errorf("002 status = %s, want QUEUED (never started)", st)
	}
	if runner.calls["planner"] != 1 {
		t.Errorf("planner calls = %d, want 1 (task 002 never started)", runner.calls["planner"])
	}
}
