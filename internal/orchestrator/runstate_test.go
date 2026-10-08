package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/runstate"
	"diadoche/internal/shutdown"
	"diadoche/internal/specs"
)

// funcRunner is an agents.Runner made of one function.
type funcRunner func(ctx context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error)

func (f funcRunner) Run(ctx context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	return f(ctx, dir, a, prompt)
}

var promptSpecRe = regexp.MustCompile(`^Task spec \(([^)]+)\):`)

// promptSpec is the spec file an agent prompt is about.
func promptSpec(prompt string) string {
	if m := promptSpecRe.FindStringSubmatch(prompt); m != nil {
		return m[1]
	}
	return ""
}

// stepCall is one agent call a taskRunner saw.
type stepCall struct{ spec, agent string }

// taskRunner plays planner/worker/tester for any spec: planner writes a
// valid plan, worker writes out-<num>.txt, tester marks the spec DONE. The
// hooks run first and may replace the call's outcome.
type taskRunner struct {
	layout specs.Layout

	mu      sync.Mutex
	calls   []stepCall
	prompts []string

	// before, when set, runs at the start of every call; a non-nil
	// result short-circuits the call with that outcome.
	before func(ctx context.Context, c stepCall, n int) (*agents.RunResult, error)

	// testerNotDone keeps tester from marking the task DONE.
	testerNotDone bool
}

func (r *taskRunner) count(agent string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.agent == agent {
			n++
		}
	}
	return n
}

