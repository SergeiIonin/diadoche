package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/specs"
)

const (
	rgSpecFile    = "001_example.md"
	oldTestSource = "package main\n\nimport \"testing\"\n\nfunc TestOld(t *testing.T) {}\n"
)

// redGreenRepo returns a repo with an initial commit holding README.md,
// a pre-existing old_test.go, the agent fixtures and spec 001_example.md
// (with specBody as its content).
func redGreenRepo(t *testing.T, specBody string) (string, specs.Layout) {
	t.Helper()
	requireGit(t)
	setTestGitEnv(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := gitutil.EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "README.md"), "# project\n")
	writeFile(t, filepath.Join(dir, "old_test.go"), oldTestSource)
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	writeFile(t, filepath.Join(layout.SpecDir, rgSpecFile), specBody)
	if err := gitutil.CommitAll(ctx, dir, "seed"); err != nil {
		t.Fatal(err)
	}
	return dir, layout
}

// testStub is a stand-in for the project's test command: a shell script
// outside the repo that records the directory it ran in and exits with a
// fixed code.
type testStub struct {
	script string // absolute path of the script
	record string // absolute path of the file it appends `pwd` to
}

func newTestStub(t *testing.T, exitCode int) testStub {
	t.Helper()
	d := t.TempDir()
	s := testStub{script: filepath.Join(d, "stub.sh"), record: filepath.Join(d, "ran.txt")}
	writeFile(t, s.script, fmt.Sprintf("pwd >> '%s'\necho stub output\nexit %d\n", s.record, exitCode))
	return s
}

// runs returns the directories the stub ran in; none if it never ran.
func (s testStub) runs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(s.record)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// planWith returns an onPlanner callback writing a valid plan whose
// "## Files" lists files and whose test command runs stub.
func planWith(stub testStub, files ...string) func(string) error {
	var b strings.Builder
	b.WriteString("# Plan\n\n## Files\n")
	for _, f := range files {
		fmt.Fprintf(&b, "- %s — create: test fixture\n", f)
	}
	fmt.Fprintf(&b, "\n## Test Command\n```\nsh '%s'\n```\n", stub.script)
	plan := b.String()
	return func(specDir string) error {
		return os.WriteFile(filepath.Join(specDir, "001_IMPL_PLAN.md"), []byte(plan), 0o644)
	}
}

// writeFeature returns an onWorker callback writing feature.go and, with
// withTest, feature_test.go.
func writeFeature(withTest bool) func(string) error {
	return func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "feature.go"), []byte("package main\n\nfunc Feature() int { return 1 }\n"), 0o644); err != nil {
			return err
		}
		if !withTest {
			return nil
		}
		return os.WriteFile(filepath.Join(dir, "feature_test.go"),
			[]byte("package main\n\nimport \"testing\"\n\nfunc TestFeature(t *testing.T) {\n\tif Feature() != 1 {\n\t\tt.Fatal()\n\t}\n}\n"), 0o644)
	}
}

func runRedGreen(t *testing.T, dir string, layout specs.Layout, runner *scriptedRunner, cfg config.Config) {
	t.Helper()
	cfg.Dir = dir
	if cfg.Iter == 0 {
		cfg.Iter = 1
	}
	cfg.MaxDiffBytes = config.DefaultMaxDiffBytes
	o := &Orchestrator{Cfg: cfg, Layout: layout, Runner: runner, Log: log.New(io.Discard, "", 0)}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
}

func readReview(t *testing.T, layout specs.Layout, iter int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(layout.SpecDir, fmt.Sprintf("001_REVIEW_%d.md", iter)))
	if err != nil {
		t.Fatalf("read review %d: %v", iter, err)
	}
	return string(b)
}

func assertStatus(t *testing.T, layout specs.Layout, want string) {
	t.Helper()
	got, err := specs.GetStatus(layout, rgSpecFile)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("status = %s, want %s", got, want)
	}
}

func TestRedGreenTestsPassingOnBaseFailIterationAndReachPlanner(t *testing.T) {
	dir, layout := redGreenRepo(t, "# Example\n")
	stub := newTestStub(t, 0)
	runner := &scriptedRunner{
		layout:           layout,
		onPlanner:        planWith(stub, "feature.go", "feature_test.go"),
		onWorker:         writeFeature(true),
		markDoneOnTester: true,
	}
	runRedGreen(t, dir, layout, runner, config.Config{Iter: 2, RedGreen: true, Worktree: false})

	if runner.testerCalls != 0 {
		t.Errorf("testerCalls = %d, want 0 — tests green on base must skip tester", runner.testerCalls)
	}
	if len(stub.runs(t)) != 2 {
		t.Errorf("stub ran %d time(s), want 2 (once per iteration)", len(stub.runs(t)))
	}
	review := readReview(t, layout, 1)
	for _, want := range []string{"tests check nothing", "feature_test.go", "TestFeature", "stub output"} {
		if !strings.Contains(review, want) {
			t.Errorf("review missing %q:\n%s", want, review)
		}
	}
	if strings.Contains(review, "TestOld") {
		t.Errorf("review lists a test file the worker didn't touch:\n%s", review)
	}
	if len(runner.plannerPrompts) != 2 {
		t.Fatalf("planner calls = %d, want 2", len(runner.plannerPrompts))
	}
	for _, want := range []string{"tests check nothing", "feature_test.go", "TestFeature"} {
		if !strings.Contains(runner.plannerPrompts[1], want) {
			t.Errorf("second planner prompt missing %q:\n%s", want, runner.plannerPrompts[1])
		}
	}
	assertStatus(t, layout, specs.StatusFailed)
}

