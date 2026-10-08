package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/runlog"
	"diadoche/internal/specs"
)

// artifactNow is the fixed clock of the artifact tests, so the run id is
// known up front.
var artifactNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local)

const artifactRunID = "20261008-120000"

// artifactRunner is a fake claude for task 001_example.md: planner writes
// a plan naming the iteration, worker writes feature.txt, tester writes the
// iteration's review and marks the task DONE from iteration doneAt on.
// Every call returns stdout/stderr naming the agent and iteration.
// onCall, when set, runs first on every call.
type artifactRunner struct {
	layout specs.Layout
	doneAt int
	calls  map[string]int
	onCall func(a *agents.Agent, n int)
	total  int
}

func newArtifactRunner(layout specs.Layout, doneAt int) *artifactRunner {
	return &artifactRunner{layout: layout, doneAt: doneAt, calls: map[string]int{}}
}

func (r *artifactRunner) Run(_ context.Context, dir string, a *agents.Agent, _ string) (agents.RunResult, error) {
	r.calls[a.Name]++
	r.total++
	iter := r.calls[a.Name]
	if r.onCall != nil {
		r.onCall(a, r.total)
	}
	result := agents.RunResult{
		Stdout:       fmt.Sprintf(`{"result":"%s %d","total_cost_usd":0.01}`+"\n", a.Name, iter),
		Stderr:       "err " + a.Name,
		Output:       fmt.Sprintf("%s %d", a.Name, iter),
		Parsed:       true,
		TotalCostUSD: 0.01,
		DurationMS:   1500,
	}
	switch a.Name {
	case "planner":
		plan := fmt.Sprintf("%s\nplan of iteration %d\n", validPlanFixture, iter)
		if err := os.WriteFile(filepath.Join(r.layout.SpecDir, "001_IMPL_PLAN.md"), []byte(plan), 0o644); err != nil {
			return result, err
		}
	case "worker":
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte(fmt.Sprintf("feature v%d\n", iter)), 0o644); err != nil {
			return result, err
		}
	case "tester":
		review := filepath.Join(r.layout.SpecDir, fmt.Sprintf("001_REVIEW_%d.md", iter))
		if err := os.WriteFile(review, []byte(fmt.Sprintf("review of iteration %d\n", iter)), 0o644); err != nil {
			return result, err
		}
		if iter >= r.doneAt {
			if err := specs.SetStatus(r.layout, "001_example.md", specs.StatusDone); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func runLogPath(layout specs.Layout) string {
	matches, _ := filepath.Glob(filepath.Join(layout.Root, "runs", "*", runlog.LogName))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

func readRunLog(t *testing.T, layout specs.Layout) string {
	t.Helper()
	p := runLogPath(layout)
	if p == "" {
		t.Fatal("no run.log under runs/")
	}
	return readFile(t, p)
}

// eventLines returns the run.log lines whose event is name and that
// contain every one of the given fragments.
func eventLines(log, name string, fragments ...string) []string {
	var out []string
	for line := range strings.SplitSeq(log, "\n") {
		if !strings.Contains(line, " event="+name+" ") && !strings.HasSuffix(line, " event="+name) {
			continue
		}
		ok := true
		for _, f := range fragments {
			ok = ok && strings.Contains(line, f)
		}
		if ok {
			out = append(out, line)
		}
	}
	return out
}

func requireEvent(t *testing.T, log, name string, fragments ...string) {
	t.Helper()
	if len(eventLines(log, name, fragments...)) == 0 {
		t.Errorf("run.log has no %s event with %v:\n%s", name, fragments, log)
	}
}

// eventNames is the event of every run.log line, in order.
func eventNames(log string) []string {
	var names []string
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		for f := range strings.FieldsSeq(line) {
			if name, ok := strings.CutPrefix(f, "event="); ok {
				names = append(names, name)
				break
			}
		}
	}
	return names
}

func TestRunWritesTaskArtifacts(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree=%v", worktree), func(t *testing.T) {
			dir, layout := seedRepo(t)
			runner := newArtifactRunner(layout, 2)
			o := newTestOrchestrator(dir, layout, runner, config.Config{Iter: 2, Worktree: worktree})
			o.Now = func() time.Time { return artifactNow }

			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertStatus(t, layout, specs.StatusDone)

			taskDir := filepath.Join(layout.Root, "runs", artifactRunID, "001-example")
			for iter := 1; iter <= 2; iter++ {
				if got := readFile(t, filepath.Join(taskDir, fmt.Sprintf("iter-%d-IMPL_PLAN.md", iter))); !strings.Contains(got, fmt.Sprintf("plan of iteration %d", iter)) {
					t.Errorf("iter-%d-IMPL_PLAN.md = %q", iter, got)
				}
				if got := readFile(t, filepath.Join(taskDir, fmt.Sprintf("iter-%d-REVIEW.md", iter))); got != fmt.Sprintf("review of iteration %d\n", iter) {
					t.Errorf("iter-%d-REVIEW.md = %q", iter, got)
				}
				for _, agent := range []string{"planner", "worker", "tester"} {
					base := filepath.Join(taskDir, fmt.Sprintf("iter-%d-%s", iter, agent))
					wantOut := fmt.Sprintf(`{"result":"%s %d","total_cost_usd":0.01}`+"\n", agent, iter)
					if got := readFile(t, base+".stdout"); got != wantOut {
						t.Errorf("%s.stdout = %q, want %q", base, got, wantOut)
					}
					if got := readFile(t, base+".stderr"); got != "err "+agent {
						t.Errorf("%s.stderr = %q", base, got)
					}
				}
			}
			diff := readFile(t, filepath.Join(taskDir, "final.diff"))
			if !strings.Contains(diff, "feature.txt") || !strings.Contains(diff, "+feature v2") {
				t.Errorf("final.diff = %q, want the worker's feature.txt", diff)
			}
			if strings.Contains(diff, ".claude/diadoche") {
				t.Errorf("final.diff includes diadoche's own files:\n%s", diff)
			}

			if st := gitOut(t, dir, "status", "--porcelain", "--untracked-files=all"); strings.Contains(st, "runs/") {
				t.Errorf("runs/ shows up in git status:\n%s", st)
			}
			if lg := gitOut(t, dir, "log", "--all", "--name-only", "--format="); strings.Contains(lg, "runs/") {
				t.Errorf("runs/ was committed:\n%s", lg)
			}
		})
	}
}

