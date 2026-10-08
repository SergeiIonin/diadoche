package questions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = "# Questions\r\n\r\nSome preamble that is not an entry.\r\n\r\n" +
	`## Question 018-1: Where should the cache live?
Task: 018_cache.md
Asked-by: planner
Question: Should the cache live in memory
or on disk?
Options:
- A: memory
- B: disk
Default: A
Why: simpler, and the data is small
Reversible: no
Answer: B — it must survive restarts.
Also keep it under .cache/.

## Question 019-1: Colour of the button
Task: ` + "`019_button.md`" + `
asked-by: worker
Question: Blue or green?
Options:
- A: blue
- B: green
Default: A
Why: brand colour
Reversible: yes
Answer:
`

func TestParseEntries(t *testing.T) {
	entries := Parse([]byte(sample))
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.ID != "018-1" || e.Summary != "Where should the cache live?" {
		t.Errorf("heading = %q / %q", e.ID, e.Summary)
	}
	if e.Task != "018_cache.md" || e.AskedBy != "planner" || e.Default != "A" || e.Reversible != "no" {
		t.Errorf("fields = %+v", e)
	}
	if e.Question != "Should the cache live in memory\nor on disk?" {
		t.Errorf("multi-line question = %q", e.Question)
	}
	if e.Options != "- A: memory\n- B: disk" {
		t.Errorf("options = %q", e.Options)
	}
	if e.Answer != "B — it must survive restarts.\nAlso keep it under .cache/." {
		t.Errorf("answer = %q", e.Answer)
	}
	if !e.Answered() {
		t.Error("entry 1 should be answered")
	}
	if entries[1].AskedBy != "worker" || entries[1].Answer != "" || entries[1].Answered() {
		t.Errorf("entry 2 = %+v, want unanswered, asked by worker", entries[1])
	}
}

func TestAnsweredPlaceholders(t *testing.T) {
	for answer, want := range map[string]bool{"": false, "  ": false, "-": false, "TBD": false, "unanswered": false, "(Unanswered)": false, "A": true, "go with B": true} {
		if got := (Entry{Answer: answer}).Answered(); got != want {
			t.Errorf("Answered(%q) = %v, want %v", answer, got, want)
		}
	}
}

func TestForTask(t *testing.T) {
	entries := []Entry{
		{ID: "018-1", Task: "18"},
		{ID: "018-2", Task: "018"},
		{ID: "018-3", Task: "`018_x.md`"},
		{ID: "018-4"},
		{ID: "019-1", Task: "019_y.md"},
		{ID: "x-1", Task: "180_z.md"},
	}
	got := ForTask(entries, "018_x.md")
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if strings.Join(ids, ",") != "018-1,018-2,018-3,018-4" {
		t.Errorf("ForTask = %v", ids)
	}
}

func TestLoadMissingFile(t *testing.T) {
	entries, err := Load(filepath.Join(t.TempDir(), FileName))
	if err != nil || entries != nil {
		t.Fatalf("Load(missing) = %v, %v; want nil, nil", entries, err)
	}
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	if entries, err := Load(path); err != nil || len(entries) != 2 {
		t.Fatalf("Load = %d entries, %v", len(entries), err)
	}
}

func TestPromptBlock(t *testing.T) {
	entries := Parse([]byte(sample))
	block := PromptBlock("/repo/QUESTIONS.md", entries)
	for _, want := range []string{
		"/repo/QUESTIONS.md",
		"Question 018-1: Should the cache live in memory",
		"- B: disk",
		"Default: A",
		"Answer: B — it must survive restarts.",
		"Question 019-1: Blue or green?",
		UnansweredLabel,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}
	empty := PromptBlock("/repo/QUESTIONS.md", nil)
	if !strings.Contains(empty, "/repo/QUESTIONS.md") || strings.Contains(empty, "Questions about this task") {
		t.Errorf("block without entries = %q", empty)
	}
}

func TestNeedsHuman(t *testing.T) {
	yes := []string{
		"I asked in QUESTIONS.md.\nNEEDS_HUMAN\n",
		"  NEEDS_HUMAN  ",
		"text\r\nNEEDS_HUMAN\r\n\r\n## Decisions\r\n- none\r\n",
	}
	for _, s := range yes {
		if !NeedsHuman(s) {
			t.Errorf("NeedsHuman(%q) = false, want true", s)
		}
	}
	no := []string{
		"",
		"I would end with NEEDS_HUMAN here, but it is reversible.",
		"`NEEDS_HUMAN`",
		"**NEEDS_HUMAN**",
		"NEEDS_HUMAN.",
		"Marker: NEEDS_HUMAN",
		"The marker looks like this:\n```\nNEEDS_HUMAN\n```\nbut I don't need it.",
		"~~~text\nNEEDS_HUMAN\n~~~",
		"needs_human",
	}
	for _, s := range no {
		if NeedsHuman(s) {
			t.Errorf("NeedsHuman(%q) = true, want false", s)
		}
	}
}
