package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/runstate"
	"diadoche/internal/specs"
)

// seqRunner finishes every task like taskRunner, except that tester leaves
// notDoneOnce's task NOT_DONE the first time; calls records every call.
type seqRunner struct {
	layout      specs.Layout
	notDoneOnce string

	mu      sync.Mutex
	calls   []stepCall
	testers map[string]int
}

func (r *seqRunner) Run(_ context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	c := stepCall{spec: promptSpec(prompt), agent: a.Name}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	n := len(r.calls)
	if a.Name == "tester" {
		if r.testers == nil {
			r.testers = map[string]int{}
		}
		r.testers[c.spec]++
	}
	testers := r.testers[c.spec]
	r.mu.Unlock()
	num := specs.TaskNumber(c.spec)
	switch a.Name {
	case "planner":
		return agents.RunResult{}, os.WriteFile(filepath.Join(r.layout.SpecDir, num+"_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644)
	case "worker":
		return agents.RunResult{}, os.WriteFile(filepath.Join(dir, "out-"+num+".txt"), fmt.Appendf(nil, "call %d\n", n), 0o644)
	case "tester":
		if c.spec == r.notDoneOnce && testers == 1 {
			return agents.RunResult{}, nil
		}
		return agents.RunResult{}, specs.SetStatus(r.layout, c.spec, specs.StatusDone)
	}
	return agents.RunResult{}, nil
}

// TestParallelOneReproducesSerialSequence pins the serial loop's exact
// sequence of agent calls and final statuses (captured on the code before
// --parallel existed): --parallel 1, and 0 in a hand-built Config, must
// reproduce it.
func TestParallelOneReproducesSerialSequence(t *testing.T) {
	golden := []stepCall{
		{"001_a.md", "planner"}, {"001_a.md", "worker"}, {"001_a.md", "tester"},
		{"002_b.md", "planner"}, {"002_b.md", "worker"}, {"002_b.md", "tester"},
		{"002_b.md", "planner"}, {"002_b.md", "worker"}, {"002_b.md", "tester"},
		{"003_c.md", "planner"}, {"003_c.md", "worker"}, {"003_c.md", "tester"},
	}
	for _, p := range []int{0, 1} {
		t.Run(fmt.Sprintf("parallel=%d", p), func(t *testing.T) {
			dir, layout := depRepo(t, map[string]string{
				"001_a.md": "# A\n", "002_b.md": "# B\n", "003_c.md": "# C\n",
			}, nil)
			r := &seqRunner{layout: layout, notDoneOnce: "002_b.md"}
			o, buf := bufferedOrchestrator(dir, layout, r, config.Config{Worktree: true, Parallel: p})
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !slices.Equal(r.calls, golden) {
				t.Errorf("calls = %v\nwant    %v", r.calls, golden)
			}
			rows, err := specs.ReadStatus(layout.StatusPath)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, row := range rows {
				got = append(got, row.Spec+"="+row.Status)
			}
			if want := []string{"001_a.md=DONE", "002_b.md=DONE", "003_c.md=DONE"}; !slices.Equal(got, want) {
				t.Errorf("STATUS.md rows = %v, want %v", got, want)
			}
			if strings.Contains(buf.String(), "[0") {
				t.Errorf("serial log lines must not carry a task prefix:\n%s", buf.String())
			}
		})
	}
}

// parRunner plays planner/worker/tester for any number of tasks running at
// once, identified by their prompts: planner writes a valid plan, worker
// out-<num>.txt in its work dir, tester writes the review the prompt asks
// for and marks the task DONE through the guarded o.setStatus. before, when
// set, runs first (n counts the calls of that spec and agent) and may
// replace the call's outcome. cost, when set, gives each call a parsed
// cost. It records every call, the most task spans (first call to tester
// exit) open at once, and how many calls were running at each call start.
type parRunner struct {
	layout specs.Layout
	o      *Orchestrator

	before func(ctx context.Context, c stepCall, dir, prompt string, n int) (*agents.RunResult, error)
	cost   func(c stepCall, n int) float64

	mu        sync.Mutex
	calls     []stepCall
	counts    map[stepCall]int
	open      map[string]bool
	maxOpen   int
	plannerIn map[string][]string // planner prompts per spec
	inCall    int
	maxInCall int
	callConc  []int // inCall right after each call started, in call order
}

var parReviewPathRe = regexp.MustCompile("[^\\s`]+_REVIEW_\\d+\\.md")

func (r *parRunner) Run(ctx context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	c := stepCall{spec: promptSpec(prompt), agent: a.Name}
	r.mu.Lock()
	if r.counts == nil {
		r.counts, r.open, r.plannerIn = map[stepCall]int{}, map[string]bool{}, map[string][]string{}
	}
	r.calls = append(r.calls, c)
	r.counts[c]++
	n := r.counts[c]
	r.inCall++
	r.maxInCall = max(r.maxInCall, r.inCall)
	r.callConc = append(r.callConc, r.inCall)
	if a.Name == "planner" {
		r.plannerIn[c.spec] = append(r.plannerIn[c.spec], prompt)
	}
	if !r.open[c.spec] {
		r.open[c.spec] = true
		open := 0
		for _, o := range r.open {
			if o {
				open++
			}
		}
		r.maxOpen = max(r.maxOpen, open)
	}
	r.mu.Unlock()
	res, err := r.run(ctx, c, dir, prompt, n)
	if r.cost != nil {
		if usd := r.cost(c, n); usd != 0 {
			res.Parsed, res.TotalCostUSD = true, usd
		}
	}
	r.mu.Lock()
	r.inCall--
	if a.Name == "tester" || err != nil {
		r.open[c.spec] = false
	}
	r.mu.Unlock()
	return res, err
}

func (r *parRunner) run(ctx context.Context, c stepCall, dir, prompt string, n int) (agents.RunResult, error) {
	if r.before != nil {
		if res, err := r.before(ctx, c, dir, prompt, n); res != nil || err != nil {
			if res == nil {
				res = &agents.RunResult{}
			}
			return *res, err
		}
	}
	num := specs.TaskNumber(c.spec)
	switch c.agent {
	case "planner":
		return agents.RunResult{}, os.WriteFile(filepath.Join(r.layout.SpecDir, num+"_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644)
	case "worker":
		return agents.RunResult{}, os.WriteFile(filepath.Join(dir, "out-"+num+".txt"), fmt.Appendf(nil, "worker call %d\n", n), 0o644)
	case "tester":
		if p := parReviewPathRe.FindString(prompt); p != "" {
			if err := os.WriteFile(p, []byte("looks good\n"), 0o644); err != nil {
				return agents.RunResult{}, err
			}
		}
		return agents.RunResult{}, r.o.setStatus(c.spec, specs.StatusDone)
	}
	return agents.RunResult{}, nil
}

func (r *parRunner) callsOf(spec string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if c.spec == spec {
			out = append(out, c.agent)
		}
	}
	return out
}

// await waits for ch from any goroutine, failing the test (not hanging it)
// after a deadlock guard.
func await(t *testing.T, ch <-chan struct{}, what string) error {
	select {
	case <-ch:
		return nil
	case <-time.After(10 * time.Second):
		t.Errorf("%s never happened", what)
		return fmt.Errorf("test deadlock guard: %s", what)
	}
}

// gitIn runs git in dir from any goroutine.
func gitIn(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func parOrchestrator(t *testing.T, dir string, layout specs.Layout, r *parRunner, cfg config.Config) (*Orchestrator, *bytes.Buffer) {
	t.Helper()
	cfg.Worktree = true
	o, buf := bufferedOrchestrator(dir, layout, r, cfg)
	r.o = o
	return o, buf
}

func wantStatus(t *testing.T, layout specs.Layout, want string, specFiles ...string) {
	t.Helper()
	for _, s := range specFiles {
		if got := statusRow(t, layout, s); got.Status != want {
			t.Errorf("%s = %s (%s), want %s", s, got.Status, got.Reason, want)
		}
	}
}

func TestParallelRunsAtMostNReadyTasksAndMergesDepsFirst(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: none\n",
		"003_c.md": "# C\nDepends-on: none\n",
		"004_d.md": "# D\nDepends-on: none\n",
		"005_e.md": "# E\nDepends-on: 001, 002\n",
	}, nil)
	three := make(chan struct{})
	var entered atomic.Int32
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, n int) (*agents.RunResult, error) {
		if c.agent != "planner" || n != 1 {
			return nil, nil
		}
		switch c.spec {
		case "001_a.md", "002_b.md", "003_c.md":
			// The first three run at once: none goes on before all three
			// are in.
			if entered.Add(1) == 3 {
				close(three)
			}
			return nil, await(t, three, "three planners at once")
		case "005_e.md":
			for _, f := range []string{"out-001.txt", "out-002.txt"} {
				if !fileExists(filepath.Join(dir, f)) {
					t.Errorf("005 started before %s was merged into the main tree", f)
				}
			}
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 3})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf)
	}
	if r.maxOpen != 3 {
		t.Errorf("at most %d tasks ran at once, want exactly 3", r.maxOpen)
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md", "003_c.md", "004_d.md", "005_e.md")
	started := 0
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "started (model=") {
			started++
			if !strings.HasPrefix(line, "[0") {
				t.Errorf("agent line without a task prefix: %q", line)
			}
		}
	}
	if started != 15 {
		t.Errorf("%d agent calls logged, want 15", started)
	}
}

