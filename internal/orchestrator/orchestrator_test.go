package orchestrator

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/specs"
)

// fakeRunner simulates a real planner/worker/tester: the worker writes a
// file change, and the tester marks the task DONE in STATUS.md, exactly
// like a real tester agent would via its own tool calls.
type fakeRunner struct {
	layout specs.Layout
	dir    string
}

func (r fakeRunner) Run(_ context.Context, workDir string, a *agents.Agent, _ string) (string, error) {
	switch a.Name {
	case "worker":
		if err := os.WriteFile(filepath.Join(workDir, "output.txt"), []byte("done by worker\n"), 0o644); err != nil {
			return "", err
		}
	case "tester":
		rows, err := specs.ReadStatus(r.layout.StatusPath)
		if err != nil {
			return "", err
		}
		for i := range rows {
			rows[i].Status = specs.StatusDone
		}
		if err := specs.WriteStatus(r.layout.StatusPath, rows); err != nil {
			return "", err
		}
	}
	return "", nil
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
}

func TestRunCommitsWhenTaskBecomesDone(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()

	t.Setenv("GIT_AUTHOR_NAME", "diadoche-test")
	t.Setenv("GIT_AUTHOR_EMAIL", "diadoche-test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "diadoche-test")
	t.Setenv("GIT_COMMITTER_EMAIL", "diadoche-test@example.com")

	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"planner", "worker", "tester"} {
		content := "---\nname: " + name + "\nmodel: test-model\n---\nbe the " + name
		if err := os.WriteFile(filepath.Join(layout.AgentsDir, name+".md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(layout.SpecDir, "001_example.md"), []byte("# Example\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: dir, Iter: 1}
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