func TestRunLogEventsInOrderAndFlushedImmediately(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := newArtifactRunner(layout, 1)
	var checks []string
	runner.onCall = func(a *agents.Agent, n int) {
		b, err := os.ReadFile(runLogPath(layout))
		if err != nil {
			checks = append(checks, fmt.Sprintf("call %d: read run.log: %v", n, err))
			return
		}
		log := string(b)
		if got := len(eventLines(log, "agent_start")); got != n {
			checks = append(checks, fmt.Sprintf("call %d (%s): %d agent_start lines in run.log, want %d", n, a.Name, got, n))
		}
		if len(eventLines(log, "agent_start", "agent="+a.Name)) == 0 {
			checks = append(checks, fmt.Sprintf("call %d: own agent_start of %s not in run.log yet", n, a.Name))
		}
		if got := len(eventLines(log, "agent_end")); got != n-1 {
			checks = append(checks, fmt.Sprintf("call %d (%s): %d agent_end lines in run.log, want %d", n, a.Name, got, n-1))
		}
	}
	o, buf := bufferedOrchestrator(dir, layout, runner, config.Config{Iter: 1})
	o.Now = func() time.Time { return artifactNow }

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.total != 3 {
		t.Fatalf("agent calls = %d, want 3", runner.total)
	}
	for _, c := range checks {
		t.Error(c)
	}

	log := readRunLog(t, layout)
	want := []string{"run_start", "task_start", "status",
		"agent_start", "agent_end", "agent_start", "agent_end", "agent_start", "agent_end",
		"verdict", "status", "commit", "task_end", "run_end"}
	if got := eventNames(log); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v\nwant     %v\n%s", got, want, log)
	}
	requireEvent(t, log, "status", "status=IN_PROGRESS")
	requireEvent(t, log, "verdict", "iter=1", "agent=tester", "verdict=DONE", "review=001_REVIEW_1.md")
	requireEvent(t, log, "status", "status=DONE", "by=tester")
	requireEvent(t, log, "task_end", "task=001_example.md", "status=DONE")
	requireEvent(t, log, "run_end", "result=ok")
	requireEvent(t, log, "run_start", "run="+artifactRunID)
	for _, line := range eventLines(log, "agent_end") {
		for _, f := range []string{"duration=1.5s", "exit=0", "cost=$0.0100", "result=ok"} {
			if !strings.Contains(line, f) {
				t.Errorf("agent_end line lacks %s: %q", f, line)
			}
		}
	}
	// Every run.log line is echoed to the log as well.
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		if !strings.Contains(buf.String(), "event: "+line+"\n") {
			t.Errorf("run.log line not echoed to the log: %q", line)
		}
	}
}

