package runstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := Path(t.TempDir())
	want := State{
		RunID: "20261008T120000Z-00ff00ff",
		Tasks: []Task{{
			Spec: "009_x.md", Num: "009", Iteration: 2, Agent: "worker", AgentState: AgentDone,
			BaseCommit: "abc123", Worktree: ".claude/diadoche/worktrees/009-x",
		}},
		UpdatedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC),
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RunID != want.RunID || !got.UpdatedAt.Equal(want.UpdatedAt) || len(got.Tasks) != 1 || got.Tasks[0] != want.Tasks[0] {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestNilTaskIsJSONNull(t *testing.T) {
	path := Path(t.TempDir())
	if err := Save(path, State{RunID: "r", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"task": null`) {
		t.Errorf("file = %s, want \"task\": null", raw)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"tasks": []`) {
		t.Errorf("file = %s, want \"tasks\": []", raw)
	}
	if len(got.Tasks) != 0 {
		t.Errorf("Tasks = %+v, want none", got.Tasks)
	}
}

func TestWorktreeOmittedWhenEmpty(t *testing.T) {
	path := Path(t.TempDir())
	if err := Save(path, State{RunID: "r", Tasks: []Task{{Spec: "a.md", Iteration: 1, Agent: "planner", AgentState: AgentPending}}}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "worktree") {
		t.Errorf("file = %s, want no worktree field", raw)
	}
}

func TestLoadMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	if s, err := Load(Path(dir)); s != nil || err != nil {
		t.Errorf("Load(missing) = %v, %v; want nil, nil", s, err)
	}
	for _, body := range []string{`{"run_id": "r", "task": {"spec"`, "garbage", `{"task": null}`} {
		if err := os.WriteFile(Path(dir), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(Path(dir)); err == nil {
			t.Errorf("Load(%q) = nil error, want one", body)
		}
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir)
	if err := Save(path, State{RunID: "first"}); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldInfo, _ := old.Stat()

	// A temp file left behind by a write that was cut off never matters.
	if err := os.WriteFile(filepath.Join(dir, ".run-state.json.tmp123"), []byte(`{"run_id": "tru`), 0o644); err != nil {
		t.Fatal(err)
	}

	for i, agent := range Agents {
		s := State{RunID: "second", Tasks: []Task{{Spec: "a.md", Iteration: i + 1, Agent: agent, AgentState: AgentRunning}}}
		if err := Save(path, s); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(path)
		if !json.Valid(raw) {
			t.Fatalf("file is not valid JSON after save %d: %s", i, raw)
		}
		got, err := Load(path)
		if err != nil || got.Tasks[0].Agent != agent {
			t.Fatalf("Load after save %d = %+v, %v", i, got, err)
		}
	}

	newInfo, _ := os.Stat(path)
	if os.SameFile(oldInfo, newInfo) {
		t.Error("Save rewrote the file in place; want a temp file renamed over it")
	}
	leftovers, _ := filepath.Glob(filepath.Join(dir, ".run-state.json.tmp*"))
	if len(leftovers) != 1 || filepath.Base(leftovers[0]) != ".run-state.json.tmp123" {
		t.Errorf("temp files = %v, want only the pre-existing .run-state.json.tmp123", leftovers)
	}
}

func TestNext(t *testing.T) {
	cases := []struct {
		agent, state string
		iter         int
		wantAgent    string
	}{
		{"planner", AgentDone, 2, "worker"},
		{"worker", AgentDone, 2, "tester"},
		{"tester", AgentDone, 3, ""},
		{"planner", AgentRunning, 2, "planner"},
		{"worker", AgentRunning, 2, "worker"},
		{"tester", AgentRunning, 2, "tester"},
		{"planner", AgentPending, 2, "planner"},
		{"worker", AgentPending, 2, "worker"},
		{"tester", AgentPending, 2, "tester"},
	}
	for _, c := range cases {
		iter, agent := Task{Spec: "a.md", Iteration: 2, Agent: c.agent, AgentState: c.state}.Next()
		if iter != c.iter || agent != c.wantAgent {
			t.Errorf("Next(%s %s) = (%d, %q), want (%d, %q)", c.agent, c.state, iter, agent, c.iter, c.wantAgent)
		}
	}
}

func TestValidate(t *testing.T) {
	ok := Task{Spec: "a.md", Iteration: 1, Agent: "tester", AgentState: AgentRunning}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate(%+v) = %v", ok, err)
	}
	bad := []Task{
		{Spec: "a.md", Iteration: 1, Agent: "reviewer", AgentState: AgentRunning},
		{Spec: "a.md", Iteration: 1, Agent: "tester", AgentState: "half"},
		{Spec: "a.md", Iteration: 0, Agent: "tester", AgentState: AgentRunning},
		{Iteration: 1, Agent: "tester", AgentState: AgentRunning},
		{Spec: "a.md", Iteration: 1, Agent: "tester", AgentState: AgentRunning, Attempt: -1},
	}
	for _, b := range bad {
		if err := b.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", b)
		}
	}
}

