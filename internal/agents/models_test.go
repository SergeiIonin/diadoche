package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveModelUsesOverrideWhenPresent(t *testing.T) {
	a := &Agent{Name: "planner", Model: "claude-fable-5-1", ModelSource: ModelSourceFrontmatter}
	if err := ResolveModel(a, ModelOverride{Value: "claude-sonnet-5-5", Source: ModelSourceFlag}); err != nil {
		t.Fatal(err)
	}
	if a.Model != "claude-sonnet-5-5" {
		t.Fatalf("Model = %q, want claude-sonnet-5-5", a.Model)
	}
	if a.ModelSource != ModelSourceFlag {
		t.Fatalf("ModelSource = %q, want %q", a.ModelSource, ModelSourceFlag)
	}
}

func TestResolveModelKeepsLoadedModelWhenNoOverride(t *testing.T) {
	a := &Agent{Name: "planner", Model: "claude-opus-5", ModelSource: ModelSourceFrontmatter}
	if err := ResolveModel(a, ModelOverride{}); err != nil {
		t.Fatal(err)
	}
	if a.Model != "claude-opus-5" {
		t.Fatalf("Model = %q, want claude-opus-5", a.Model)
	}
	if a.ModelSource != ModelSourceFrontmatter {
		t.Fatalf("ModelSource = %q, want it untouched (%q)", a.ModelSource, ModelSourceFrontmatter)
	}
}

func TestResolveModelRejectsUnknownModel(t *testing.T) {
	a := &Agent{Name: "planner", Model: "claude-opus-5", ModelSource: ModelSourceFrontmatter}
	err := ResolveModel(a, ModelOverride{Value: "made-up-model-9000", Source: ModelSourceFlag})
	if err == nil {
		t.Fatal("expected an error for an unknown model")
	}
	if !strings.Contains(err.Error(), string(ModelSourceFlag)) {
		t.Errorf("expected the error to name the source %q, got: %v", ModelSourceFlag, err)
	}
}

func TestResolveModelRejectsUnknownLoadedModelEvenWithoutOverride(t *testing.T) {
	a := &Agent{Name: "planner", Model: "some-typo-in-frontmatter", ModelSource: ModelSourceFrontmatter}
	if err := ResolveModel(a, ModelOverride{}); err == nil {
		t.Fatal("expected an error for an unrecognized model coming from frontmatter/defaults")
	}
}

// TestResolveModelPriority walks the whole flag > env > frontmatter >
// built-in default chain the way a real run does: LoadOrDefault first, then
// ResolveModel on top of it.
func TestResolveModelPriority(t *testing.T) {
	const projectPrompt = "planner prompt from the project"

	writeProjectAgent := func(t *testing.T, frontmatter string) string {
		t.Helper()
		dir := t.TempDir()
		content := "---\nname: planner\n" + frontmatter + "---\n" + projectPrompt
		if err := os.WriteFile(filepath.Join(dir, "planner.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	tests := []struct {
		name        string
		frontmatter string // "" means: write no project file at all
		noFile      bool
		override    ModelOverride
		wantModel   string
		wantSource  ModelSource
	}{
		{
			name:        "flag beats everything",
			frontmatter: "model: claude-sonnet-5-5\n",
			override:    ModelOverride{Value: "claude-haiku-4-5-20251001", Source: ModelSourceFlag},
			wantModel:   "claude-haiku-4-5-20251001",
			wantSource:  ModelSourceFlag,
		},
		{
			name:        "env beats frontmatter",
			frontmatter: "model: claude-sonnet-5-5\n",
			override:    ModelOverride{Value: "claude-haiku-4-5-20251001", Source: ModelSourceEnv},
			wantModel:   "claude-haiku-4-5-20251001",
			wantSource:  ModelSourceEnv,
		},
		{
			name:        "frontmatter beats built-in default",
			frontmatter: "model: claude-sonnet-5-5\n",
			wantModel:   "claude-sonnet-5-5",
			wantSource:  ModelSourceFrontmatter,
		},
		{
			name:        "project prompt without a model falls through to the built-in default",
			frontmatter: "",
			wantModel:   "claude-opus-5-5",
			wantSource:  ModelSourceDefault,
		},
		{
			name:       "no project file at all uses the built-in default",
			noFile:     true,
			wantModel:  "claude-opus-5-5",
			wantSource: ModelSourceDefault,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if !tc.noFile {
				dir = writeProjectAgent(t, tc.frontmatter)
			}

			a, err := LoadOrDefault(dir, "planner")
			if err != nil {
				t.Fatal(err)
			}
			if err := ResolveModel(a, tc.override); err != nil {
				t.Fatal(err)
			}
			if a.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", a.Model, tc.wantModel)
			}
			if a.ModelSource != tc.wantSource {
				t.Errorf("ModelSource = %q, want %q", a.ModelSource, tc.wantSource)
			}
			// A model override must never disturb the prompt.
			if !tc.noFile && a.SystemPrompt != projectPrompt {
				t.Errorf("SystemPrompt = %q, want the project file's %q", a.SystemPrompt, projectPrompt)
			}
		})
	}
}

func TestResolveModelRejectsEmptyModel(t *testing.T) {
	t.Run("empty override", func(t *testing.T) {
		a := &Agent{Name: "planner", Model: "claude-opus-5-5", ModelSource: ModelSourceDefault}
		err := ResolveModel(a, ModelOverride{Value: "", Source: ModelSourceFlag})
		if err == nil {
			t.Fatal("expected an error for an explicitly empty model override")
		}
		if !strings.Contains(err.Error(), "empty model") {
			t.Errorf("expected an \"empty model\" error, got: %v", err)
		}
	})

	t.Run("blank model in project frontmatter", func(t *testing.T) {
		dir := t.TempDir()
		content := "---\nname: planner\nmodel:\n---\nprompt"
		if err := os.WriteFile(filepath.Join(dir, "planner.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		a, err := LoadOrDefault(dir, "planner")
		if err != nil {
			t.Fatal(err)
		}
		err = ResolveModel(a, ModelOverride{})
		if err == nil {
			t.Fatal("expected an error for a blank model: field")
		}
		if !strings.Contains(err.Error(), "empty model") {
			t.Errorf("expected an \"empty model\" error, got: %v", err)
		}
	})
}

func TestModelOverrideIsSet(t *testing.T) {
	if (ModelOverride{}).IsSet() {
		t.Error("the zero ModelOverride must not count as set")
	}
	if !(ModelOverride{Value: "claude-opus-5-5", Source: ModelSourceFlag}).IsSet() {
		t.Error("a flag override must count as set")
	}
}

func TestModelDescriptionNamesModelAndSource(t *testing.T) {
	a := &Agent{Name: "tester", Model: "claude-sonnet-5-5", ModelSource: ModelSourceEnv}
	if got, want := a.ModelDescription(), "claude-sonnet-5-5 (env)"; got != want {
		t.Errorf("ModelDescription() = %q, want %q", got, want)
	}
}

func TestAllBuiltinModelsAreKnown(t *testing.T) {
	for _, name := range DefaultNames {
		a, err := LoadOrDefault(t.TempDir(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !KnownModels[a.Model] {
			t.Errorf("%s: built-in default model %q is not in KnownModels", name, a.Model)
		}
	}
}