func TestParallelMergesAreSerialAndInFinishOrder(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: none\n",
		"003_c.md": "# C\nDepends-on: none\n",
	}, nil)
	merged := map[string]chan struct{}{"001_a.md": make(chan struct{}), "002_b.md": make(chan struct{}), "003_c.md": make(chan struct{})}
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, _ int) (*agents.RunResult, error) {
		// Testers finish in the order 002, 003, 001.
		switch {
		case c.agent == "tester" && c.spec == "003_c.md":
			return nil, await(t, merged["002_b.md"], "002's merge")
		case c.agent == "tester" && c.spec == "001_a.md":
			return nil, await(t, merged["003_c.md"], "003's merge")
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 3})
	var inMerge, maxInMerge atomic.Int32
	var mu sync.Mutex
	var order []string
	o.onMerge = func(spec string, enter bool) {
		if enter {
			n := inMerge.Add(1)
			if n > maxInMerge.Load() {
				maxInMerge.Store(n)
			}
			mu.Lock()
			order = append(order, spec)
			mu.Unlock()
			return
		}
		inMerge.Add(-1)
		close(merged[spec])
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf)
	}
	if maxInMerge.Load() != 1 {
		t.Errorf("%d merges overlapped, want 1 at a time", maxInMerge.Load())
	}
	if want := []string{"002_b.md", "003_c.md", "001_a.md"}; !slices.Equal(order, want) {
		t.Errorf("merge order = %v, want the finish order %v", order, want)
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md", "003_c.md")
	for _, f := range []string{"out-001.txt", "out-002.txt", "out-003.txt"} {
		if !fileExists(filepath.Join(dir, f)) {
			t.Errorf("%s not merged into the main tree", f)
		}
	}
}

