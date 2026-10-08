package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
)

// independentSpecs is n tasks 001_a.md, 002_b.md, ... with no dependencies.
func independentSpecs(n int) map[string]string {
	out := map[string]string{}
	for i := range n {
		l := string(rune('a' + i))
		out[fmt.Sprintf("%03d_%s.md", i+1, l)] = "# " + strings.ToUpper(l) + "\nDepends-on: none\n"
	}
	return out
}

// limitRefusal is a call refused by claude's session limit, as
// agents.CLIRunner returns it.
func limitRefusal(agent string) (*agents.RunResult, error) {
	out := limitJSON(sessionLimitText)
	return &agents.RunResult{ExitCode: 1, Output: out}, fmt.Errorf("agent %s: exit status 1: %s", agent, out)
}

// waitFor polls cond (test code, not a pause) until it holds, failing the
// test after a deadlock guard.
func waitFor(t *testing.T, what string, cond func() bool) error {
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Errorf("%s never happened", what)
			return fmt.Errorf("test deadlock guard: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

func (r *parRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// runLogText is the run's run.log.
func runLogText(t *testing.T, layout specs.Layout) string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(layout.Root, "runs", "*", "run.log"))
	if err != nil || len(m) != 1 {
		t.Fatalf("run.log files = %v (%v), want one", m, err)
	}
	return readFile(t, m[0])
}

// logLines are the lines of text containing every one of parts.
func logLines(text string, parts ...string) []int {
	var out []int
	for i, line := range strings.Split(text, "\n") {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			out = append(out, i)
		}
	}
	return out
}

func TestParallelLimitPausesTheWholeRunOnce(t *testing.T) {
	dir, layout := depRepo(t, independentSpecs(3), nil)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, n int) (*agents.RunResult, error) {
		switch {
		case c.agent == "planner" && c.spec != "001_a.md" && n == 1:
			// In flight when the limit hits; they end during the pause.
			entered <- struct{}{}
			return nil, await(t, release, "release of the planners")
		case c == stepCall{"001_a.md", "worker"} && n == 1:
			for range 2 {
				<-entered
			}
			return limitRefusal(c.agent)
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 3, Iter: 2})
	sleeper := &fakeSleep{}
	sleeper.onSleep = func(context.Context, time.Duration) error {
		startedBefore := r.callCount()
		close(release)
		// Both planners end and their workers wait at the gate.
		if err := waitFor(t, "two calls waiting at the gate", func() bool { return o.shared().gate.waiters() >= 2 }); err != nil {
			return err
		}
		if got := r.callCount(); got != startedBefore {
			t.Errorf("%d call(s) started while the run was paused", got-startedBefore)
		}
		return nil
	}
	o.Sleep = sleeper.Sleep

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	if len(sleeper.sleeps) != 1 {
		t.Errorf("sleeps = %v, want exactly one", sleeper.sleeps)
	}
	if n := o.shared().gate.pauseCount(); n != 1 {
		t.Errorf("pauses = %d, want 1", n)
	}
	runLog := runLogText(t, layout)
	if n := len(logLines(runLog, "event=pause ")); n != 1 {
		t.Errorf("%d event=pause line(s) in run.log, want 1:\n%s", n, runLog)
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md", "003_c.md")
	assertNoRestarterTrigger(t, buf.String())
}

func TestParallelLimitFollowerJoinsThePause(t *testing.T) {
	dir, layout := depRepo(t, independentSpecs(2), nil)
	var entered atomic.Int32
	both := make(chan struct{})
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, n int) (*agents.RunResult, error) {
		if c.agent == "worker" && n == 1 {
			if entered.Add(1) == 2 {
				close(both)
			}
			if err := await(t, both, "both workers in flight"); err != nil {
				return nil, err
			}
			return limitRefusal(c.agent)
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 2, Iter: 2})
	sleeper := &fakeSleep{}
	sleeper.onSleep = func(context.Context, time.Duration) error {
		// The leader's pause lasts until the other refusal has joined it.
		return waitFor(t, "a follower joining the pause", func() bool { return o.shared().gate.followerCount() >= 1 })
	}
	o.Sleep = sleeper.Sleep

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	if len(sleeper.sleeps) != 1 {
		t.Errorf("sleeps = %v, want exactly one", sleeper.sleeps)
	}
	runLog := runLogText(t, layout)
	if n := len(logLines(runLog, "event=pause ")); n != 1 {
		t.Errorf("%d event=pause line(s), want 1:\n%s", n, runLog)
	}
	if n := len(logLines(runLog, "event=pause_wait")); n != 1 {
		t.Errorf("%d event=pause_wait line(s), want 1:\n%s", n, runLog)
	}
	if !strings.Contains(buf.String(), "paused: the run is already paused, waiting with it") {
		t.Errorf("follower not logged:\n%s", buf.String())
	}
	for _, s := range []string{"001_a.md", "002_b.md"} {
		if got := r.callsOf(s); !slices.Equal(got, []string{"planner", "worker", "worker", "tester"}) {
			t.Errorf("%s calls = %v, want the refused worker retried once", s, got)
		}
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md")
	assertNoRestarterTrigger(t, buf.String())
}

