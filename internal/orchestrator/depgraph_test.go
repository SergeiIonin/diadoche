package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/specs"
)

// depRunner finishes every task it is given: planner writes a valid plan,
// worker a file of its own, tester marks the task DONE. order records the
// tasks in the order planner was called for them; onPlanner, if set, runs
// on each planner call with the task's spec.
type depRunner struct {
	layout    specs.Layout
	order     []string
	onPlanner func(spec string) error
}

func (r *depRunner) Run(_ context.Context, workDir string, a *agents.Agent, _ string) (agents.RunResult, error) {
	spec, err := r.current()
	if err != nil {
		return agents.RunResult{}, err
	}
	num := specs.TaskNumber(spec)
	switch a.Name {
	case "planner":
		r.order = append(r.order, spec)
		if r.onPlanner != nil {
			if err := r.onPlanner(spec); err != nil {
				return agents.RunResult{}, err
			}
		}
		err = os.WriteFile(filepath.Join(r.layout.SpecDir, num+"_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644)
	case "worker":
		err = os.WriteFile(filepath.Join(workDir, "out-"+num+".txt"), []byte("done by worker\n"), 0o644)
	case "tester":
		err = specs.SetStatus(r.layout, spec, specs.StatusDone)
	}
	return agents.RunResult{}, err
}

// current is the task the run is working on: the IN_PROGRESS row.
func (r *depRunner) current() (string, error) {
	rows, err := specs.ReadStatus(r.layout.StatusPath)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Status == specs.StatusInProgress {
			return row.Spec, nil
		}
	}
	return "", errors.New("no task is IN_PROGRESS")
}

// depRepo is seedRepo with its spec replaced by specs (file -> content)
// and, when rows is non-nil, STATUS.md set to rows; all committed.
func depRepo(t *testing.T, specFiles map[string]string, rows []specs.Row) (string, specs.Layout) {
	t.Helper()
	dir, layout := seedRepo(t)
	if err := os.Remove(filepath.Join(layout.SpecDir, "001_example.md")); err != nil {
		t.Fatal(err)
	}
	for f, body := range specFiles {
		writeFile(t, filepath.Join(layout.SpecDir, f), body)
	}
	if rows != nil {
		if err := specs.WriteStatus(layout.StatusPath, rows); err != nil {
			t.Fatal(err)
		}
	}
	if err := gitutil.CommitAll(context.Background(), dir, "dependency fixture"); err != nil {
		t.Fatal(err)
	}
	return dir, layout
}

func TestRunStopsOnDependencyCycleBeforeAnyAgent(t *testing.T) {
	cases := map[string]struct {
		specs map[string]string
		cycle string
	}{
		"two tasks": {
			specs: map[string]string{"001_a.md": "# A\nDepends-on: 002\n", "002_b.md": "# B\nDepends-on: 001\n"},
			cycle: "dependency cycle: 001_a.md -> 002_b.md -> 001_a.md",
		},
		"three tasks": {
			specs: map[string]string{
				"001_a.md": "# A\nDepends-on: 003\n",
				"002_b.md": "# B\nDepends-on: 001\n",
				"003_c.md": "# C\nDepends-on: 002\n",
				"004_d.md": "# D\nDepends-on: none\n",
			},
			cycle: "dependency cycle: 001_a.md -> 003_c.md -> 002_b.md -> 001_a.md",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir, layout := depRepo(t, c.specs, nil)
			o := newTestOrchestrator(dir, layout, neverCalledRunner{t}, config.Config{})
			err := o.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), c.cycle) {
				t.Fatalf("Run = %v, want an error containing %q", err, c.cycle)
			}
			for spec := range c.specs {
				if st, _ := specs.GetStatus(layout, spec); st != specs.StatusQueued {
					t.Errorf("%s = %s, want QUEUED", spec, st)
				}
			}
		})
	}
}

