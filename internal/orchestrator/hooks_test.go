package orchestrator

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/specs"
)

// hookTaskRunner is a fake claude that works on whichever task is
// IN_PROGRESS: planner writes a valid plan for it, worker writes
// <num>.txt, and tester marks it (and only it) DONE unless neverDone.
type hookTaskRunner struct {
	layout    specs.Layout
	neverDone bool
	onWorker  func(dir string)
}

func (r *hookTaskRunner) current() (specs.Row, error) {
	rows, err := specs.ReadStatus(r.layout.StatusPath)
	if err != nil {
		return specs.Row{}, err
	}
	for _, row := range rows {
		if row.Status == specs.StatusInProgress {
			return row, nil
		}
	}
	return specs.Row{}, fmt.Errorf("no task IN_PROGRESS")
}

func (r *hookTaskRunner) Run(_ context.Context, dir string, a *agents.Agent, _ string) (agents.RunResult, error) {
	row, err := r.current()
	if err != nil {
		return agents.RunResult{}, err
	}
	num := specs.TaskNumber(row.Spec)
	switch a.Name {
	case "planner":
		err = os.WriteFile(filepath.Join(r.layout.SpecDir, num+"_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644)
	case "worker":
		err = os.WriteFile(filepath.Join(dir, num+".txt"), []byte("work of "+row.Spec+"\n"), 0o644)
		if r.onWorker != nil {
			r.onWorker(dir)
		}
	case "tester":
		if !r.neverDone {
			err = specs.SetStatus(r.layout, row.Spec, specs.StatusDone)
		}
	}
	return agents.RunResult{}, err
}

// writeHook writes an executable shell script into a temp dir outside the
// repository and returns its path.
func writeHook(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("hooks run through /bin/sh")
	}
	p := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// addSecondTask adds and commits spec 002_other.md.
func addSecondTask(t *testing.T, dir string, layout specs.Layout) {
	t.Helper()
	writeFile(t, filepath.Join(layout.SpecDir, "002_other.md"), "# Other\n")
	if err := gitutil.CommitAll(context.Background(), dir, "second task"); err != nil {
		t.Fatal(err)
	}
}

func envMap(text string) map[string]string {
	m := map[string]string{}
	for line := range strings.SplitSeq(text, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

func TestOnTaskDoneHookGetsEnvAndRunsInRepoRoot(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree=%v", worktree), func(t *testing.T) {
			dir, layout := seedRepo(t)
			base := gitOut(t, dir, "rev-parse", "HEAD")
			out := t.TempDir()
			hook := writeHook(t, fmt.Sprintf("env | grep '^DIADOCHE_' > %q/env.txt\npwd -P > %q/pwd.txt", out, out))
			o := newTestOrchestrator(dir, layout, &hookTaskRunner{layout: layout}, config.Config{Worktree: worktree, OnTaskDone: []string{hook}})
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			env := envMap(readFile(t, filepath.Join(out, "env.txt")))
			root, _ := filepath.Abs(dir)
			for k, want := range map[string]string{
				"DIADOCHE_TASK_ID":        "001-example",
				"DIADOCHE_TASK_TITLE":     taskTitle([]byte("# Example\n"), "001_example.md"),
				"DIADOCHE_REPO_ROOT":      root,
				"DIADOCHE_BASE_COMMIT":    base,
				"DIADOCHE_DECISIONS_FILE": o.DecisionsPath("001_example.md"),
			} {
				if got, ok := env[k]; !ok || got != want {
					t.Errorf("%s = %q (set: %v), want %q", k, got, ok, want)
				}
			}
			if env["DIADOCHE_TASK_TITLE"] != "Example" {
				t.Errorf("DIADOCHE_TASK_TITLE = %q, want Example", env["DIADOCHE_TASK_TITLE"])
			}
			if p := env["DIADOCHE_DECISIONS_FILE"]; p == "" || !filepath.IsAbs(p) {
				t.Errorf("DIADOCHE_DECISIONS_FILE = %q, want an absolute path", p)
			} else if _, err := os.Stat(p); err != nil {
				t.Errorf("decisions file: %v", err)
			}
			merge := env["DIADOCHE_MERGE_COMMIT"]
			if merge == "" || merge == base {
				t.Fatalf("DIADOCHE_MERGE_COMMIT = %q, want a commit after the base %s", merge, base)
			}
			if gitFails(dir, "cat-file", "-e", merge+":001.txt") {
				t.Errorf("merge commit %s doesn't contain the worker's 001.txt", merge)
			}
			if gitFails(dir, "merge-base", "--is-ancestor", merge, "HEAD") {
				t.Errorf("merge commit %s is not on the run's branch", merge)
			}
			wantPwd, _ := filepath.EvalSymlinks(dir)
			if got := strings.TrimSpace(readFile(t, filepath.Join(out, "pwd.txt"))); got != wantPwd {
				t.Errorf("hook ran in %q, want the repository root %q", got, wantPwd)
			}
		})
	}
}

func TestOnTaskDoneDecisionsFileEmptyWhenMissing(t *testing.T) {
	dir, layout := seedRepo(t)
	o := newTestOrchestrator(dir, layout, nil, config.Config{})
	env, err := o.hookEnv("001_example.md", "001", &workspace{base: "b", mergeCommit: "m"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "DIADOCHE_DECISIONS_FILE=") {
			found = true
			if kv != "DIADOCHE_DECISIONS_FILE=" {
				t.Errorf("got %q, want an empty DIADOCHE_DECISIONS_FILE", kv)
			}
		}
	}
	if !found {
		t.Error("DIADOCHE_DECISIONS_FILE must be set even without a decisions file")
	}

	// A recorded path whose file is gone counts as missing too.
	o.decisions = map[string]string{"001_example.md": filepath.Join(t.TempDir(), "decisions.json")}
	env, _ = o.hookEnv("001_example.md", "001", &workspace{})
	if !strings.Contains(strings.Join(env, "\n"), "\nDIADOCHE_DECISIONS_FILE=") || strings.Contains(strings.Join(env, "\n"), "decisions.json") {
		t.Errorf("a missing decisions file must give an empty variable, env: %q", env)
	}
}

func onTaskDoneSubjects(t *testing.T, dir string) int {
	t.Helper()
	return strings.Count(gitOut(t, dir, "log", "--format=%s"), "diadoche: on-task-done for 001-example")
}

func TestOnTaskDoneHookCommitsItsChanges(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree=%v", worktree), func(t *testing.T) {
			dir, layout := seedRepo(t)
			hook := writeHook(t, "echo hook > hook.txt")
			o := newTestOrchestrator(dir, layout, &hookTaskRunner{layout: layout}, config.Config{Worktree: worktree, OnTaskDone: []string{hook}})
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if n := onTaskDoneSubjects(t, dir); n != 1 {
				t.Fatalf("want exactly one on-task-done commit, found %d:\n%s", n, gitOut(t, dir, "log", "--format=%s"))
			}
			sha := gitOut(t, dir, "log", "-1", "--format=%H", "--grep", "^diadoche: on-task-done for 001-example$")
			if files := gitOut(t, dir, "show", "--name-only", "--format=", sha); files != "hook.txt" {
				t.Errorf("the hook commit should carry only hook.txt, has:\n%s", files)
			}
			if got := gitOut(t, dir, "show", "HEAD:hook.txt"); got != "hook" {
				t.Errorf("hook.txt at HEAD = %q", got)
			}
			if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
				t.Errorf("main tree should be clean, got:\n%s", st)
			}
			if st := statusRow(t, layout, "001_example.md"); st.Status != specs.StatusDone || st.Reason != "" {
				t.Errorf("row = %+v, want DONE without a reason", st)
			}
		})
	}
}