func TestParallelDegradesToOneUntilACallSucceeds(t *testing.T) {
	dir, layout := depRepo(t, independentSpecs(3), nil)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var testers atomic.Int32
	threeTesters := make(chan struct{})
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, n int) (*agents.RunResult, error) {
		switch {
		case c.agent == "planner" && c.spec != "001_a.md" && n == 1:
			entered <- struct{}{}
			return nil, await(t, release, "release of the planners")
		case c == stepCall{"001_a.md", "worker"} && n == 1:
			for range 2 {
				<-entered
			}
			return limitRefusal(c.agent)
		case c.agent == "tester":
			// Back to 3: all three testers are in a call at once.
			if testers.Add(1) == 3 {
				close(threeTesters)
			}
			return nil, await(t, threeTesters, "three testers at once")
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 3, Iter: 2})
	sleeper := &fakeSleep{}
	// startedBefore is how many calls started before the pause ended.
	startedBefore := 0
	sleeper.onSleep = func(context.Context, time.Duration) error {
		close(release)
		err := waitFor(t, "two calls waiting at the gate", func() bool { return o.shared().gate.waiters() >= 2 })
		startedBefore = r.callCount()
		return err
	}
	o.Sleep = sleeper.Sleep

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	if startedBefore == 0 || startedBefore >= len(r.calls) {
		t.Fatalf("calls = %v, %d before the pause ended", r.calls, startedBefore)
	}
	// The first call after the pause runs alone, and it is the one that
	// restores the degree.
	if r.callConc[startedBefore] != 1 {
		t.Errorf("calls running at the first call after the pause = %d, want 1 (calls %v, callConc %v)", r.callConc[startedBefore], r.calls, r.callConc)
	}
	if after := slices.Max(r.callConc[startedBefore:]); after != 3 {
		t.Errorf("most calls at once after recovery = %d, want 3 (callConc %v)", after, r.callConc)
	}
	runLog := runLogText(t, layout)
	down := logLines(runLog, "event=parallelism", "degree=1", "reason=usage_limit")
	up := logLines(runLog, "event=parallelism", "degree=3", "reason=recovered")
	if len(down) != 1 || len(up) != 1 || down[0] > up[0] {
		t.Errorf("parallelism events: degree=1 at %v, degree=3 at %v; want one each, in that order:\n%s", down, up, runLog)
	}
	for _, want := range []string{
		"parallel: a call was refused by a claude limit; effective parallelism 1 until a call succeeds after the pause",
		"parallel: a call succeeded after the pause; effective parallelism back to 3",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md", "003_c.md")
	assertNoRestarterTrigger(t, buf.String())
}

func TestParallelDegradedRunStartsNoNewTask(t *testing.T) {
	dir, layout := depRepo(t, independentSpecs(4), nil)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	finished002 := make(chan struct{})
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, n int) (*agents.RunResult, error) {
		switch {
		case c == stepCall{"002_b.md", "tester"} && n == 1, c == stepCall{"003_c.md", "planner"} && n == 1:
			// In flight when the limit hits; they end during the pause,
			// and 002 finishes while the run is degraded.
			entered <- struct{}{}
			return nil, await(t, release, "release of the in-flight calls")
		case c == stepCall{"001_a.md", "worker"} && n == 1:
			for range 2 {
				<-entered
			}
			return limitRefusal(c.agent)
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 3, Iter: 2})
	o.onFinished = func(spec string) {
		if spec == "002_b.md" {
			close(finished002)
		}
	}
	sleeper := &fakeSleep{}
	sleeper.onSleep = func(context.Context, time.Duration) error {
		close(release)
		return await(t, finished002, "002 finishing during the pause")
	}
	o.Sleep = sleeper.Sleep

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	runLog := runLogText(t, layout)
	up := logLines(runLog, "event=parallelism", "degree=3")
	end002 := logLines(runLog, "task=002_b.md", "event=task_end")
	start004 := logLines(runLog, "task=004_d.md")
	if len(up) != 1 || len(end002) != 1 || len(start004) == 0 {
		t.Fatalf("run.log lacks the events (degree=3 %v, 002 end %v, 004 %v):\n%s", up, end002, start004, runLog)
	}
	if end002[0] > up[0] {
		t.Errorf("002 ended after the recovery; the test needs it to end while degraded:\n%s", runLog)
	}
	if start004[0] < up[0] {
		t.Errorf("004 started at line %d, before the degree went back to 3 at line %d:\n%s", start004[0], up[0], runLog)
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md", "003_c.md", "004_d.md")
}

