package agents

import "testing"

func TestApplyModelOverrideUsesOverrideWhenPresent(t *testing.T) {
	a := &Agent{Name: "planner", Model: "claude-fable-5-1"}
	if err := ApplyModelOverride(a, "claude-sonnet-5"); err != nil {
		t.Fatal(err)
	}
	if a.Model != "claude-sonnet-5" {
		t.Fatalf("Model = %q, want claude-sonnet-5", a.Model)
	}
}

func TestApplyModelOverrideKeepsLoadedModelWhenOverrideEmpty(t *testing.T) {
	a := &Agent{Name: "planner", Model: "claude-opus-5"}
	if err := ApplyModelOverride(a, ""); err != nil {
		t.Fatal(err)
	}
	if a.Model != "claude-opus-5" {
		t.Fatalf("Model = %q, want claude-opus-5", a.Model)
	}
}

func TestApplyModelOverrideRejectsUnknownModel(t *testing.T) {
	a := &Agent{Name: "planner", Model: "claude-opus-5"}
	if err := ApplyModelOverride(a, "made-up-model-9000"); err == nil {
		t.Fatal("expected an error for an unknown model")
	}
}

func TestApplyModelOverrideRejectsUnknownLoadedModelEvenWithoutOverride(t *testing.T) {
	a := &Agent{Name: "planner", Model: "some-typo-in-frontmatter"}
	if err := ApplyModelOverride(a, ""); err == nil {
		t.Fatal("expected an error for an unrecognized model coming from frontmatter/defaults")
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