func TestOnTaskDoneHookWithoutChangesMakesNoCommit(t *testing.T) {
	count := func(hooks []string) string {
		dir, layout := seedRepo(t)
		o := newTestOrchestrator(dir, layout, &hookTaskRunner{layout: layout}, config.Config{Worktree: true, OnTaskDone: hooks})
		if err := o.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if n := onTaskDoneSubjects(t, dir); n != 0 {
			t.Errorf("a hook that changes nothing made %d on-task-done commit(s)", n)
		}
		return gitOut(t, dir, "rev-list", "--count", "HEAD")
	}
	if with, without := count([]string{writeHook(t, "true")}), count(nil); with != without {
		t.Errorf("commit count with a no-op hook = %s, without hooks = %s", with, without)
	}
}

func TestOnTaskDoneHookFailureIsWarningOnly(t *testing.T) {
	dir, layout := seedRepo(t)
	addSecondTask(t, dir, layout)
	hook := writeHook(t, "echo to-stdout\necho to-stderr >&2\nexit 2")
	o := newTestOrchestrator(dir, layout, &hookTaskRunner{layout: layout}, config.Config{Worktree: true, OnTaskDone: []string{hook}})
	o.Now = func() time.Time { return artifactNow }
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, spec := range []string{"001_example.md", "002_other.md"} {
		row := statusRow(t, layout, spec)
		if row.Status != specs.StatusDone || !strings.Contains(row.Reason, "on-task-done hook 1 failed (exit 2)") {
			t.Errorf("%s row = %+v, want DONE with the hook failure as reason", spec, row)
		}
	}
	if raw := readFile(t, layout.StatusPath); !strings.Contains(raw, "DONE: on-task-done hook 1 failed (exit 2)") {
		t.Errorf("STATUS.md lacks the hook warning:\n%s", raw)
	}
	// The run continued to the next task, which ran its hook too.
	if gitFails(dir, "cat-file", "-e", "HEAD:002.txt") {
		t.Error("the second task's work was not merged")
	}
	runLog := readRunLog(t, layout)
	requireEvent(t, runLog, "warning", "what=on-task-done-1")
	requireEvent(t, runLog, "on_task_done", "result=failed", "exit=2")
	if n := len(eventLines(runLog, "on_task_done", "result=failed")); n != 2 {
		t.Errorf("want two failed on_task_done events (one per task), got %d", n)
	}
	requireEvent(t, runLog, "task_end", "002_other.md", "status=DONE")
	taskDir := filepath.Join(layout.Root, "runs", artifactRunID, "001-example")
	if got := readFile(t, filepath.Join(taskDir, "on-task-done-1.stdout")); got != "to-stdout\n" {
		t.Errorf("on-task-done-1.stdout = %q", got)
	}
	if got := readFile(t, filepath.Join(taskDir, "on-task-done-1.stderr")); got != "to-stderr\n" {
		t.Errorf("on-task-done-1.stderr = %q", got)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("main tree should be clean, got:\n%s", st)
	}
}

