//go:build unix

package agents

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitForFile polls until path exists and is non-empty, returning its
// trimmed contents.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
	return ""
}

func TestCLIRunnerCancelKillsTheWholeCallProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	stub := fakeClaude(t, "sleep 30 &\necho $! > "+pidFile+"\nwait\n")
	r := CLIRunner{Bin: stub}
	a := &Agent{Name: "worker", Model: "claude-opus-5"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type out struct {
		res RunResult
		err error
	}
	done := make(chan out, 1)
	start := time.Now()
	go func() {
		res, err := r.Run(ctx, t.TempDir(), a, "x")
		done <- out{res, err}
	}()

	pid, err := strconv.Atoi(waitForFile(t, pidFile))
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	var o out
	select {
	case o = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of the cancel")
	}
	if time.Since(start) >= 5*time.Second {
		t.Errorf("Run took %s", time.Since(start))
	}
	if o.err == nil || o.res.ExitCode != -1 {
		t.Errorf("Run = %+v, %v; want an error and ExitCode -1", o.res, o.err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d (claude's tool subprocess) survived the cancel", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCLIRunnerRunsClaudeInItsOwnProcessGroup(t *testing.T) {
	pgidFile := filepath.Join(t.TempDir(), "pgid")
	stub := fakeClaude(t, "ps -o pgid= -p $$ > "+pgidFile+"\n")
	r := CLIRunner{Bin: stub}
	if _, err := r.Run(context.Background(), t.TempDir(), &Agent{Name: "worker"}, "x"); err != nil {
		t.Fatal(err)
	}
	pgid, err := strconv.Atoi(waitForFile(t, pgidFile))
	if err != nil {
		t.Fatal(err)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("claude ran in diadoche's process group %d; want its own", pgid)
	}
}