func TestParallelRunBudgetStopsAtIterationBoundary(t *testing.T) {
	dir, layout := depRepo(t, independentSpecs(4), nil)
	worker001 := make(chan struct{})
	var once sync.Once
	var planners atomic.Int32
	allStarted := make(chan struct{})
	r := &parRunner{layout: layout}
	r.cost = func(c stepCall, n int) float64 {
		if c == (stepCall{"001_a.md", "planner"}) {
			return 1.5
		}
		return 0
	}
	r.before = func(_ context.Context, c stepCall, _, prompt string, n int) (*agents.RunResult, error) {
		if c.agent == "planner" && n == 1 {
			// 001's planner crosses the budget only once the three
			// tasks are all in flight.
			if planners.Add(1) == 3 {
				close(allStarted)
			}
			if err := await(t, allStarted, "three tasks in flight"); err != nil {
				return nil, err
			}
		}
		switch {
		case c == stepCall{"001_a.md", "worker"}:
			// 001's planner cost is on the books by now.
			once.Do(func() { close(worker001) })
		case c.agent == "tester" && c.spec != "001_a.md":
			if err := await(t, worker001, "001's worker"); err != nil {
				return nil, err
			}
			if p := parReviewPathRe.FindString(prompt); p != "" {
				if err := os.WriteFile(p, []byte("not yet\n"), 0o644); err != nil {
					return nil, err
				}
			}
			return &agents.RunResult{}, nil // NOT_DONE
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 3, Iter: 3, MaxCostPerRunUSD: 1})

	err := o.Run(context.Background())
	if !errors.Is(err, ErrRunBudgetExceeded) || !strings.Contains(err.Error(), "run cost") {
		t.Fatalf("Run error = %v, want the run budget's\n%s", err, buf.String())
	}
	if got := r.callsOf("004_d.md"); len(got) != 0 {
		t.Errorf("004 calls = %v, want none", got)
	}
	wantStatus(t, layout, specs.StatusQueued, "004_d.md")
	wantStatus(t, layout, specs.StatusDone, "001_a.md")
	wantStatus(t, layout, specs.StatusInProgress, "002_b.md", "003_c.md")
	for _, s := range []string{"002_b.md", "003_c.md"} {
		if got := r.callsOf(s); !slices.Equal(got, []string{"planner", "worker", "tester"}) {
			t.Errorf("%s calls = %v, want exactly its first iteration", s, got)
		}
		if _, err := os.Stat(filepath.Join(layout.SpecDir, specs.TaskNumber(s)+"_FAILED.md")); err == nil {
			t.Errorf("%s has a FAILED report", s)
		}
	}
	if !strings.Contains(buf.String(), "run budget exhausted") {
		t.Errorf("run budget stop not logged:\n%s", buf.String())
	}
}

func TestParallelTaskBudgetFailsOnlyThatTask(t *testing.T) {
	dir, layout := depRepo(t, independentSpecs(3), nil)
	r := &parRunner{layout: layout}
	r.cost = func(c stepCall, n int) float64 {
		if c == (stepCall{"002_b.md", "worker"}) {
			return 1.5
		}
		return 0.01
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 3, Iter: 2, MaxCostPerTaskUSD: 1})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	if row := statusRow(t, layout, "002_b.md"); row.Status != specs.StatusFailed || !strings.Contains(row.Reason, "budget exceeded") {
		t.Errorf("002 = %+v, want FAILED for its budget", row)
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "003_c.md")
}

