package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/smoke"
	"diadoche/internal/specs"
)

// orderedRunner is scriptedRunner that also appends "tester" to the order
// file when tester is called.
type orderedRunner struct {
	*scriptedRunner
	order string
}

func (r *orderedRunner) Run(ctx context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	if a.Name == "tester" {
		appendLine(r.order, "tester")
	}
	return r.scriptedRunner.Run(ctx, dir, a, prompt)
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// smokeFixture is a red-green repo with task 001, a test stub that records
// "redgreen" in the order file, and a worker that writes the feature, its
// test and, when worktreeSmoke is set, the worktree's smoke.yaml.
type smokeFixture struct {
	dir    string
	layout specs.Layout
	tmp    string
	order  string
	runner *orderedRunner
	logBuf bytes.Buffer

	worktreeSmoke string
}

func newSmokeFixture(t *testing.T) *smokeFixture {
	t.Helper()
	f := &smokeFixture{}
	f.dir, f.layout = redGreenRepo(t, "# Example\n")
	f.tmp = t.TempDir()
	f.order = filepath.Join(f.tmp, "order")
	stub := testStub{script: filepath.Join(f.tmp, "stub.sh"), record: filepath.Join(f.tmp, "ran.txt")}
	writeFile(t, stub.script, fmt.Sprintf("echo redgreen >> '%s'\nexit 1\n", f.order))
	f.runner = &orderedRunner{order: f.order, scriptedRunner: &scriptedRunner{
		layout:    f.layout,
		onPlanner: planWith(stub, "feature.go", "feature_test.go"),
		onWorker: func(dir string) error {
			if err := writeFeature(true)(dir); err != nil {
				return err
			}
			if f.worktreeSmoke == "" {
				return nil
			}
			p := smoke.PathIn(specs.NewLayout(dir).Root)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			return os.WriteFile(p, []byte(f.worktreeSmoke), 0o644)
		},
	}}
	return f
}

// step is a smoke shell command recording "<name>:<physical cwd>".
func (f *smokeFixture) step(name string) string {
	return fmt.Sprintf(`echo "%s:$(pwd -P)" >> '%s'`, name, f.order)
}

func (f *smokeFixture) run(t *testing.T, cfg config.Config, mutate func(*Orchestrator)) error {
	t.Helper()
	cfg.Dir = f.dir
	if cfg.Iter == 0 {
		cfg.Iter = 1
	}
	cfg.MaxDiffBytes = config.DefaultMaxDiffBytes
	o := &Orchestrator{Cfg: cfg, Layout: f.layout, Runner: f.runner, Log: log.New(&f.logBuf, "", 0)}
	if mutate != nil {
		mutate(o)
	}
	return o.Run(context.Background())
}

func (f *smokeFixture) orderLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.order)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// steps returns the order lines with the "<dir>" part dropped.
func (f *smokeFixture) steps(t *testing.T) []string {
	var out []string
	for _, l := range f.orderLines(t) {
		name, _, _ := strings.Cut(l, ":")
		out = append(out, name)
	}
	return out
}

func (f *smokeFixture) taskArtifactsDir(t *testing.T) string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(f.layout.Root, "runs", "*", "001-example"))
	if len(m) != 1 {
		t.Fatalf("task artifacts dirs = %q", m)
	}
	return m[0]
}

func (f *smokeFixture) worktreeDir(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(p, ".claude", "diadoche", "worktrees", "001-example")
}

func (f *smokeFixture) testerPrompt(t *testing.T) string {
	t.Helper()
	if len(f.runner.testerPrompts) == 0 {
		t.Fatal("tester was not called")
	}
	return f.runner.testerPrompts[0]
}

func assertNotDone(t *testing.T, f *smokeFixture) {
	t.Helper()
	got, err := specs.GetStatus(f.layout, rgSpecFile)
	if err != nil {
		t.Fatal(err)
	}
	if got == specs.StatusDone {
		t.Errorf("status = DONE, want an unsuccessful task")
	}
}