func TestOnTaskDoneHookTimeoutIsKilled(t *testing.T) {
	dir, layout := seedRepo(t)
	hook := writeHook(t, "sleep 30 &\nsleep 30")
	o := newTestOrchestrator(dir, layout, &hookTaskRunner{layout: layout}, config.Config{
		Worktree: true, OnTaskDone: []string{hook}, HookTimeout: 200 * time.Millisecond,
	})
	start := time.Now()
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Run took %s; the timed-out hook should have been killed", elapsed)
	}
	row := statusRow(t, layout, "001_example.md")
	if row.Status != specs.StatusDone || !strings.Contains(row.Reason, "on-task-done hook 1 timed out") {
		t.Errorf("row = %+v, want DONE with a timeout reason", row)
	}
	runLog := readRunLog(t, layout)
	requireEvent(t, runLog, "on_task_done", "result=timeout")
	requireEvent(t, runLog, "warning", "what=on-task-done-1")
}

func TestOnTaskDoneHooksRunInOrder(t *testing.T) {
	dir, layout := seedRepo(t)
	logFile := filepath.Join(t.TempDir(), "order.log")
	a := writeHook(t, fmt.Sprintf("echo a >> %q", logFile))
	b := writeHook(t, fmt.Sprintf("echo b >> %q", logFile))
	o := newTestOrchestrator(dir, layout, &hookTaskRunner{layout: layout}, config.Config{Worktree: true, OnTaskDone: []string{a, b}})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := readFile(t, logFile); got != "a\nb\n" {
		t.Errorf("hooks ran as %q, want a then b", got)
	}
}