func TestRunLogPauseEvents(t *testing.T) {
	requireGit(t)
	setTestGitEnv(t)
	o, runner, _, buf := newPauseFixture(t, config.Config{Iter: 3}, "001_example.md")
	runner.refuseAgent = "worker"
	runner.refusals = []string{sessionLimitText}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	log := readRunLog(t, o.Layout)
	requireEvent(t, log, "status", "agent=worker", "status=PAUSED")
	requireEvent(t, log, "pause", "agent=worker", "kind=", "wait=")
	requireEvent(t, log, "status", "agent=worker", "status=IN_PROGRESS")
	requireEvent(t, log, "resume", "agent=worker")
	requireEvent(t, log, "task_end", "status=DONE")
	// Both worker calls keep their output.
	taskDir := filepath.Join(runLogPath(o.Layout), "..", "001-example")
	for _, f := range []string{"iter-1-worker.stdout", "iter-1-worker-2.stdout"} {
		if _, err := os.Stat(filepath.Join(taskDir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	assertNoRestarterTrigger(t, buf.String())
}

func TestRunLogParkEvents(t *testing.T) {
	dir, layout := seedRepo(t)
	o := newTestOrchestrator(dir, layout, failingWorker(layout), config.Config{Iter: 1})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	log := readRunLog(t, layout)
	requireEvent(t, log, "status", "status=FAILED")
	requireEvent(t, log, "park", "branch="+parkedBranch)
	requireEvent(t, log, "task_end", "status=FAILED")
	if names := eventNames(log); names[len(names)-2] != "task_end" {
		t.Errorf("task_end is not the task's last event: %v", names)
	}
	diff := readFile(t, filepath.Join(filepath.Dir(runLogPath(layout)), "001-example", "final.diff"))
	for _, want := range []string{"+edited by worker", "+added by worker", "gone.txt"} {
		if !strings.Contains(diff, want) {
			t.Errorf("final.diff lacks %q:\n%s", want, diff)
		}
	}
}

func TestRunLogMergeEvent(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := newArtifactRunner(layout, 1)
	o := newTestOrchestrator(dir, layout, runner, config.Config{Iter: 1, Worktree: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	log := readRunLog(t, layout)
	requireEvent(t, log, "merge", "branch=diadoche/001-example", "how=fast-forward")
	requireEvent(t, log, "task_end", "status=DONE")
}

func TestArtifactWriteErrorDoesNotStopRun(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := newArtifactRunner(layout, 1)
	runner.onCall = func(a *agents.Agent, _ int) {
		if a.Name != "planner" {
			return
		}
		// A regular file where the task's artifact directory belongs
		// makes every artifact write fail, even as root.
		taskDir := filepath.Join(filepath.Dir(runLogPath(layout)), "001-example")
		if err := os.WriteFile(taskDir, []byte("in the way"), 0o644); err != nil {
			t.Error(err)
		}
	}
	o, buf := bufferedOrchestrator(dir, layout, runner, config.Config{Iter: 1})

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertStatus(t, layout, specs.StatusDone)
	log := readRunLog(t, layout)
	requireEvent(t, log, "warning", "what=iter-1-planner.stdout")
	requireEvent(t, log, "warning", "what=final.diff")
	if !strings.Contains(buf.String(), "warning:") {
		t.Errorf("no warning in the log:\n%s", buf.String())
	}
	names := eventNames(log)
	if n := len(names); n < 2 || names[n-2] != "task_end" || names[n-1] != "run_end" {
		t.Errorf("run.log does not end with task_end, run_end: %v", names)
	}
	requireEvent(t, log, "task_end", "status=DONE")
}

func TestDryRunWritesNoRunDir(t *testing.T) {
	o, _, _, _ := newPauseFixture(t, config.Config{Iter: 1, DryRun: true}, "001_example.md")
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.Layout.Root, "runs")); !os.IsNotExist(err) {
		t.Errorf("runs/ exists after a --dry-run: %v", err)
	}
}
