package agents

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLoadOrDefaultProjectPromptWithoutModelUsesDefaultModel(t *testing.T) {
	dir := t.TempDir()
	override := "---\nname: worker\n---\nonly the prompt is customized"
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := LoadOrDefault(dir, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if a.SystemPrompt != "only the prompt is customized" {
		t.Errorf("SystemPrompt = %q, want the project file's body", a.SystemPrompt)
	}
	if a.Model != "claude-opus-5-5" {
		t.Errorf("Model = %q, want the built-in default claude-opus-5-5", a.Model)
	}
	if a.ModelSource != ModelSourceDefault {
		t.Errorf("ModelSource = %q, want %q", a.ModelSource, ModelSourceDefault)
	}
}

func TestLoadOrDefaultSetsModelSource(t *testing.T) {
	embedded, err := LoadOrDefault(t.TempDir(), "planner")
	if err != nil {
		t.Fatal(err)
	}
	if embedded.ModelSource != ModelSourceDefault {
		t.Errorf("embedded default ModelSource = %q, want %q", embedded.ModelSource, ModelSourceDefault)
	}

	dir := t.TempDir()
	override := "---\nname: planner\nmodel: claude-fable-5-1\n---\ncustom prompt"
	if err := os.WriteFile(filepath.Join(dir, "planner.md"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := LoadOrDefault(dir, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if project.ModelSource != ModelSourceFrontmatter {
		t.Errorf("project override ModelSource = %q, want %q", project.ModelSource, ModelSourceFrontmatter)
	}
}

// TestBuiltinDefaultModels pins the built-in defaults: they're a cost
// decision (001_agent_models_config.md), not an implementation detail, so
// changing one should fail here and be changed on purpose.
func TestBuiltinDefaultModels(t *testing.T) {
	want := map[string]string{
		"planner":      "claude-opus-5-5",
		"worker":       "claude-opus-5-5",
		"tester":       "claude-sonnet-5-5",
		"preprocessor": "claude-opus-5",
	}
	for name, model := range want {
		a, err := LoadOrDefault(t.TempDir(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if a.Model != model {
			t.Errorf("%s: built-in default model = %q, want %q", name, a.Model, model)
		}
	}
}

// TestEmbeddedTesterPromptIsDiffScoped pins the other half of the plan
// contract into the built-in tester prompt: tester reviews from the diff
// it is handed, in either of the two shapes the orchestrator can send, and
// never re-explores the repository on its own.
func TestEmbeddedTesterPromptIsDiffScoped(t *testing.T) {
	tester, err := LoadOrDefault(t.TempDir(), "tester")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"diff", "untracked", "--stat", "ambiguous", "do not re-explore", "## Test Command"} {
		if !strings.Contains(tester.SystemPrompt, want) {
			t.Errorf("embedded tester prompt is missing %q", want)
		}
	}
}

// TestEmbeddedPlannerAndWorkerPromptsCarryPlanContract pins the
// IMPL_PLAN.md contract into the built-in prompts: planner must describe
// both required sections and the per-file create/modify marker, and worker
// must be told to work from that list and how to extend it. The headings are
// spelled out literally here on purpose — implplan parses the same strings.
func TestEmbeddedPlannerAndWorkerPromptsCarryPlanContract(t *testing.T) {
	planner, err := LoadOrDefault(t.TempDir(), "planner")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Files", "## Test Command", "create", "modify", "linter", "at least one file"} {
		if !strings.Contains(planner.SystemPrompt, want) {
			t.Errorf("embedded planner prompt is missing %q", want)
		}
	}

	worker, err := LoadOrDefault(t.TempDir(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Files", "justif", "IMPL_PLAN.md"} {
		if !strings.Contains(worker.SystemPrompt, want) {
			t.Errorf("embedded worker prompt is missing %q", want)
		}
	}
	// The ban on editing spec files must not swallow the one exception
	// worker needs: appending to the plan's Files section.
	if strings.Contains(worker.SystemPrompt, "spec/NNN_*") {
		t.Error("embedded worker prompt still bans all spec/NNN_* files, which contradicts appending to the plan's Files section")
	}
}