func (r *taskRunner) Run(ctx context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	c := stepCall{spec: promptSpec(prompt), agent: a.Name}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
	n := r.count(a.Name)
	if r.before != nil {
		if res, err := r.before(ctx, c, n); res != nil || err != nil {
			if res == nil {
				res = &agents.RunResult{}
			}
			return *res, err
		}
	}
	num := specs.TaskNumber(c.spec)
	switch a.Name {
	case "planner":
		return agents.RunResult{}, os.WriteFile(filepath.Join(r.layout.SpecDir, num+"_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644)
	case "worker":
		return agents.RunResult{}, os.WriteFile(filepath.Join(dir, "out-"+num+".txt"), fmt.Appendf(nil, "worker call %d\n", n), 0o644)
	case "tester":
		if r.testerNotDone {
			return agents.RunResult{}, nil
		}
		return agents.RunResult{}, specs.SetStatus(r.layout, c.spec, specs.StatusDone)
	}
	return agents.RunResult{}, nil
}

func readState(t *testing.T, layout specs.Layout) *runstate.State {
	t.Helper()
	s, err := runstate.Load(runstate.Path(layout.Root))
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("run-state.json does not exist")
	}
	return s
}

func writeState(t *testing.T, layout specs.Layout, task *runstate.Task) {
	t.Helper()
	if err := runstate.Save(runstate.Path(layout.Root), runstate.State{RunID: "old-run", Task: task, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func bufferedOrchestrator(dir string, layout specs.Layout, runner agents.Runner, cfg config.Config) (*Orchestrator, *bytes.Buffer) {
	o := newTestOrchestrator(dir, layout, runner, cfg)
	var buf bytes.Buffer
	o.Log = log.New(&lockedWriter{w: &buf}, "", 0)
	return o, &buf
}

// lockedWriter serializes writes from the log and the shutdown goroutine.
type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func assertInterruptedNotParked(t *testing.T, err error, layout specs.Layout) {
	t.Helper()
	if !errors.Is(err, ErrInterrupted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want ErrInterrupted (a context.Canceled)", err)
	}
	if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusInProgress {
		t.Errorf("status = %s, want %s", got, specs.StatusInProgress)
	}
	if fileExists(filepath.Join(layout.SpecDir, "001_FAILED.md")) {
		t.Error("001_FAILED.md exists: an interrupted task must not be parked")
	}
}

func TestResumeContinuesAtTesterOfSameIteration(t *testing.T) {
	dir, layout := seedRepo(t)
	head := gitOut(t, dir, "rev-parse", "HEAD")
	cfg := config.Config{Iter: 3, Worktree: false}

	stop := make(chan struct{})
	run1 := &taskRunner{layout: layout, testerNotDone: true}
	run1.before = func(_ context.Context, c stepCall, n int) (*agents.RunResult, error) {
		if c.agent == "tester" {
			writeFile(t, filepath.Join(layout.SpecDir, fmt.Sprintf("001_REVIEW_%d.md", n)), "not yet\n")
		}
		if c.agent == "worker" && n == 2 {
			close(stop) // the first signal arrives during worker's 2nd call
		}
		return nil, nil
	}
	o := newTestOrchestrator(dir, layout, run1, cfg)
	o.StopRequested = stop
	assertInterruptedNotParked(t, o.Run(context.Background()), layout)

	st := readState(t, layout)
	if st.Task == nil {
		t.Fatal("state has no task after the interrupt")
	}
	if got := *st.Task; got.Spec != "001_example.md" || got.Num != "001" || got.Iteration != 2 || got.Agent != "worker" || got.AgentState != runstate.AgentDone || got.BaseCommit != head {
		t.Fatalf("state = %+v, want 001 iteration 2 worker done on base %s", got, head)
	}
	if run1.count("tester") != 1 {
		t.Fatalf("run 1 tester calls = %d, want 1", run1.count("tester"))
	}

	run2 := &taskRunner{layout: layout}
	o2 := newTestOrchestrator(dir, layout, run2, cfg)
	if err := o2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p, w, ts := run2.count("planner"), run2.count("worker"), run2.count("tester"); p != 0 || w != 0 || ts != 1 {
		t.Fatalf("run 2 calls planner=%d worker=%d tester=%d, want 0/0/1", p, w, ts)
	}
	if !strings.Contains(run2.prompts[0], "001_REVIEW_2.md") {
		t.Error("run 2's tester prompt doesn't ask for 001_REVIEW_2.md: not iteration 2")
	}
	if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusDone {
		t.Errorf("status = %s, want DONE", got)
	}
	if msg := gitOut(t, dir, "log", "-1", "--format=%s"); msg != "diadoche: complete 001_example.md" {
		t.Errorf("last commit = %q, want the task's DONE commit", msg)
	}
	if st := readState(t, layout); st.Task != nil {
		t.Errorf("final state task = %+v, want null", st.Task)
	}
}

func TestRunStateRecordsEveryStep(t *testing.T) {
	dir, layout := seedRepo(t)
	head := gitOut(t, dir, "rev-parse", "HEAD")
	statePath := runstate.Path(layout.Root)

	r := &taskRunner{layout: layout}
	var seen []string
	r.before = func(_ context.Context, c stepCall, _ int) (*agents.RunResult, error) {
		raw, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatalf("%s call: %v", c.agent, err)
		}
		if !json.Valid(raw) {
			t.Fatalf("%s call: run-state.json is not valid JSON: %s", c.agent, raw)
		}
		st := readState(t, layout)
		if st.RunID == "" || st.UpdatedAt.IsZero() || st.Task == nil {
			t.Fatalf("%s call: state = %+v", c.agent, st)
		}
		want := runstate.Task{
			Spec: "001_example.md", Num: "001", Iteration: 1, Agent: c.agent, AgentState: runstate.AgentRunning,
			BaseCommit: head, Worktree: ".claude/diadoche/worktrees/001-example",
		}
		if *st.Task != want {
			t.Errorf("%s call: state task = %+v, want %+v", c.agent, *st.Task, want)
		}
		seen = append(seen, c.agent)
		return nil, nil
	}
	o := newTestOrchestrator(dir, layout, r, config.Config{Worktree: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "planner,worker,tester" {
		t.Errorf("calls = %v", seen)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("run-state.json gone after the run: %v", err)
	}
	if !strings.Contains(string(raw), `"task": null`) || readState(t, layout).Task != nil {
		t.Errorf("final state = %s, want task null", raw)
	}
}

func TestRunStateRecordsAFinishedPlanner(t *testing.T) {
	dir, layout := seedRepo(t)
	head := gitOut(t, dir, "rev-parse", "HEAD")
	stop := make(chan struct{})
	r := &taskRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _ int) (*agents.RunResult, error) {
		if c.agent == "planner" {
			close(stop)
		}
		return nil, nil
	}
	o := newTestOrchestrator(dir, layout, r, config.Config{Worktree: true})
	o.StopRequested = stop
	assertInterruptedNotParked(t, o.Run(context.Background()), layout)
	if r.count("worker") != 0 {
		t.Error("worker ran after the stop request")
	}
	want := runstate.Task{
		Spec: "001_example.md", Num: "001", Iteration: 1, Agent: "planner", AgentState: runstate.AgentDone,
		BaseCommit: head, Worktree: ".claude/diadoche/worktrees/001-example",
	}
	if st := readState(t, layout); st.Task == nil || *st.Task != want {
		t.Errorf("state task = %+v, want %+v", st.Task, want)
	}
}

func TestRunStateWriteIsAtomic(t *testing.T) {
	dir, layout := seedRepo(t)
	statePath := runstate.Path(layout.Root)
	var handle *os.File
	r := &taskRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _ int) (*agents.RunResult, error) {
		if c.agent == "planner" {
			f, err := os.Open(statePath)
			if err != nil {
				t.Fatal(err)
			}
			handle = f
		}
		return nil, nil
	}
	o := newTestOrchestrator(dir, layout, r, config.Config{})
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if handle == nil {
		t.Fatal("planner was never called")
	}
	defer handle.Close()
	oldInfo, err := handle.Stat()
	if err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(oldInfo, newInfo) {
		t.Error("run-state.json was rewritten in place; want each save to rename a temp file over it")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(layout.Root, ".run-state.json.tmp*")); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

// twoTaskRepo is seedRepo plus a second spec 002_other.md.
func twoTaskRepo(t *testing.T) (string, specs.Layout) {
	dir, layout := seedRepo(t)
	writeFile(t, filepath.Join(layout.SpecDir, "002_other.md"), "# Other\n")
	return dir, layout
}

func callList(calls []stepCall) string {
	var parts []string
	for _, c := range calls {
		parts = append(parts, specs.TaskNumber(c.spec)+":"+c.agent)
	}
	return strings.Join(parts, " ")
}

func TestFreshIgnoresSavedState(t *testing.T) {
	saved := &runstate.Task{Spec: "002_other.md", Num: "002", Iteration: 2, Agent: "tester", AgentState: runstate.AgentRunning}

	t.Run("fresh", func(t *testing.T) {
		dir, layout := twoTaskRepo(t)
		writeState(t, layout, saved)
		r := &taskRunner{layout: layout}
		o, logBuf := bufferedOrchestrator(dir, layout, r, config.Config{Fresh: true})
		if err := o.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := "001:planner 001:worker 001:tester 002:planner 002:worker 002:tester"
		if got := callList(r.calls); got != want {
			t.Errorf("calls = %s, want %s", got, want)
		}
		if !strings.Contains(logBuf.String(), "--fresh") {
			t.Errorf("log doesn't mention --fresh:\n%s", logBuf)
		}
		if st := readState(t, layout); st.RunID == "old-run" || st.Task != nil {
			t.Errorf("state = %+v (task %+v), want a new run id and no task", st, st.Task)
		}
	})

	t.Run("without fresh", func(t *testing.T) {
		dir, layout := twoTaskRepo(t)
		head := gitOut(t, dir, "rev-parse", "HEAD")
		// 002 got through planner and worker in the run that stopped.
		writeFile(t, filepath.Join(layout.SpecDir, "002_IMPL_PLAN.md"), validPlanFixture)
		writeFile(t, filepath.Join(dir, "out-002.txt"), "worker of the earlier run\n")
		withBase := *saved
		withBase.BaseCommit = head
		writeState(t, layout, &withBase)

		r := &taskRunner{layout: layout}
		o := newTestOrchestrator(dir, layout, r, config.Config{})
		if err := o.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := "002:tester 001:planner 001:worker 001:tester"
		if got := callList(r.calls); got != want {
			t.Errorf("calls = %s, want %s", got, want)
		}
		if !strings.Contains(r.prompts[0], "002_REVIEW_2.md") {
			t.Error("002's tester didn't run in iteration 2")
		}
	})
}

func TestStateContradictingStatusIsIgnored(t *testing.T) {
	cases := []struct {
		name string
		task runstate.Task
	}{
		{"task already DONE", runstate.Task{Spec: "001_example.md", Num: "001", Iteration: 2, Agent: "tester", AgentState: runstate.AgentRunning}},
		{"task unknown", runstate.Task{Spec: "999_ghost.md", Num: "999", Iteration: 2, Agent: "tester", AgentState: runstate.AgentRunning}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, layout := twoTaskRepo(t)
			if err := specs.WriteStatus(layout.StatusPath, []specs.Row{
				{Num: "001", Spec: "001_example.md", Status: specs.StatusDone},
				{Num: "002", Spec: "002_other.md", Status: specs.StatusQueued},
			}); err != nil {
				t.Fatal(err)
			}
			task := c.task
			writeState(t, layout, &task)

			r := &taskRunner{layout: layout}
			o, logBuf := bufferedOrchestrator(dir, layout, r, config.Config{})
			if err := o.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(logBuf.String(), "warning: ignoring run-state.json") {
				t.Errorf("no warning in the log:\n%s", logBuf)
			}
			if got, want := callList(r.calls), "002:planner 002:worker 002:tester"; got != want {
				t.Errorf("calls = %s, want %s", got, want)
			}
			for _, p := range r.prompts {
				if strings.Contains(p, "001_example.md") {
					t.Error("an agent was called about the DONE task 001")
				}
			}
		})
	}
}

// signalFixture runs one task whose worker call is controlled by the test,
// with signals fed through shutdown.Watch as main does.
type signalFixture struct {
	layout       specs.Layout
	o            *Orchestrator
	runner       *taskRunner
	sigs         chan os.Signal
	stop         <-chan struct{}
	ctx          context.Context
	workerCalled chan struct{}
}

func newSignalFixture(t *testing.T, timeout time.Duration, worker func(ctx context.Context, f *signalFixture) (*agents.RunResult, error)) *signalFixture {
	dir, layout := seedRepo(t)
	f := &signalFixture{layout: layout, sigs: make(chan os.Signal, 4), workerCalled: make(chan struct{})}
	f.runner = &taskRunner{layout: layout}
	f.runner.before = func(ctx context.Context, c stepCall, _ int) (*agents.RunResult, error) {
		if c.agent != "worker" {
			return nil, nil
		}
		close(f.workerCalled)
		return worker(ctx, f)
	}
	f.o = newTestOrchestrator(dir, layout, f.runner, config.Config{})
	ctx, stop, release := shutdown.Watch(context.Background(), f.sigs, timeout, f.o.Log.Printf)
	t.Cleanup(release)
	f.ctx, f.stop = ctx, stop
	f.o.StopRequested = stop
	return f
}

// runAndSignal runs the loop, sending n SIGINTs once worker was called.
func (f *signalFixture) runAndSignal(t *testing.T, n int) error {
	go func() {
		select {
		case <-f.workerCalled:
		case <-time.After(10 * time.Second):
			return
		}
		for range n {
			f.sigs <- syscall.SIGINT
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return f.o.Run(f.ctx)
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never happened", what)
	}
}

func TestSignalDuringAgentCallFinishesCallThenStops(t *testing.T) {
	var killed bool
	f := newSignalFixture(t, time.Hour, func(ctx context.Context, f *signalFixture) (*agents.RunResult, error) {
		waitClosed(t, f.stop, "the stop request")
		time.Sleep(100 * time.Millisecond)
		killed = ctx.Err() != nil
		return nil, nil // the default worker writes its file
	})
	err := f.runAndSignal(t, 1)
	assertInterruptedNotParked(t, err, f.layout)
	if killed {
		t.Error("the worker call's ctx was cancelled by the first signal; want the call to finish")
	}
	if !fileExists(filepath.Join(f.o.Cfg.Dir, "out-001.txt")) {
		t.Error("worker's file is missing: the call wasn't finished")
	}
	if n := f.runner.count("tester"); n != 0 {
		t.Errorf("tester called %d times after the signal", n)
	}
	if st := readState(t, f.layout); st.Task == nil || st.Task.Agent != "worker" || st.Task.AgentState != runstate.AgentDone {
		t.Errorf("state task = %+v, want worker done", st.Task)
	}
}

func TestShutdownTimeoutKillsCallAndKeepsAgentRunning(t *testing.T) {
	f := newSignalFixture(t, 50*time.Millisecond, func(ctx context.Context, _ *signalFixture) (*agents.RunResult, error) {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Error("the worker call was never killed")
		}
		return &agents.RunResult{ExitCode: -1}, errors.New("signal: killed")
	})
	err := f.runAndSignal(t, 1)
	assertInterruptedNotParked(t, err, f.layout)
	if st := readState(t, f.layout); st.Task == nil || st.Task.Agent != "worker" || st.Task.AgentState != runstate.AgentRunning {
		t.Fatalf("state task = %+v, want worker running", st.Task)
	}

	r := &taskRunner{layout: f.layout}
	o := newTestOrchestrator(f.o.Cfg.Dir, f.layout, r, config.Config{})
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := callList(r.calls), "001:worker 001:tester"; got != want {
		t.Errorf("follow-up calls = %s, want %s", got, want)
	}
	if got := statusRow(t, f.layout, "001_example.md").Status; got != specs.StatusDone {
		t.Errorf("status = %s, want DONE", got)
	}
}

func TestSecondSignalStopsImmediately(t *testing.T) {
	var waited time.Duration
	f := newSignalFixture(t, time.Hour, func(ctx context.Context, f *signalFixture) (*agents.RunResult, error) {
		waitClosed(t, f.stop, "the stop request")
		start := time.Now()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		waited = time.Since(start)
		return &agents.RunResult{ExitCode: -1}, errors.New("signal: killed")
	})
	err := f.runAndSignal(t, 2)
	assertInterruptedNotParked(t, err, f.layout)
	if waited > time.Second {
		t.Errorf("the call was killed %s after the first signal, want at once on the second", waited)
	}
	if st := readState(t, f.layout); st.Task == nil || st.Task.Agent != "worker" || st.Task.AgentState != runstate.AgentRunning {
		t.Errorf("state task = %+v, want worker running", st.Task)
	}
}

func TestDryRunWritesNoRunState(t *testing.T) {
	dir, layout := seedRepo(t)
	r := &scriptedRunner{layout: layout, onPlanner: writeValidPlan, markDoneOnTester: true}
	o := newTestOrchestrator(dir, layout, r, config.Config{DryRun: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fileExists(runstate.Path(layout.Root)) {
		t.Error("--dry-run wrote run-state.json")
	}
}

func TestStateBehindReviewsFallsBackToFiles(t *testing.T) {
	dir, layout := seedRepo(t)
	writeFile(t, filepath.Join(layout.SpecDir, "001_IMPL_PLAN.md"), validPlanFixture)
	writeFile(t, filepath.Join(layout.SpecDir, "001_REVIEW_1.md"), "no\n")
	writeFile(t, filepath.Join(layout.SpecDir, "001_REVIEW_2.md"), "still no\n")
	writeState(t, layout, &runstate.Task{Spec: "001_example.md", Num: "001", Iteration: 1, Agent: "planner", AgentState: runstate.AgentRunning})

	r := &taskRunner{layout: layout}
	o, logBuf := bufferedOrchestrator(dir, layout, r, config.Config{Iter: 3})
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logBuf.String(), "run-state.json is behind the reviews on disk") {
		t.Errorf("no warning in the log:\n%s", logBuf)
	}
	if got, want := callList(r.calls), "001:worker 001:tester"; got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
	if !strings.Contains(r.prompts[len(r.prompts)-1], "001_REVIEW_3.md") {
		t.Error("tester didn't run in iteration 3")
	}
}