func TestParallelMergeConflictReexecutesOnceThenBlocks(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: none\n",
		"003_c.md": "# C\nDepends-on: 002\n",
	}, nil)
	merged001 := make(chan struct{})
	var headAtReexec string
	var stateAtReexec *runstate.Task
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, wd, _ string, n int) (*agents.RunResult, error) {
		switch {
		case c.agent == "worker" && c.spec == "001_a.md":
			return nil, os.WriteFile(filepath.Join(wd, "shared.txt"), []byte("from 001\n"), 0o644)
		case c.agent == "worker" && c.spec == "002_b.md" && n == 1:
			return nil, os.WriteFile(filepath.Join(wd, "shared.txt"), []byte("from 002 first\n"), 0o644)
		case c.agent == "worker" && c.spec == "002_b.md":
			if err := os.WriteFile(filepath.Join(wd, "shared.txt"), []byte("from 002 second\n"), 0o644); err != nil {
				return nil, err
			}
			// Meanwhile the main branch moves on with a conflicting edit.
			if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("main edit\n"), 0o644); err != nil {
				return nil, err
			}
			if _, err := gitIn(dir, "add", "shared.txt"); err != nil {
				return nil, err
			}
			_, err := gitIn(dir, "commit", "-q", "-m", "main", "--", "shared.txt")
			return nil, err
		case c.agent == "tester" && c.spec == "002_b.md" && n == 1:
			return nil, await(t, merged001, "001's merge")
		case c.agent == "planner" && c.spec == "002_b.md" && n == 2:
			head, err := gitIn(dir, "rev-parse", "HEAD")
			if err != nil {
				return nil, err
			}
			headAtReexec = head
			st, err := runstate.Load(runstate.Path(layout.Root))
			if err != nil || st == nil {
				return nil, fmt.Errorf("read run-state.json: %v", err)
			}
			for _, rec := range st.Tasks {
				if rec.Spec == "002_b.md" {
					stateAtReexec = &rec
				}
			}
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 2})
	o.onMerge = func(spec string, enter bool) {
		if spec == "001_a.md" && !enter {
			close(merged001)
		}
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf)
	}
	runLog := readRunLog(t, layout)

	// The re-execution, from the new HEAD.
	reexec := eventLines(runLog, "reexec", "task=002_b.md")
	if len(reexec) != 1 {
		t.Fatalf("reexec events = %v, want one", reexec)
	}
	if headAtReexec == "" || !strings.Contains(reexec[0], "base="+headAtReexec) {
		t.Errorf("reexec event %q, want base=%s (main HEAD after 001's merge)", reexec[0], headAtReexec)
	}
	if stateAtReexec == nil || stateAtReexec.BaseCommit != headAtReexec || stateAtReexec.Attempt != 2 || stateAtReexec.Iteration != 1 {
		t.Errorf("run-state.json record of 002 during attempt 2 = %+v, want base %s, attempt 2, iteration 1", stateAtReexec, headAtReexec)
	}
	const conflictBranch = "diadoche/conflict/002-b"
	if prompts := r.plannerIn["002_b.md"]; len(prompts) != 2 || !strings.Contains(prompts[1], "re-executed") || !strings.Contains(prompts[1], conflictBranch) {
		t.Errorf("attempt 2's planner prompt lacks the re-execution note naming %s", conflictBranch)
	}
	if !fileExists(filepath.Join(layout.SpecDir, "002_ATTEMPT1_REVIEW_1.md")) || !fileExists(filepath.Join(layout.SpecDir, "002_ATTEMPT1_IMPL_PLAN.md")) {
		t.Error("the first attempt's plan and review were not archived")
	}

	// The second conflict: BLOCKED, both attempts reachable, dependents
	// BLOCKED too.
	row := statusRow(t, layout, "002_b.md")
	if row.Status != specs.StatusBlocked || !strings.Contains(row.Reason, "conflict") || !strings.Contains(row.Reason, conflictBranch) {
		t.Errorf("002 = %+v, want BLOCKED naming the conflict and %s", row, conflictBranch)
	}
	if got := gitOut(t, dir, "show", conflictBranch+":shared.txt"); got != "from 002 first" {
		t.Errorf("shared.txt on %s = %q, want the first attempt's", conflictBranch, got)
	}
	if got := gitOut(t, dir, "show", "diadoche/002-b:shared.txt"); got != "from 002 second" {
		t.Errorf("shared.txt on diadoche/002-b = %q, want the second attempt's", got)
	}
	if row := statusRow(t, layout, "003_c.md"); row.Status != specs.StatusBlocked || !strings.Contains(row.Reason, "depends on 002_b.md, which is BLOCKED") {
		t.Errorf("003 = %+v, want BLOCKED because of 002", row)
	}
	if calls := r.callsOf("003_c.md"); len(calls) != 0 {
		t.Errorf("003's agents were called: %v", calls)
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md")
	if lines := eventLines(runLog, "merge_blocked", "task=002_b.md", "attempt=2"); len(lines) != 1 {
		t.Errorf("merge_blocked events with attempt=2 = %v, want one", lines)
	}
}

