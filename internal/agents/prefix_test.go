package agents

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// loadRoles loads the built-in planner/worker/tester definitions and gives
// all three the same run prefix, exactly as the orchestrator does.
func loadRoles(t *testing.T, prefix string) []*Agent {
	t.Helper()
	empty := t.TempDir() // no project overrides: the built-in defaults
	var out []*Agent
	for _, name := range []string{"planner", "worker", "tester"} {
		a, err := LoadOrDefault(empty, name)
		if err != nil {
			t.Fatalf("LoadOrDefault(%q): %v", name, err)
		}
		a.Prefix = prefix
		out = append(out, a)
	}
	return out
}

func TestAssembledPromptsStartWithIdenticalPrefix(t *testing.T) {
	prefix := BuildPrefix(DefaultPrefix(), []string{"go"})
	roles := loadRoles(t, prefix)

	tasks := []string{
		"Task spec (001_a.md):\nplan it\n",
		"Task spec (001_a.md):\nimplement it\n",
		"Task spec (001_a.md):\nreview it\n",
	}

	var systems []string
	for i, a := range roles {
		p := Assemble(a, tasks[i])
		if !strings.HasPrefix(p.Full(), prefix) {
			t.Errorf("%s: assembled prompt does not start with the shared prefix", a.Name)
		}
		systems = append(systems, p.System)
	}

	if systems[0] == "" {
		t.Fatal("the shared prefix is empty")
	}
	for i := 1; i < len(systems); i++ {
		if systems[i] != systems[0] {
			t.Errorf("%s's system prompt differs from %s's; every role must get the same bytes",
				roles[i].Name, roles[0].Name)
		}
	}
}

func TestPrefixHasNoPerCallData(t *testing.T) {
	prefix := BuildPrefix(DefaultPrefix(), []string{"go"})

	// Two completely different calls: different tasks, different iteration
	// numbers, and a timestamp in the task text for good measure.
	first := Assemble(loadRoles(t, prefix)[0],
		"Task spec (001_a.md):\nfirst task\n\niteration 1 of 3\n"+time.Now().String())
	second := Assemble(loadRoles(t, prefix)[2],
		"Task spec (007_b.md):\nseventh task\n\niteration 3 of 3\n"+time.Now().String())

	if first.System != second.System {
		t.Fatal("the system prompt differs between two calls; it must be byte-identical")
	}

	year := strconv.Itoa(time.Now().Year())
	for _, forbidden := range []string{"001_a.md", "007_b.md", "iteration", "first task", "seventh task", year} {
		if strings.Contains(strings.ToLower(first.System), strings.ToLower(forbidden)) {
			t.Errorf("the prefix contains per-call data %q", forbidden)
		}
	}

	if again := BuildPrefix(DefaultPrefix(), []string{"go"}); again != prefix {
		t.Error("BuildPrefix is not deterministic for the same arguments")
	}
	for _, placeholder := range []string{"%s", "%d", "{{"} {
		if strings.Contains(DefaultPrefix(), placeholder) {
			t.Errorf("the built-in prefix contains an unsubstituted placeholder %q", placeholder)
		}
	}
}

func TestAssembleKeepsRolePromptVerbatimInUserTurn(t *testing.T) {
	worker := loadRoles(t, BuildPrefix(DefaultPrefix(), nil))[1]
	const task = "Task spec (001_a.md):\ndo the thing\n"

	p := Assemble(worker, task)
	if !strings.HasPrefix(p.User, worker.SystemPrompt) {
		t.Error("the user turn must start with the role instructions, unchanged")
	}
	if !strings.HasSuffix(p.User, task) {
		t.Error("the user turn must end with this call's task data")
	}
	if strings.Contains(p.System, worker.SystemPrompt) {
		t.Error("role instructions leaked into the shared system prompt")
	}
}

func TestAssembleWithoutPrefixOrRole(t *testing.T) {
	p := Assemble(&Agent{Name: "worker", SystemPrompt: "be a worker"}, "do the thing")
	if p.System != "" {
		t.Errorf("System = %q, want empty when the agent has no prefix", p.System)
	}
	if p.Full() != p.User {
		t.Errorf("Full() = %q, want just the user turn %q", p.Full(), p.User)
	}

	p = Assemble(&Agent{Name: "worker", Prefix: "shared"}, "do the thing")
	if p.User != "do the thing" {
		t.Errorf("User = %q, want just the task when the agent has no role prompt", p.User)
	}
}

func TestLoadPrefixOrDefault(t *testing.T) {
	t.Run("no override file", func(t *testing.T) {
		got, source, err := LoadPrefixOrDefault(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got != DefaultPrefix() {
			t.Error("want the built-in prefix when the project has no prefix.md")
		}
		if source != "built-in default" {
			t.Errorf("source = %q, want %q", source, "built-in default")
		}
	})

	t.Run("override file", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, PrefixFileName)
		if err := os.WriteFile(path, []byte("\nproject prefix\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, source, err := LoadPrefixOrDefault(root)
		if err != nil {
			t.Fatal(err)
		}
		if got != "project prefix" {
			t.Errorf("prefix = %q, want the override file's content", got)
		}
		if source != path {
			t.Errorf("source = %q, want %q", source, path)
		}
	})

	t.Run("blank override file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, PrefixFileName), []byte("  \n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadPrefixOrDefault(root); err == nil {
			t.Fatal("expected a blank prefix.md to be a startup error")
		}
	})
}

func TestBuildPrefixSkillsNote(t *testing.T) {
	base := "base text"
	if got := BuildPrefix(base, nil); got != base {
		t.Errorf("BuildPrefix(base, nil) = %q, want the base unchanged", got)
	}
	got := BuildPrefix(base, []string{"golang-testing", "golang-cli"})
	if !strings.HasPrefix(got, base) {
		t.Error("the skills note must come after the base text")
	}
	for _, want := range []string{".claude/skills", "golang-testing", "golang-cli"} {
		if !strings.Contains(got, want) {
			t.Errorf("skills note missing %q; got: %s", want, got)
		}
	}
}

func TestDefaultPrefixCoversLoopAndFiles(t *testing.T) {
	got := DefaultPrefix()
	for _, want := range []string{
		"planner", "worker", "tester",
		"STATUS.md", "QUEUE.md", "IMPL_PLAN", "REVIEW",
		"commit",
		"follow in the user message",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("built-in prefix missing %q", want)
		}
	}
}