func TestRunStopsOnDanglingDependsOn(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\nDepends-on: 999\n",
		"003_c.md": "# C\nDepends-on: 001, 998\n",
	}, nil)
	o := newTestOrchestrator(dir, layout, neverCalledRunner{t}, config.Config{})
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded with dangling Depends-on references")
	}
	for _, want := range []string{"002_b.md: Depends-on names 999", "003_c.md: Depends-on names 998"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func TestRunBlocksTaskWithFailedOrBlockedDependency(t *testing.T) {
	for _, status := range []string{specs.StatusFailed, specs.StatusBlocked} {
		t.Run(status, func(t *testing.T) {
			dir, layout := depRepo(t, map[string]string{
				"001_a.md": "# A\n",
				"002_b.md": "# B\nDepends-on: 001\n",
				"003_c.md": "# C\nDepends-on: 002\n",
				"004_d.md": "# D\n",
			}, []specs.Row{
				{Num: "001", Spec: "001_a.md", Status: status},
				{Num: "002", Spec: "002_b.md", Status: specs.StatusQueued},
				{Num: "003", Spec: "003_c.md", Status: specs.StatusQueued},
				{Num: "004", Spec: "004_d.md", Status: specs.StatusQueued},
			})
			runner := &depRunner{layout: layout}
			o := newTestOrchestrator(dir, layout, runner, config.Config{Iter: 1})
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if got := strings.Join(runner.order, ","); got != "004_d.md" {
				t.Errorf("tasks run = %s, want only 004_d.md", got)
			}
			if row := taskRow(t, layout, "002_b.md"); row.Status != specs.StatusBlocked || row.Reason != "depends on 001_a.md, which is "+status {
				t.Errorf("002_b.md = %s: %q, want BLOCKED naming 001_a.md", row.Status, row.Reason)
			}
			if row := taskRow(t, layout, "003_c.md"); row.Status != specs.StatusBlocked || row.Reason != "depends on 002_b.md, which is BLOCKED" {
				t.Errorf("003_c.md = %s: %q, want BLOCKED naming 002_b.md", row.Status, row.Reason)
			}
			if row := taskRow(t, layout, "004_d.md"); row.Status != specs.StatusDone {
				t.Errorf("004_d.md = %s, want DONE: a %s task ahead of it must not block it", row.Status, status)
			}
			log := readRunLog(t, layout)
			requireEvent(t, log, "status", "task=002_b.md", "status=BLOCKED", "reason=dependency", "blocked_by=001_a.md")
			requireEvent(t, log, "status", "task=003_c.md", "status=BLOCKED", "reason=dependency", "blocked_by=002_b.md")
		})
	}
}

func TestRunDefersPendingDependencyToLaterRound(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\nDepends-on: none\n",
		"002_b.md": "# B\n",
	}, nil)
	runner := &depRunner{layout: layout}
	runner.onPlanner = func(spec string) error {
		if spec != "001_a.md" {
			return nil
		}
		if err := os.WriteFile(filepath.Join(layout.SpecDir, "003_new.md"), []byte("# New\nDepends-on: none\n"), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(layout.SpecDir, "002_b.md"), []byte("# B\nDepends-on: 003\n"), 0o644)
	}
	o := newTestOrchestrator(dir, layout, runner, config.Config{Iter: 1})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := strings.Join(runner.order, ","), "001_a.md,003_new.md,002_b.md"; got != want {
		t.Errorf("tasks run = %s, want %s", got, want)
	}
	for _, spec := range []string{"001_a.md", "002_b.md", "003_new.md"} {
		if st, _ := specs.GetStatus(layout, spec); st != specs.StatusDone {
			t.Errorf("%s = %s, want DONE", spec, st)
		}
	}
	requireEvent(t, readRunLog(t, layout), "skipped", "task=002_b.md", "reason=pending_dependency", "003")
}

func TestRunErrorsWhenNothingIsReady(t *testing.T) {
	dir, layout := depRepo(t, map[string]string{
		"001_a.md": "# A\n",
		"002_b.md": "# B\nDepends-on: 001\n",
	}, []specs.Row{
		{Num: "001", Spec: "001_a.md", Status: "SKIPPED"},
		{Num: "002", Spec: "002_b.md", Status: specs.StatusQueued},
	})
	o := newTestOrchestrator(dir, layout, neverCalledRunner{t}, config.Config{})
	err := o.Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded although no task could ever become ready")
	}
	for _, want := range []string{"no pending task is ready", "002_b.md waits for 001 (SKIPPED)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if st, _ := specs.GetStatus(layout, "002_b.md"); st != specs.StatusQueued {
		t.Errorf("002_b.md = %s, want QUEUED", st)
	}
}