func TestRedGreenTestsFailingOnBaseContinueToTester(t *testing.T) {
	dir, layout := redGreenRepo(t, "# Example\n")
	stub := newTestStub(t, 1) // e.g. the test doesn't compile without feature.go
	runner := &scriptedRunner{
		layout:           layout,
		onPlanner:        planWith(stub, "feature.go", "feature_test.go"),
		onWorker:         writeFeature(true),
		markDoneOnTester: true,
	}
	runRedGreen(t, dir, layout, runner, config.Config{RedGreen: true})

	if runner.testerCalls != 1 {
		t.Fatalf("testerCalls = %d, want 1", runner.testerCalls)
	}
	if len(stub.runs(t)) != 1 {
		t.Errorf("stub ran %d time(s), want 1", len(stub.runs(t)))
	}
	if strings.Contains(runner.testerPrompts[0], "existing tests modified") {
		t.Errorf("tester prompt has the existing-tests note though no existing test changed")
	}
	if _, err := os.Stat(filepath.Join(layout.SpecDir, "001_REVIEW_1.md")); err == nil {
		t.Errorf("red-green wrote a review for a red result")
	}
	assertStatus(t, layout, specs.StatusDone)
}

func TestRedGreenNoTestFilesFailsIteration(t *testing.T) {
	dir, layout := redGreenRepo(t, "# Example\n")
	stub := newTestStub(t, 1)
	runner := &scriptedRunner{
		layout:           layout,
		onPlanner:        planWith(stub, "feature.go"),
		onWorker:         writeFeature(false),
		markDoneOnTester: true,
	}
	runRedGreen(t, dir, layout, runner, config.Config{RedGreen: true})

	if runner.testerCalls != 0 {
		t.Errorf("testerCalls = %d, want 0", runner.testerCalls)
	}
	if review := readReview(t, layout, 1); !strings.Contains(review, "worker wrote no tests") || !strings.Contains(review, specs.NoTestsMarker) {
		t.Errorf("review lacks the reason or the no-tests hint:\n%s", review)
	}
	if runs := stub.runs(t); runs != nil {
		t.Errorf("stub ran %v, want never", runs)
	}
	assertStatus(t, layout, specs.StatusFailed)
}

func TestRedGreenNoTestsMarkerSkipsCheck(t *testing.T) {
	dir, layout := redGreenRepo(t, "# Example\n\n"+specs.NoTestsMarker+"\n")
	stub := newTestStub(t, 0)
	runner := &scriptedRunner{
		layout:           layout,
		onPlanner:        planWith(stub, "feature.go"),
		onWorker:         writeFeature(false),
		markDoneOnTester: true,
	}
	runRedGreen(t, dir, layout, runner, config.Config{RedGreen: true})

	if runner.testerCalls != 1 {
		t.Errorf("testerCalls = %d, want 1", runner.testerCalls)
	}
	if runs := stub.runs(t); runs != nil {
		t.Errorf("stub ran %v, want never", runs)
	}
	assertStatus(t, layout, specs.StatusDone)
}

func TestRedGreenModifiedExistingTestAddsTesterNote(t *testing.T) {
	for _, listed := range []bool{false, true} {
		t.Run(fmt.Sprintf("listed=%v", listed), func(t *testing.T) {
			dir, layout := redGreenRepo(t, "# Example\n")
			stub := newTestStub(t, 1)
			files := []string{"feature.go", "feature_test.go"}
			if listed {
				files = append(files, "old_test.go")
			}
			runner := &scriptedRunner{
				layout:    layout,
				onPlanner: planWith(stub, files...),
				onWorker: func(dir string) error {
					if err := writeFeature(true)(dir); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(dir, "old_test.go"), []byte("package main\n"), 0o644)
				},
				markDoneOnTester: true,
			}
			runRedGreen(t, dir, layout, runner, config.Config{RedGreen: true})

			if runner.testerCalls != 1 {
				t.Fatalf("testerCalls = %d, want 1", runner.testerCalls)
			}
			prompt := runner.testerPrompts[0]
			hasNote := strings.Contains(prompt, "existing tests modified")
			if listed && hasNote {
				t.Errorf("tester prompt has the existing-tests note though old_test.go is in ## Files")
			}
			if !listed && (!hasNote || !strings.Contains(prompt, "not listed in the plan's ## Files: old_test.go")) {
				t.Errorf("tester prompt lacks the existing-tests note naming old_test.go:\n%s", prompt)
			}
		})
	}
}