func TestNewRunID(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 30, 0, 0, time.FixedZone("x", 3600))
	a, b := NewRunID(now), NewRunID(now)
	if !regexp.MustCompile(`^20261008T113000Z-[0-9a-f]{8}$`).MatchString(a) {
		t.Errorf("NewRunID = %q", a)
	}
	if a == b {
		t.Errorf("two run ids are equal: %q", a)
	}
}

func TestWaitingRoundTripsAndIsOmittedWhenEmpty(t *testing.T) {
	path := Path(t.TempDir())
	rec := Task{Spec: "001_x.md", Num: "001", Iteration: 1, Agent: "planner", AgentState: AgentPending, BaseCommit: "abc"}
	want := State{RunID: "r", UpdatedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), Waiting: map[string]Task{"001_x.md": rec}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Waiting["001_x.md"] != rec || len(got.Waiting) != 1 {
		t.Errorf("waiting = %+v, want %+v", got.Waiting, want.Waiting)
	}

	if err := Save(path, State{RunID: "r"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "waiting") {
		t.Errorf("empty waiting not omitted:\n%s", b)
	}

	// A file written before the key existed still loads.
	old := `{"run_id": "old", "task": null, "updated_at": "2026-10-08T12:00:00Z"}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(path); err != nil || s.RunID != "old" || s.Waiting != nil {
		t.Errorf("old file = %+v, %v", s, err)
	}
}

func TestSeveralTasksRoundTripAndMirrorTheFirstAsTask(t *testing.T) {
	path := Path(t.TempDir())
	a := Task{Spec: "001_a.md", Num: "001", Iteration: 1, Agent: "worker", AgentState: AgentRunning, BaseCommit: "abc", Worktree: ".claude/diadoche/worktrees/001-a"}
	b := Task{Spec: "002_b.md", Num: "002", Iteration: 2, Agent: "planner", AgentState: AgentPending, BaseCommit: "def", Attempt: 2}
	if err := Save(path, State{RunID: "r", Tasks: []Task{a, b}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var j struct {
		Tasks []Task `json:"tasks"`
		Task  *Task  `json:"task"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	if len(j.Tasks) != 2 || j.Task == nil || *j.Task != a {
		t.Fatalf("file = %s, want two tasks and \"task\" = the first", raw)
	}
	if !strings.Contains(string(raw), `"attempt": 2`) || strings.Count(string(raw), `"attempt"`) != 1 {
		t.Errorf("file = %s, want attempt only on the second record", raw)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 2 || got.Tasks[0] != a || got.Tasks[1] != b {
		t.Errorf("Tasks = %+v, want %+v", got.Tasks, []Task{a, b})
	}
}

func TestLegacySingleTaskFileLoadsAsOneRecord(t *testing.T) {
	path := Path(t.TempDir())
	old := `{"run_id": "old", "task": {"spec": "001_a.md", "num": "001", "iteration": 1, "agent": "tester", "agent_state": "running", "base_commit": "abc"}, "updated_at": "2026-10-08T12:00:00Z"}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Task{Spec: "001_a.md", Num: "001", Iteration: 1, Agent: "tester", AgentState: AgentRunning, BaseCommit: "abc"}
	if len(got.Tasks) != 1 || got.Tasks[0] != want {
		t.Errorf("Tasks = %+v, want [%+v]", got.Tasks, want)
	}
}

func TestTasksWinOverTask(t *testing.T) {
	path := Path(t.TempDir())
	both := `{"run_id": "r", "tasks": [{"spec": "002_b.md", "num": "002", "iteration": 1, "agent": "planner", "agent_state": "pending"}], "task": {"spec": "001_a.md", "num": "001", "iteration": 1, "agent": "tester", "agent_state": "running"}}`
	if err := os.WriteFile(path, []byte(both), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].Spec != "002_b.md" {
		t.Errorf("Tasks = %+v, want only 002_b.md from \"tasks\"", got.Tasks)
	}
}