func TestParallelFailureLeavesOthersRunning(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: none\n",
		"003_c.md": "# C\nDepends-on: none\n",
	}, nil)
	in001, in003 := make(chan struct{}), make(chan struct{})
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, _ int) (*agents.RunResult, error) {
		switch {
		case c.agent == "planner" && c.spec == "001_a.md":
			close(in001)
		case c.agent == "planner" && c.spec == "003_c.md":
			close(in003)
		case c.agent == "worker" && c.spec == "002_b.md":
			if err := await(t, in001, "001's planner"); err != nil {
				return nil, err
			}
			if err := await(t, in003, "003's planner"); err != nil {
				return nil, err
			}
			return &agents.RunResult{ExitCode: 1}, errors.New("worker crashed")
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 3})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf)
	}
	wantStatus(t, layout, specs.StatusFailed, "002_b.md")
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "003_c.md")
	if gitFails(dir, "rev-parse", "--verify", "diadoche/failed/002-b") {
		t.Error("002's changes were not kept on diadoche/failed/002-b")
	}
}

func TestParallelConsecutiveFailuresAbortAfterInFlightFinish(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: none\n",
		"003_c.md": "# C\nDepends-on: none\n",
		"004_d.md": "# D\nDepends-on: none\n",
	}, nil)
	threeFailed := make(chan struct{})
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, _ int) (*agents.RunResult, error) {
		switch {
		case c.agent == "worker" && c.spec != "004_d.md":
			return &agents.RunResult{ExitCode: 1}, errors.New("worker crashed")
		case c.agent == "tester" && c.spec == "004_d.md":
			return nil, await(t, threeFailed, "three failed tasks")
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 4})
	var finished atomic.Int32
	o.onFinished = func(spec string) {
		if spec != "004_d.md" && finished.Add(1) == 3 {
			close(threeFailed)
		}
	}
	err := o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "aborting after 3 tasks failed in a row") {
		t.Fatalf("Run = %v, want the consecutive-failures abort\n%s", err, buf)
	}
	wantStatus(t, layout, specs.StatusFailed, "001_a.md", "002_b.md", "003_c.md")
	wantStatus(t, layout, specs.StatusDone, "004_d.md")
	if !fileExists(filepath.Join(dir, "out-004.txt")) {
		t.Error("004 was not merged")
	}
}

