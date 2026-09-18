package initializer

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"diadoche/internal/agents"
	"diadoche/internal/specs"
)

func TestRunScaffoldsFreshProject(t *testing.T) {
	dir := t.TempDir()
	logger := log.New(io.Discard, "", 0)

	if err := Run(dir, logger); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	layout := specs.NewLayout(dir)

	if _, err := os.Stat(layout.StatusPath); err != nil {
		t.Errorf("expected %s to exist: %v", layout.StatusPath, err)
	}
	if _, err := os.Stat(filepath.Join(layout.SpecDir, "raw")); err != nil {
		t.Errorf("expected spec/raw to exist: %v", err)
	}
	if _, err := os.Stat(layout.SkillsDir); err != nil {
		t.Errorf("expected skills dir to exist: %v", err)
	}
	for _, name := range agents.DefaultNames {
		path := filepath.Join(layout.AgentsDir, name+".md")
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("expected %s to exist: %v", path, err)
			continue
		}
		want, err := agents.DefaultPromptFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s content = %q, want built-in default %q", path, got, want)
		}
	}
}

func TestRunDoesNotOverwriteExistingAgentFile(t *testing.T) {
	dir := t.TempDir()
	logger := log.New(io.Discard, "", 0)

	layout := specs.NewLayout(dir)
	if err := os.MkdirAll(layout.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	customized := "---\nname: planner\nmodel: custom-model\n---\ncustom prompt"
	plannerPath := filepath.Join(layout.AgentsDir, "planner.md")
	if err := os.WriteFile(plannerPath, []byte(customized), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Run(dir, logger); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	got, err := os.ReadFile(plannerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != customized {
		t.Fatalf("Run overwrote a customized agent file: got %q, want %q", got, customized)
	}
}

func TestRunUsesExistingClaudeDir(t *testing.T) {
	dir := t.TempDir()
	logger := log.New(io.Discard, "", 0)

	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, ".claude", "keep-me.txt")
	if err := os.WriteFile(marker, []byte("pre-existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Run(dir, logger); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected pre-existing file under .claude to survive: %v", err)
	}
	layout := specs.NewLayout(dir)
	if _, err := os.Stat(layout.StatusPath); err != nil {
		t.Errorf("expected scaffolding inside the pre-existing .claude dir: %v", err)
	}
}