func TestParallelUsageAndRunLogUnderRace(t *testing.T) {
	dir, layout := depRepo(t, independentSpecs(3), nil)
	r := &parRunner{layout: layout}
	r.cost = func(stepCall, int) float64 { return 0.01 }
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 3, Iter: 2})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md", "003_c.md")

	want := map[stepCall]int{}
	for _, c := range r.calls {
		want[c]++
	}
	got := map[stepCall]int{}
	lines := strings.Split(strings.TrimSuffix(readFile(t, filepath.Join(layout.Root, "usage.jsonl")), "\n"), "\n")
	if len(lines) != len(r.calls) {
		t.Errorf("usage.jsonl has %d lines, want %d (one per call)", len(lines), len(r.calls))
	}
	for _, line := range lines {
		var rec usage.Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Task == "" || rec.Agent == "" {
			t.Errorf("malformed usage line %q: %v", line, err)
			continue
		}
		got[stepCall{rec.Task, rec.Agent}]++
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("usage records per call = %v, want %v", got, want)
	}

	runLog := runLogText(t, layout)
	for _, line := range strings.Split(strings.TrimSuffix(runLog, "\n"), "\n") {
		if _, err := time.Parse("2006-01-02T15:04:05.000Z07:00", strings.Fields(line + " x")[0]); err != nil || !strings.Contains(line, " event=") {
			t.Errorf("malformed run.log line %q", line)
		}
	}
	if n := len(logLines(runLog, "event=agent_end")); n != len(r.calls) {
		t.Errorf("%d agent_end events, want %d", n, len(r.calls))
	}
	runDirs, _ := filepath.Glob(filepath.Join(layout.Root, "runs", "*"))
	if len(runDirs) != 1 {
		t.Fatalf("run dirs = %v", runDirs)
	}
	for _, id := range []string{"001-a", "002-b", "003-c"} {
		if _, err := os.Stat(filepath.Join(runDirs[0], id, "iter-1-planner.stdout")); err != nil {
			t.Errorf("artifact of %s: %v", id, err)
		}
	}
}

func TestParallelADRNumbersAreDistinct(t *testing.T) {
	dir, layout := depRepo(t, independentSpecs(2), nil)
	seedADRs(t, dir)
	var testers atomic.Int32
	bothTesters := make(chan struct{})
	var adrCalls, adrInFlight, adrMaxInFlight atomic.Int32
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, wdir, _ string, n int) (*agents.RunResult, error) {
		switch c.agent {
		case "worker":
			num := specs.TaskNumber(c.spec)
			if err := os.WriteFile(filepath.Join(wdir, "out-"+num+".txt"), []byte("work\n"), 0o644); err != nil {
				return nil, err
			}
			answer := "Done.\n\n## Decisions\n\n### Decision of task " + num + "\n" +
				"Context: Something forced it.\nDecision: Do it.\nAlternatives: none considered\n"
			return &agents.RunResult{Output: answer}, nil
		case "tester":
			if testers.Add(1) == 2 {
				close(bothTesters)
			}
			return nil, await(t, bothTesters, "both testers at once")
		case "adr-writer":
			in := adrInFlight.Add(1)
			defer adrInFlight.Add(-1)
			for {
				m := adrMaxInFlight.Load()
				if in <= m || adrMaxInFlight.CompareAndSwap(m, in) {
					break
				}
			}
			k := adrCalls.Add(1)
			title := fmt.Sprintf("Parallel decision %d", k)
			p := filepath.Join(wdir, "docs", "adr", fmt.Sprintf("TMP1-parallel-decision-%d.md", k))
			if err := os.WriteFile(p, []byte(adrDoc("TMP1", title, "Accepted", "Do it.", "")), 0o644); err != nil {
				return nil, err
			}
			return &agents.RunResult{Output: "wrote ADRs"}, nil
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 2, Iter: 2, ADR: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf.String())
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md")
	if n := adrCalls.Load(); n != 2 {
		t.Fatalf("adr-writer called %d times, want 2\n%s", n, buf.String())
	}
	if m := adrMaxInFlight.Load(); m != 1 {
		t.Errorf("%d ADR steps overlapped, want them one at a time", m)
	}
	files := adrFiles(t, dir)
	want := []string{"0001-sqlite-index.md", "0002-log-to-stderr.md", "0003-parallel-decision-1.md", "0004-parallel-decision-2.md", "README.md"}
	if !slices.Equal(files, want) {
		t.Fatalf("docs/adr = %v, want %v", files, want)
	}
	index := readFile(t, filepath.Join(dir, "docs", "adr", "README.md"))
	for _, s := range []string{"Parallel decision 1", "Parallel decision 2", "0003-parallel-decision-1.md", "0004-parallel-decision-2.md"} {
		if n := strings.Count(index, s); n != 1 {
			t.Errorf("README.md lists %q %d times, want once:\n%s", s, n, index)
		}
	}
	if strings.Contains(index, "TMP") {
		t.Errorf("README.md mentions a TMP number:\n%s", index)
	}
}