func TestSmokeRunsAfterRedGreenBeforeTesterInWorktree(t *testing.T) {
	f := newSmokeFixture(t)
	f.worktreeSmoke = fmt.Sprintf("up: %s\nchecks:\n  - cmd: %s\ndown: %s\n", f.step("up"), f.step("check"), f.step("down"))
	f.runner.markDoneOnTester = true
	if err := f.run(t, config.Config{Worktree: true, RedGreen: true}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := strings.Join(f.steps(t), " "), "redgreen up check down tester"; got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
	wt := f.worktreeDir(t)
	for _, l := range f.orderLines(t) {
		if name, cwd, ok := strings.Cut(l, ":"); ok && cwd != wt {
			t.Errorf("%s ran in %s, want the worktree %s", name, cwd, wt)
		}
	}
}

func TestSmokeFallsBackToMainTreeFile(t *testing.T) {
	f := newSmokeFixture(t)
	writeFile(t, smoke.PathIn(f.layout.Root), fmt.Sprintf("up: %s\ndown: %s\n", f.step("up"), f.step("down")))
	if err := f.run(t, config.Config{Worktree: true}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(f.steps(t), " "); got != "up down tester" {
		t.Errorf("order = %q, want %q", got, "up down tester")
	}
	wt := f.worktreeDir(t)
	if l := f.orderLines(t)[0]; l != "up:"+wt {
		t.Errorf("up ran as %q, want in the worktree %s", l, wt)
	}
}

func TestNoSmokeFileChangesNothing(t *testing.T) {
	f := newSmokeFixture(t)
	if err := f.run(t, config.Config{Worktree: true}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out := f.logBuf.String(); strings.Contains(out, "smoke") {
		t.Errorf("log mentions smoke:\n%s", out)
	}
	if strings.Contains(readRunLog(t, f.layout), "event=smoke_") {
		t.Error("run.log has a smoke event")
	}
	entries, err := os.ReadDir(f.taskArtifactsDir(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "smoke") || strings.HasSuffix(e.Name(), "-console.txt") || strings.HasSuffix(e.Name(), ".png") {
			t.Errorf("unexpected artifact %s", e.Name())
		}
	}
	p := f.testerPrompt(t)
	if strings.Contains(p, "Smoke check") || !strings.Contains(p, normalDiffGuidance) {
		t.Errorf("tester prompt differs from a run without smoke:\n%s", p)
	}
}

func TestSmokeFailingCheckSkipsTester(t *testing.T) {
	f := newSmokeFixture(t)
	f.worktreeSmoke = fmt.Sprintf("up: %s\nchecks:\n  - cmd: echo boom-output; exit 3\ndown: %s\n", f.step("up"), f.step("down"))
	f.runner.markDoneOnTester = true
	if err := f.run(t, config.Config{Worktree: true}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.runner.testerCalls != 0 {
		t.Errorf("testerCalls = %d, want 0", f.runner.testerCalls)
	}
	review := readReview(t, f.layout, 1)
	for _, want := range []string{"Smoke check failed at checks[1]", "exit 3", "boom-output", "tester was not called"} {
		if !strings.Contains(review, want) {
			t.Errorf("review lacks %q:\n%s", want, review)
		}
	}
	if _, err := os.Stat(filepath.Join(f.taskArtifactsDir(t), "iter-1-REVIEW.md")); err != nil {
		t.Errorf("review artifact: %v", err)
	}
	requireEvent(t, readRunLog(t, f.layout), "smoke_failed", "step=checks[1]", "iter=1")
	if got := strings.Join(f.steps(t), " "); got != "up down" {
		t.Errorf("order = %q, want %q", got, "up down")
	}
	assertNotDone(t, f)
}

func TestSmokeFailingUpSkipsTesterAndRunsDown(t *testing.T) {
	for _, c := range []struct{ name, yaml, failed string }{
		{"up", "up: exit 1\nchecks:\n  - cmd: %[1]s\ndown: %[2]s\n", "up"},
		{"wait", "wait:\n  cmd: exit 1\n  timeout: 300ms\n  interval: 50ms\nchecks:\n  - cmd: %[1]s\ndown: %[2]s\n", "wait"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newSmokeFixture(t)
			f.worktreeSmoke = fmt.Sprintf(c.yaml, f.step("check"), f.step("down"))
			f.runner.markDoneOnTester = true
			if err := f.run(t, config.Config{Worktree: true}, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if f.runner.testerCalls != 0 {
				t.Errorf("testerCalls = %d, want 0", f.runner.testerCalls)
			}
			if review := readReview(t, f.layout, 1); !strings.Contains(review, "Smoke check failed at "+c.failed) {
				t.Errorf("review does not name %s:\n%s", c.failed, review)
			}
			if got := strings.Join(f.steps(t), " "); got != "down" {
				t.Errorf("order = %q, want only down", got)
			}
			requireEvent(t, readRunLog(t, f.layout), "smoke_failed", "step="+c.failed)
			assertNotDone(t, f)
		})
	}
}

// browserSmoke sets up a fake browser command that writes a PNG and prints
// one console error, and a smoke.yaml with one browser URL.
func browserSmoke(t *testing.T, f *smokeFixture) string {
	t.Helper()
	cmd := filepath.Join(f.tmp, "browser.sh")
	writeFile(t, cmd, "printf PNG > \"$3\"\necho 'TypeError: x is undefined'\n")
	f.worktreeSmoke = fmt.Sprintf("up: %s\nchecks:\n  - cmd: %s\nbrowser:\n  - http://127.0.0.1:1/\ndown: %s\n",
		f.step("up"), f.step("check"), f.step("down"))
	return "sh " + cmd
}

func TestSmokeSuccessBlockInTesterPrompt(t *testing.T) {
	f := newSmokeFixture(t)
	bc := browserSmoke(t, f)
	if err := f.run(t, config.Config{Worktree: true, BrowserCmd: bc}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	p := f.testerPrompt(t)
	for _, want := range []string{
		"Smoke check (.claude/diadoche/worktrees/001-example/.claude/diadoche/smoke.yaml)",
		"iter-1-01-127-0-0-1-1.png",
		"TypeError: x is undefined",
		"do not fail the iteration by themselves",
		"iter-1-smoke.log",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("tester prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestSmokeCleanPassShortBlock(t *testing.T) {
	f := newSmokeFixture(t)
	f.worktreeSmoke = fmt.Sprintf("up: %s\nchecks:\n  - cmd: %s\ndown: %s\n", f.step("up"), f.step("check"), f.step("down"))
	if err := f.run(t, config.Config{Worktree: true}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	p := f.testerPrompt(t)
	if !strings.Contains(p, "Smoke check (") || !strings.Contains(p, "passed — the application came up and every configured step succeeded (up, checks[1], down)") {
		t.Errorf("tester prompt lacks the passed block:\n%s", p)
	}
}

func TestSmokeConsoleErrorsAloneDoNotFail(t *testing.T) {
	f := newSmokeFixture(t)
	bc := browserSmoke(t, f)
	f.runner.markDoneOnTester = true
	if err := f.run(t, config.Config{Worktree: true, BrowserCmd: bc}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := specs.GetStatus(f.layout, rgSpecFile)
	if err != nil {
		t.Fatal(err)
	}
	if got != specs.StatusDone {
		t.Errorf("status = %s, want DONE", got)
	}
	if lines := eventLines(readRunLog(t, f.layout), "smoke_failed"); len(lines) != 0 {
		t.Errorf("smoke_failed events: %q", lines)
	}
	if !strings.Contains(f.testerPrompt(t), "TypeError: x is undefined") {
		t.Error("tester prompt lacks the console error")
	}
}

func TestSmokeNoBrowserCmdWarnsOnceAndRunsRest(t *testing.T) {
	f := newSmokeFixture(t)
	browserSmoke(t, f)
	if err := f.run(t, config.Config{Worktree: true, Iter: 2}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := strings.Count(f.logBuf.String(), "browser step skipped"); n != 1 {
		t.Errorf("warning logged %d time(s), want 1:\n%s", n, f.logBuf.String())
	}
	if got, want := strings.Join(f.steps(t), " "), "up check down tester up check down tester"; got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
	if f.runner.testerCalls != 2 {
		t.Errorf("testerCalls = %d, want 2", f.runner.testerCalls)
	}
}

func TestSmokeBrowserCmdFromEnv(t *testing.T) {
	f := newSmokeFixture(t)
	bc := browserSmoke(t, f)
	t.Setenv("DIADOCHE_BROWSER_CMD", bc)
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, config.Config{Worktree: true, BrowserCmd: cfg.BrowserCmd}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(f.testerPrompt(t), "iter-1-01-127-0-0-1-1.png") {
		t.Errorf("browser step did not run with the env command:\n%s", f.testerPrompt(t))
	}
}

func TestSmokeArtifacts(t *testing.T) {
	f := newSmokeFixture(t)
	bc := browserSmoke(t, f)
	if err := f.run(t, config.Config{Worktree: true, BrowserCmd: bc}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ad := f.taskArtifactsDir(t)
	smokeLog := readFile(t, filepath.Join(ad, "iter-1-smoke.log"))
	for _, want := range []string{"== up: ok", "== checks[1]: ok", "== down: ok", "screenshot: iter-1-01-127-0-0-1-1.png", "TypeError: x is undefined"} {
		if !strings.Contains(smokeLog, want) {
			t.Errorf("smoke log lacks %q:\n%s", want, smokeLog)
		}
	}
	if got := readFile(t, filepath.Join(ad, "iter-1-01-127-0-0-1-1.png")); got != "PNG" {
		t.Errorf("screenshot = %q", got)
	}
	if got := readFile(t, filepath.Join(ad, "iter-1-01-127-0-0-1-1-console.txt")); got != "TypeError: x is undefined\n" {
		t.Errorf("console file = %q", got)
	}
}

func TestSmokeInvalidFileFailsWithFileAndLine(t *testing.T) {
	f := newSmokeFixture(t)
	marker := filepath.Join(f.tmp, "up-ran")
	f.worktreeSmoke = fmt.Sprintf("up: touch '%s'\nbogus: x\n", marker)
	f.runner.markDoneOnTester = true
	err := f.run(t, config.Config{Worktree: true}, nil)
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("up ran from an invalid smoke.yaml")
	}
	if f.runner.testerCalls != 0 {
		t.Errorf("testerCalls = %d, want 0", f.runner.testerCalls)
	}
	out := f.logBuf.String()
	if err != nil {
		out += err.Error()
	}
	if !strings.Contains(out, ".claude/diadoche/worktrees/001-example/.claude/diadoche/smoke.yaml:2: bogus") {
		t.Errorf("failure does not name file and line:\n%s", out)
	}
	requireEvent(t, readRunLog(t, f.layout), "smoke_invalid", "line=2")
	assertNotDone(t, f)
}

func TestSmokeStopDuringStepStillRunsDown(t *testing.T) {
	f := newSmokeFixture(t)
	started := filepath.Join(f.tmp, "started")
	f.worktreeSmoke = fmt.Sprintf("wait:\n  cmd: touch '%s'; sleep 0.05; exit 1\n  timeout: 30s\n  interval: 50ms\ndown: %s\n", started, f.step("down"))
	stop := make(chan struct{})
	go func() {
		for range 600 {
			if _, err := os.Stat(started); err == nil {
				close(stop)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	err := f.run(t, config.Config{Worktree: true}, func(o *Orchestrator) { o.StopRequested = stop })
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Run = %v, want ErrInterrupted", err)
	}
	if got := strings.Join(f.steps(t), " "); got != "down" {
		t.Errorf("order = %q, want down", got)
	}
	if f.runner.testerCalls != 0 {
		t.Errorf("testerCalls = %d, want 0", f.runner.testerCalls)
	}
	if lines := eventLines(readRunLog(t, f.layout), "smoke_failed"); len(lines) != 0 {
		t.Errorf("smoke_failed events: %q", lines)
	}
	if _, err := os.Stat(filepath.Join(f.layout.SpecDir, "001_REVIEW_1.md")); err == nil {
		t.Error("an interrupted smoke step wrote a review")
	}
}

func TestSmokeSkippedInSimulatedLoop(t *testing.T) {
	f := newSmokeFixture(t)
	f.worktreeSmoke = fmt.Sprintf("up: %s\ndown: %s\n", f.step("up"), f.step("down"))
	if err := f.run(t, config.Config{}, func(o *Orchestrator) { o.simulate = true }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(f.steps(t), " "); got != "tester" {
		t.Errorf("order = %q, want only tester: smoke must not run in a simulated loop", got)
	}
}

func TestFatalSmokeStep(t *testing.T) {
	for step, want := range map[string]bool{"up": true, "wait": true, "checks[3]": true, "browser": false, "down": false, "": false} {
		if got := fatalSmokeStep(step); got != want {
			t.Errorf("fatalSmokeStep(%q) = %v, want %v", step, got, want)
		}
	}
}

func TestSmokeTesterNote(t *testing.T) {
	clean := smoke.Result{
		Steps:   []smoke.StepResult{{Name: "up", Status: smoke.StatusOK}, {Name: "down", Status: smoke.StatusOK}},
		DownRan: true, DownOK: true,
	}
	note := smokeTesterNote("smoke.yaml", "runs/r/001-x", 2, clean)
	if !strings.HasPrefix(note, "Smoke check (smoke.yaml): passed") || !strings.Contains(note, "(up, down)") ||
		!strings.Contains(note, "runs/r/001-x/iter-2-smoke.log") {
		t.Errorf("clean note = %q", note)
	}
	if strings.Contains(note, "do not fail") {
		t.Errorf("clean note has the full block: %q", note)
	}

	withErrors := clean
	withErrors.Pages = []smoke.Page{{URL: "http://x/", OK: true, Screenshot: "iter-2-01-x.png", ConsoleErrors: []string{"boom"}}}
	note = smokeTesterNote("smoke.yaml", "", 2, withErrors)
	for _, want := range []string{"Smoke check (smoke.yaml) — result", "boom", "iter-2-01-x.png", "do not fail the iteration by themselves"} {
		if !strings.Contains(note, want) {
			t.Errorf("note lacks %q: %q", want, note)
		}
	}
	if strings.Contains(note, "iter-2-smoke.log") {
		t.Errorf("note refers to a log without artifacts: %q", note)
	}
}
