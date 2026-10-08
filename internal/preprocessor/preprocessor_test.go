package preprocessor

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
)

func writeAgentFixture(t *testing.T, agentsDir, name, model string) {
	t.Helper()
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\nmodel: " + model + "\n---\nbe the " + name
	if err := os.WriteFile(filepath.Join(agentsDir, name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunDryRunSkipsUsageLog(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	writeAgentFixture(t, layout.AgentsDir, "preprocessor", "claude-opus-5")
	taskPath := filepath.Join(dir, "raw.md")
	if err := os.WriteFile(taskPath, []byte("do several things"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: dir, Task: "raw.md", DryRun: true}
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(layout.Root, "usage.jsonl")); err == nil {
		t.Error("expected no usage.jsonl to be written in --dry-run")
	}
}

func TestRunFailsFastOnUnknownPreprocessorModelOverride(t *testing.T) {
	dir := t.TempDir()
	layout := specs.NewLayout(dir)
	writeAgentFixture(t, layout.AgentsDir, "preprocessor", "claude-opus-5")
	taskPath := filepath.Join(dir, "raw.md")
	if err := os.WriteFile(taskPath, []byte("do several things"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: dir, Task: "raw.md", DryRun: true,
		PreprocessorModel: agents.ModelOverride{Value: "not-a-real-model", Source: agents.ModelSourceFlag}}
	if err := Run(context.Background(), cfg); err == nil {
		t.Fatal("expected Run to fail on an unknown --preprocessor-model override")
	}
}

func TestRunWritesUsageRecordWhenNotDryRun(t *testing.T) {
	dir := t.TempDir()

	layout := specs.NewLayout(dir)
	writeAgentFixture(t, layout.AgentsDir, "preprocessor", "claude-opus-5")
	taskPath := filepath.Join(dir, "raw.md")
	if err := os.WriteFile(taskPath, []byte("do several things"), 0o644); err != nil {
		t.Fatal(err)
	}

	// claude-bin pointed at `true`(1) would still be a real subprocess call;
	// instead this test only needs Run to get past model resolution and
	// attempt the real CLIRunner path safely, so point --claude-bin at a
	// deterministic stub that emits well-formed --output-format json.
	stub := filepath.Join(dir, "fake-claude.sh")
	script := "#!/bin/sh\necho '{\"total_cost_usd\":0.01,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: dir, Task: "raw.md", ClaudeBin: stub}
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(layout.Root, "usage.jsonl"))
	if err != nil {
		t.Fatalf("expected usage.jsonl to be written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("usage.jsonl has %d lines, want 1; content:\n%s", len(lines), data)
	}
	var rec usage.Record
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("usage line not valid JSON: %v: %s", err, lines[0])
	}
	if rec.Agent != "preprocessor" || rec.CostUSD != 0.01 || rec.ExitCode != 0 || !rec.Parsed {
		t.Errorf("unexpected usage record: %+v", rec)
	}
}

func TestRunFailsWhenCallExceedsMaxCallCost(t *testing.T) {
	dir := t.TempDir()
	writeAgentFixture(t, specs.NewLayout(dir).AgentsDir, "preprocessor", "claude-opus-5")
	taskPath := filepath.Join(dir, "raw.md")
	if err := os.WriteFile(taskPath, []byte("do several things"), 0o644); err != nil {
		t.Fatal(err)
	}

	stub := filepath.Join(dir, "fake-claude.sh")
	script := "#!/bin/sh\necho '{\"total_cost_usd\":5.00,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Dir: dir, Task: "raw.md", ClaudeBin: stub, MaxCallCostUSD: 1}
	if err := Run(context.Background(), cfg); err == nil {
		t.Fatal("expected Run to fail when the call's cost exceeds --max-call-cost")
	}
}

// --max-call-minutes has minute granularity, too coarse to exercise a real
// kill-on-timeout here without a slow (near-a-minute) test; that mechanism
// is covered directly in internal/agents.TestCLIRunnerKillsCallExceedingMaxCallDuration.
// This package only wires cfg.MaxCallMinutes into CLIRunner.MaxCallDuration
// (see Run), and TestRunWritesUsageRecordWhenNotDryRun already exercises
// that wiring at its zero value (no limit).

func TestRunLogsPreprocessorModelWithSource(t *testing.T) {
	t.Run("env override", func(t *testing.T) {
		dir := t.TempDir()
		layout := specs.NewLayout(dir)
		writeAgentFixture(t, layout.AgentsDir, "preprocessor", "claude-opus-5")
		if err := os.WriteFile(filepath.Join(dir, "raw.md"), []byte("do several things"), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg := config.Config{Dir: dir, Task: "raw.md", DryRun: true,
			PreprocessorModel: agents.ModelOverride{Value: "claude-haiku-4-5-20251001", Source: agents.ModelSourceEnv}}
		got := captureStdout(t, func() {
			if err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run failed: %v", err)
			}
		})

		if !strings.Contains(got, "preprocessor=claude-haiku-4-5-20251001 (env)") {
			t.Errorf("expected the log to report the model and its source; got:\n%s", got)
		}
	})

	t.Run("built-in default", func(t *testing.T) {
		dir := t.TempDir()
		// No .claude/agents/preprocessor.md: the built-in default applies.
		if err := os.WriteFile(filepath.Join(dir, "raw.md"), []byte("do several things"), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg := config.Config{Dir: dir, Task: "raw.md", DryRun: true}
		got := captureStdout(t, func() {
			if err := Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run failed: %v", err)
			}
		})

		if !strings.Contains(got, "preprocessor=claude-opus-5 (built-in default)") {
			t.Errorf("expected the preprocessor default to stay claude-opus-5; got:\n%s", got)
		}
	})
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what
// was written to it. Run logs to os.Stdout directly (it builds its own
// logger), so that's the only way to assert on its output.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string, 1)
	go func() {
		var buf strings.Builder
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()
	w.Close()
	out := <-done
	r.Close()
	return out
}