func TestLimitGateStateMachine(t *testing.T) {
	ctx := context.Background()
	g := newLimitGate()
	acquireAsync := func() <-chan gateTicket {
		ch := make(chan gateTicket, 1)
		go func() {
			tk, err := g.acquire(ctx, nil, nil)
			if err != nil {
				t.Errorf("acquire: %v", err)
			}
			ch <- tk
		}()
		return ch
	}
	blocked := func(ch <-chan gateTicket, what string) {
		t.Helper()
		select {
		case <-ch:
			t.Errorf("%s: acquire returned, want it blocked", what)
		case <-time.After(20 * time.Millisecond):
		}
	}
	got := func(ch <-chan gateTicket) gateTicket {
		t.Helper()
		select {
		case tk := <-ch:
			return tk
		case <-time.After(10 * time.Second):
			t.Fatal("acquire never returned")
		}
		return gateTicket{}
	}

	old, err := g.acquire(ctx, nil, nil)
	if err != nil || old.slot {
		t.Fatalf("open acquire = %+v, %v", old, err)
	}
	g.done(old, false)
	if leader, degradedNow, _ := g.refused(); !leader || !degradedNow {
		t.Fatalf("first refusal: leader=%v degradedNow=%v, want both", leader, degradedNow)
	}
	if g.effective(3) != 1 {
		t.Errorf("effective while paused = %d, want 1", g.effective(3))
	}
	first := acquireAsync()
	blocked(first, "paused")
	if err := waitFor(t, "a waiter", func() bool { return g.waiters() == 1 }); err != nil {
		return
	}
	leader, _, wait := g.refused()
	if leader || wait == nil {
		t.Fatal("a refusal while paused must follow the pause")
	}
	g.endPause()
	<-wait
	t1 := got(first)
	if !t1.slot {
		t.Errorf("degraded ticket %+v holds no slot", t1)
	}
	second := acquireAsync()
	blocked(second, "degraded slot taken")
	if g.done(t1, false) {
		t.Error("a failed call recovered the gate")
	}
	t2 := got(second)
	if g.done(old, true) {
		t.Error("a call from before the pause recovered the gate")
	}
	if g.effective(3) != 1 {
		t.Errorf("effective while degraded = %d, want 1", g.effective(3))
	}
	if !g.done(t2, true) {
		t.Error("a success after the pause did not recover the gate")
	}
	if g.effective(3) != 3 {
		t.Errorf("effective after recovery = %d, want 3", g.effective(3))
	}
	select {
	case <-g.wake:
	default:
		t.Error("recovery did not wake the dispatcher")
	}
	if g.pauseCount() != 1 {
		t.Errorf("pauses = %d, want 1", g.pauseCount())
	}

	m := time.Minute
	var waits []time.Duration
	for range 6 {
		waits = append(waits, g.nextBackoff())
	}
	if want := []time.Duration{5 * m, 10 * m, 20 * m, 40 * m, 60 * m, 60 * m}; !slices.Equal(waits, want) {
		t.Errorf("backoffs = %v, want %v", waits, want)
	}
	tk, _ := g.acquire(ctx, nil, nil)
	g.done(tk, true)
	if w := g.nextBackoff(); w != 5*m {
		t.Errorf("backoff after a success = %v, want 5m", w)
	}

	g.refused()
	stop := make(chan struct{})
	close(stop)
	if _, err := g.acquire(ctx, stop, nil); !errors.Is(err, ErrInterrupted) {
		t.Errorf("acquire on stop = %v, want ErrInterrupted", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := g.acquire(cctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("acquire on a cancelled ctx = %v, want context.Canceled", err)
	}
	if g.waiters() != 0 {
		t.Errorf("waiters = %d after every acquire returned", g.waiters())
	}
}
