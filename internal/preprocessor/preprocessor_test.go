package preprocessor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diadoche/internal/config"
	"diadoche/internal/specs"
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

	cfg := config.Config{Dir: dir, Task: "raw.md", DryRun: true, PreprocessorModel: "not-a-real-model"}
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
	if !strings.Contains(string(data), `"agent":"preprocessor"`) || !strings.Contains(string(data), `"cost_usd":0.01`) {
		t.Errorf("unexpected usage.jsonl content: %s", data)
	}
}