func TestParallelRunStateListsActiveTasks(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: none\n",
	}, nil)
	both, release := make(chan struct{}), make(chan struct{})
	var inWorker atomic.Int32
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, _ int) (*agents.RunResult, error) {
		if c.agent == "worker" {
			if inWorker.Add(1) == 2 {
				close(both)
			}
			return nil, await(t, release, "the release")
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 2})
	errc := make(chan error, 1)
	go func() { errc <- o.Run(context.Background()) }()

	if await(t, both, "both workers running") == nil {
		raw, err := os.ReadFile(runstate.Path(layout.Root))
		if err != nil {
			t.Fatal(err)
		}
		var st struct {
			Tasks []runstate.Task `json:"tasks"`
			Task  *runstate.Task  `json:"task"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, rec := range st.Tasks {
			got = append(got, rec.Spec+":"+rec.Agent+":"+rec.AgentState)
		}
		if want := []string{"001_a.md:worker:running", "002_b.md:worker:running"}; !slices.Equal(got, want) {
			t.Errorf("run-state.json tasks = %v, want %v", got, want)
		}
		if st.Task == nil || st.Task.Spec != "001_a.md" {
			t.Errorf("mirrored \"task\" = %+v, want 001's record", st.Task)
		}
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v\n%s", err, buf)
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md")
	if st := readState(t, layout); len(st.Tasks) != 0 {
		t.Errorf("tasks after the run = %+v, want none", st.Tasks)
	}
}

// resumeFixture is two independent tasks a stopped run left IN_PROGRESS:
// each has a valid plan and its worktree on branch diadoche/<NNN>-<slug>,
// made from head, with one uncommitted change.
func resumeFixture(t *testing.T) (dir string, layout specs.Layout, head string) {
	t.Helper()
	dir, layout = depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: none\n",
	}, nil)
	head = gitOut(t, dir, "rev-parse", "HEAD")
	for _, id := range []string{"001-a", "002-b"} {
		wt := filepath.Join(layout.Root, worktreesDirName, id)
		gitOut(t, dir, "worktree", "add", "-q", "-b", "diadoche/"+id, wt, head)
		writeFile(t, filepath.Join(wt, "early-"+id+".txt"), "from the stopped run\n")
		writeFile(t, filepath.Join(layout.SpecDir, id[:3]+"_IMPL_PLAN.md"), validPlanFixture)
	}
	if err := specs.WriteStatus(layout.StatusPath, []specs.Row{
		{Num: "001", Spec: "001_a.md", Status: specs.StatusInProgress},
		{Num: "002", Spec: "002_b.md", Status: specs.StatusInProgress},
	}); err != nil {
		t.Fatal(err)
	}
	return dir, layout, head
}

func TestParallelResumesEveryRecordedTask(t *testing.T) {
	records := func(head string) []runstate.Task {
		return []runstate.Task{
			{Spec: "001_a.md", Num: "001", Iteration: 1, Agent: "tester", AgentState: runstate.AgentRunning, BaseCommit: head, Worktree: ".claude/diadoche/worktrees/001-a"},
			{Spec: "002_b.md", Num: "002", Iteration: 1, Agent: "worker", AgentState: runstate.AgentPending, BaseCommit: head, Worktree: ".claude/diadoche/worktrees/002-b"},
		}
	}
	for _, p := range []int{2, 1} {
		t.Run(fmt.Sprintf("parallel=%d", p), func(t *testing.T) {
			dir, layout, head := resumeFixture(t)
			if err := runstate.Save(runstate.Path(layout.Root), runstate.State{RunID: "old-run", Tasks: records(head)}); err != nil {
				t.Fatal(err)
			}
			r := &parRunner{layout: layout}
			o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: p})
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v\n%s", err, buf)
			}
			if got := r.callsOf("001_a.md"); !slices.Equal(got, []string{"tester"}) {
				t.Errorf("001 calls = %v, want only tester (resumed there)", got)
			}
			if got := r.callsOf("002_b.md"); !slices.Equal(got, []string{"worker", "tester"}) {
				t.Errorf("002 calls = %v, want worker then tester (resumed at worker)", got)
			}
			if p == 1 {
				if got := r.calls[0]; got != (stepCall{"001_a.md", "tester"}) {
					t.Errorf("first call = %v, want 001's tester", got)
				}
			}
			wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md")
			for _, f := range []string{"early-001-a.txt", "early-002-b.txt"} {
				if !fileExists(filepath.Join(dir, f)) {
					t.Errorf("%s from the stopped run was not merged", f)
				}
			}
		})
	}
}

func TestParallelResumesLegacySingleTaskFile(t *testing.T) {
	dir, layout, head := resumeFixture(t)
	legacy := fmt.Sprintf(`{"run_id": "old", "task": {"spec": "001_a.md", "num": "001", "iteration": 1, "agent": "tester", "agent_state": "running", "base_commit": %q, "worktree": ".claude/diadoche/worktrees/001-a"}, "updated_at": "2026-10-08T12:00:00Z"}`, head)
	writeFile(t, runstate.Path(layout.Root), legacy)
	r := &parRunner{layout: layout}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 2})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf)
	}
	if got := r.callsOf("001_a.md"); !slices.Equal(got, []string{"tester"}) {
		t.Errorf("001 calls = %v, want only tester (resumed from the legacy \"task\")", got)
	}
	wantStatus(t, layout, specs.StatusDone, "001_a.md", "002_b.md")
}

func TestParallelStopSavesAllActiveTasks(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: none\n",
	}, nil)
	stop, release := make(chan struct{}), make(chan struct{})
	var inWorker atomic.Int32
	r := &parRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _, _ string, _ int) (*agents.RunResult, error) {
		if c.agent == "worker" {
			if inWorker.Add(1) == 2 {
				close(stop) // the first signal, while both are in worker
				close(release)
			}
			return nil, await(t, release, "the release")
		}
		return nil, nil
	}
	o, buf := parOrchestrator(t, dir, layout, r, config.Config{Parallel: 2})
	o.StopRequested = stop
	err := o.Run(context.Background())
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Run = %v, want ErrInterrupted\n%s", err, buf)
	}
	wantStatus(t, layout, specs.StatusInProgress, "001_a.md", "002_b.md")
	st := readState(t, layout)
	var got []string
	for _, rec := range st.Tasks {
		iter, agent := rec.Next()
		got = append(got, fmt.Sprintf("%s:%d:%s", rec.Spec, iter, agent))
	}
	if want := []string{"001_a.md:1:tester", "002_b.md:1:tester"}; !slices.Equal(got, want) {
		t.Errorf("saved tasks resume at %v, want %v", got, want)
	}
	if n := len(r.callsOf("001_a.md")) + len(r.callsOf("002_b.md")); n != 4 {
		t.Errorf("%d agent calls, want planner and worker of each", n)
	}
	runLog := readRunLog(t, layout)
	if lines := append(eventLines(runLog, "merge"), eventLines(runLog, "park")...); len(lines) != 0 {
		t.Errorf("an interrupted task was merged or parked: %v", lines)
	}
}

func TestParallelDryRunReportsWaves(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: none\n",
		"003_c.md": "# C\nDepends-on: none\n",
		"004_d.md": "# D\nDepends-on: none\n",
		"005_e.md": "# E\nDepends-on: 001, 002\n",
	}, nil)
	statusBefore, _ := os.ReadFile(layout.StatusPath)
	queueBefore, _ := os.ReadFile(layout.QueuePath)
	var prompts atomic.Int32
	runner := funcRunner(func(context.Context, string, *agents.Agent, string) (agents.RunResult, error) {
		prompts.Add(1)
		return agents.RunResult{}, nil
	})
	o, buf := bufferedOrchestrator(dir, layout, runner, config.Config{DryRun: true, Worktree: true, Parallel: 3})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v\n%s", err, buf)
	}
	out := buf.String()
	for _, want := range []string{
		"dry-run: --parallel 3: tasks that could run concurrently (up to 3 at a time):",
		"dry-run:   wave 1: 001_a.md, 002_b.md, 003_c.md, 004_d.md",
		"dry-run:   wave 2: 005_e.md",
		"dry-run: merges would happen one at a time in the order tasks finish",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if prompts.Load() != 15 {
		t.Errorf("%d prompts walked, want 15", prompts.Load())
	}
	statusAfter, _ := os.ReadFile(layout.StatusPath)
	queueAfter, _ := os.ReadFile(layout.QueuePath)
	if string(statusAfter) != string(statusBefore) || string(queueAfter) != string(queueBefore) {
		t.Error("a dry run changed STATUS.md or QUEUE.md")
	}
}