func TestOnTaskDoneNotRunForFailedTask(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree=%v", worktree), func(t *testing.T) {
			dir, layout := seedRepo(t)
			marker := filepath.Join(t.TempDir(), "ran")
			hook := writeHook(t, fmt.Sprintf("touch %q", marker))
			o := newTestOrchestrator(dir, layout, &hookTaskRunner{layout: layout, neverDone: true}, config.Config{
				Worktree: worktree, Iter: 1, OnTaskDone: []string{hook},
			})
			_ = o.Run(context.Background())
			if st, _ := specs.GetStatus(layout, "001_example.md"); st != specs.StatusFailed {
				t.Fatalf("status = %s, want FAILED", st)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("a FAILED task ran its hook (stat err = %v)", err)
			}
			if lines := eventLines(readRunLog(t, layout), "on_task_done_start"); len(lines) != 0 {
				t.Errorf("a FAILED task logged hook events: %q", lines)
			}
		})
	}
}

func TestOnTaskDoneNotRunForBlockedTask(t *testing.T) {
	dir, layout := seedRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	hook := writeHook(t, fmt.Sprintf("touch %q", marker))
	runner := &hookTaskRunner{layout: layout, onWorker: func(d string) {
		_ = os.WriteFile(filepath.Join(d, "edit.txt"), []byte("edited by worker\n"), 0o644)
		// Meanwhile the main branch moves on with a conflicting edit.
		_ = os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("edited on main\n"), 0o644)
		if out, err := exec.Command("git", "-C", dir, "commit", "-qam", "main edit").CombinedOutput(); err != nil {
			t.Errorf("commit on main: %v: %s", err, out)
		}
	}}
	o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true, OnTaskDone: []string{hook}})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st, _ := specs.GetStatus(layout, "001_example.md"); st != specs.StatusBlocked {
		t.Fatalf("status = %s, want BLOCKED", st)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a BLOCKED task ran its hook (stat err = %v)", err)
	}
}

func TestOnTaskDoneHookCanceledRunSkipsRemainingHooks(t *testing.T) {
	dir, layout := seedRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	o := newTestOrchestrator(dir, layout, nil, config.Config{OnTaskDone: []string{writeHook(t, fmt.Sprintf("touch %q", marker))}})
	o.Log = log.New(io.Discard, "", 0)
	if _, _, err := specs.Sync(layout); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if failed := o.runTaskDoneHooks(ctx, "001_example.md", "001", &workspace{}, nil); !failed {
		t.Error("skipped hooks should be reported as a failure")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a hook ran after the run was cancelled (stat err = %v)", err)
	}
	if row := statusRow(t, layout, "001_example.md"); !strings.Contains(row.Reason, "skipped (interrupted)") {
		t.Errorf("row = %+v, want the skipped hook as reason", row)
	}
}

func TestCappedBufferTruncates(t *testing.T) {
	b := &cappedBuffer{limit: 10}
	for range 3 {
		if n, err := b.Write([]byte("0123456")); n != 7 || err != nil {
			t.Fatalf("Write = %d, %v; want 7, nil", n, err)
		}
	}
	got := b.Bytes()
	if !bytes.HasPrefix(got, []byte("0123456012\n")) || !bytes.Contains(got, []byte("output truncated at 10 bytes")) {
		t.Errorf("Bytes = %q", got)
	}
	small := &cappedBuffer{limit: 10}
	_, _ = small.Write([]byte("abc"))
	if got := string(small.Bytes()); got != "abc" {
		t.Errorf("Bytes = %q, want abc untouched", got)
	}
}