func TestRedGreenDisabledDoesNothing(t *testing.T) {
	dir, layout := redGreenRepo(t, "# Example\n")
	stub := newTestStub(t, 0)
	runner := &scriptedRunner{
		layout:    layout,
		onPlanner: planWith(stub, "feature.go"),
		onWorker: func(dir string) error {
			if err := writeFeature(false)(dir); err != nil {
				return err
			}
			// Even a weakened existing test gets no extra tester item.
			return os.WriteFile(filepath.Join(dir, "old_test.go"), []byte("package main\n"), 0o644)
		},
		markDoneOnTester: true,
	}
	runRedGreen(t, dir, layout, runner, config.Config{RedGreen: false})

	if runner.testerCalls != 1 {
		t.Fatalf("testerCalls = %d, want 1", runner.testerCalls)
	}
	if runs := stub.runs(t); runs != nil {
		t.Errorf("stub ran %v, want never", runs)
	}
	if strings.Contains(runner.testerPrompts[0], "existing tests modified") {
		t.Errorf("tester prompt has a red-green note with --red-green=false")
	}
	assertStatus(t, layout, specs.StatusDone)
}

// assertCheckWorktreeGone checks that every directory the stub ran in is
// gone, was outside the repo, and is no longer a registered worktree.
func assertCheckWorktreeGone(t *testing.T, dir string, stub testStub) {
	t.Helper()
	runs := stub.runs(t)
	if len(runs) == 0 {
		t.Fatal("stub never ran")
	}
	resolvedDir, _ := filepath.EvalSymlinks(dir)
	worktrees := gitOut(t, dir, "worktree", "list", "--porcelain")
	for _, ran := range runs {
		if _, err := os.Stat(ran); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("temporary worktree %s still exists (err=%v)", ran, err)
		}
		for _, root := range []string{dir, resolvedDir} {
			if strings.HasPrefix(ran, root+string(filepath.Separator)) {
				t.Errorf("check ran in %s, inside the repo %s", ran, root)
			}
		}
		if strings.Contains(worktrees, filepath.Base(filepath.Dir(ran))) {
			t.Errorf("temporary worktree %s still registered:\n%s", ran, worktrees)
		}
	}
}

func TestRedGreenTemporaryWorktreeIsRemoved(t *testing.T) {
	for _, exit := range []int{0, 2} {
		t.Run(fmt.Sprintf("exit=%d", exit), func(t *testing.T) {
			dir, layout := redGreenRepo(t, "# Example\n")
			stub := newTestStub(t, exit)
			runner := &scriptedRunner{
				layout:    layout,
				onPlanner: planWith(stub, "feature.go", "feature_test.go"),
				onWorker:  writeFeature(true),
			}
			runRedGreen(t, dir, layout, runner, config.Config{RedGreen: true, Iter: 1})

			assertCheckWorktreeGone(t, dir, stub)
			// The check copies files into its own worktree only; the task's
			// working tree keeps worker's changes until the task is parked,
			// so look at the parked branch instead.
			if out := gitOut(t, dir, "show", "--name-only", "--format=", "diadoche/failed/001-example"); !strings.Contains(out, "feature.go") {
				t.Errorf("worker's feature.go missing from the parked changes: %q", out)
			}
		})
	}
}

func TestRedGreenWorktreeMode(t *testing.T) {
	dir, layout := redGreenRepo(t, "# Example\n")
	stub := newTestStub(t, 1)
	var workDir string
	runner := &scriptedRunner{
		layout:    layout,
		onPlanner: planWith(stub, "feature.go", "feature_test.go"),
		onWorker: func(d string) error {
			workDir = d
			return writeFeature(true)(d)
		},
		markDoneOnTester: true,
	}
	o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true, RedGreen: true, Iter: 1})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if runner.testerCalls != 1 {
		t.Fatalf("testerCalls = %d, want 1", runner.testerCalls)
	}
	if workDir == dir {
		t.Fatal("worker ran in the main working tree, want the task worktree")
	}
	assertCheckWorktreeGone(t, dir, stub)
	assertStatus(t, layout, specs.StatusDone)
	if _, err := os.Stat(filepath.Join(dir, "feature.go")); err != nil {
		t.Errorf("feature.go not merged into the main tree: %v", err)
	}
}
