package agents

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrDefaultFallsBackToEmbeddedDefault(t *testing.T) {
	dir := t.TempDir() // no planner.md here at all

	a, err := LoadOrDefault(dir, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "planner" {
		t.Fatalf("Name = %q, want %q", a.Name, "planner")
	}
	if a.Model == "" || a.SystemPrompt == "" {
		t.Fatalf("expected embedded default to have a model and prompt, got %+v", a)
	}
}

func TestLoadOrDefaultPrefersProjectOverride(t *testing.T) {
	dir := t.TempDir()
	override := "---\nname: planner\nmodel: custom-model\n---\ncustom prompt"
	if err := os.WriteFile(filepath.Join(dir, "planner.md"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := LoadOrDefault(dir, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if a.Model != "custom-model" || a.SystemPrompt != "custom prompt" {
		t.Fatalf("expected project override to win, got %+v", a)
	}
}

func TestLoadOrDefaultUnknownAgentErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrDefault(dir, "not-a-real-agent"); err == nil {
		t.Fatal("expected an error for an agent with neither an override nor a built-in default")
	}
}

func TestAllBuiltinAgentsHaveDefaults(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"planner", "worker", "tester", "preprocessor"} {
		a, err := LoadOrDefault(dir, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if a.Model == "" {
			t.Errorf("%s: embedded default has no model", name)
		}
	}
}
