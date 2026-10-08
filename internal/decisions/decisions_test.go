package decisions

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"diadoche/internal/agents"
)

func TestParseMissingSection(t *testing.T) {
	ds, found := Parse("Implemented the plan.\n\n## Summary\n\nAll tests pass.\n")
	if found || len(ds) != 0 {
		t.Fatalf("Parse = %v, %v; want empty, not found", ds, found)
	}
}

func TestParseEmptyAndNoneSections(t *testing.T) {
	for _, in := range []string{
		"text\n\n## Decisions\n",
		"## Decisions\n\n\n",
		"## Decisions\n\nnone\n",
		"Done.\n\n## Decisions\n\nNone.\n",
		"## Decisions\n\n`none`\n",
		"## Decisions\n\nnone\n\n## Other\n\n### Not a decision\nContext: c\n",
	} {
		ds, found := Parse(in)
		if !found || ds == nil || len(ds) != 0 {
			t.Errorf("Parse(%q) = %#v, %v; want non-nil empty, found", in, ds, found)
		}
	}
}

func TestParseTwoDecisionsEveryFieldIntact(t *testing.T) {
	answer := `I implemented the plan: added the cache and wired it into the server.
All tests pass.

## Decisions

### Store the cache index as JSON lines
Context: The index must survive restarts. Only one writer appends to it.
Decision: Write one JSON object per line to index.jsonl.
Alternatives: SQLite — rejected, new dependency; one JSON document — rejected, full rewrites.

### Add a /healthz endpoint
Context: The deploy needs a liveness probe.
Decision: Serve GET /healthz returning 200 with an empty body.
Alternatives: none considered
`
	ds, found := Parse(answer)
	if !found {
		t.Fatal("section not found")
	}
	want := []Decision{
		{
			Title:        "Store the cache index as JSON lines",
			Context:      "The index must survive restarts. Only one writer appends to it.",
			Decision:     "Write one JSON object per line to index.jsonl.",
			Alternatives: "SQLite — rejected, new dependency; one JSON document — rejected, full rewrites.",
		},
		{
			Title:        "Add a /healthz endpoint",
			Context:      "The deploy needs a liveness probe.",
			Decision:     "Serve GET /healthz returning 200 with an empty body.",
			Alternatives: "none considered",
		},
	}
	if len(ds) != len(want) {
		t.Fatalf("got %d decisions, want %d: %#v", len(ds), len(want), ds)
	}
	for i := range want {
		if ds[i].Title != want[i].Title {
			t.Errorf("[%d].Title = %q, want %q", i, ds[i].Title, want[i].Title)
		}
		if ds[i].Context != want[i].Context {
			t.Errorf("[%d].Context = %q, want %q", i, ds[i].Context, want[i].Context)
		}
		if ds[i].Decision != want[i].Decision {
			t.Errorf("[%d].Decision = %q, want %q", i, ds[i].Decision, want[i].Decision)
		}
		if ds[i].Alternatives != want[i].Alternatives {
			t.Errorf("[%d].Alternatives = %q, want %q", i, ds[i].Alternatives, want[i].Alternatives)
		}
	}
}

func TestParseSeveralDecisionsMultilineCRLF(t *testing.T) {
	answer := strings.Join([]string{
		"## Decisions",
		"",
		"### One",
		"Context: first line",
		"second line",
		"",
		"Decision: d1",
		"Alternatives: a1",
		"### Two",
		"- **Context:** c2",
		"**Decision**: d2",
		"Alternatives:",
		"a2 on its own line",
		"### Three",
		"Context: c3",
		"Decision: d3",
		"Alternatives: a3",
		"",
	}, "\r\n")
	ds, found := Parse(answer)
	want := []Decision{
		{"One", "first line\nsecond line", "d1", "a1"},
		{"Two", "c2", "d2", "a2 on its own line"},
		{"Three", "c3", "d3", "a3"},
	}
	if !found || !reflect.DeepEqual(ds, want) {
		t.Fatalf("Parse = %#v, %v\nwant %#v", ds, found, want)
	}
}

func TestParseLastHeadingWins(t *testing.T) {
	answer := "The prompt asks for:\n\n> ## Decisions\n\n```\n## Decisions\n\n### Example\nContext: x\nDecision: y\nAlternatives: z\n```\n\n" +
		"## Decisions\n\n### Real\nContext: c\nDecision: d\nAlternatives: a\n"
	ds, found := Parse(answer)
	want := []Decision{{"Real", "c", "d", "a"}}
	if !found || !reflect.DeepEqual(ds, want) {
		t.Fatalf("Parse = %#v, %v; want %#v", ds, found, want)
	}
}

func TestParseSectionEndsAtNextHeading(t *testing.T) {
	answer := "## Decisions\n\n### Only\nContext: c\nDecision: d\nAlternatives: a\n\n## Notes\n\nnot part of it\n# Top\nnor this\n"
	ds, found := Parse(answer)
	want := []Decision{{"Only", "c", "d", "a"}}
	if !found || !reflect.DeepEqual(ds, want) {
		t.Fatalf("Parse = %#v, %v; want %#v", ds, found, want)
	}
}

func TestMarshal(t *testing.T) {
	b, err := Marshal(nil)
	if err != nil || string(b) != "[]\n" {
		t.Fatalf("Marshal(nil) = %q, %v; want \"[]\\n\"", b, err)
	}
	in := []Decision{{Title: "T", Context: "C", Decision: "D", Alternatives: "A"}}
	b, err = Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := "[\n  {\n    \"title\": \"T\",\n    \"context\": \"C\",\n    \"decision\": \"D\",\n    \"alternatives\": \"A\"\n  }\n]\n"
	if string(b) != want {
		t.Fatalf("Marshal = %q, want %q", b, want)
	}
	var back []Decision
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip = %#v, %v; want %#v", back, err, in)
	}
}

// TestBuiltinWorkerPromptExampleParses keeps the layout documented in the
// built-in worker prompt and this parser from drifting apart.
func TestBuiltinWorkerPromptExampleParses(t *testing.T) {
	b, err := agents.DefaultPromptFile("worker")
	if err != nil {
		t.Fatal(err)
	}
	var example string
	parts := strings.Split(string(b), "```")
	for i := 1; i < len(parts); i += 2 {
		block := strings.TrimPrefix(parts[i], "\n")
		if strings.HasPrefix(block, Heading+"\n") && strings.Contains(block, "### ") {
			example = block
			break
		}
	}
	if example == "" {
		t.Fatal("built-in worker prompt has no fenced ## Decisions example with entries")
	}
	ds, found := Parse(example)
	if !found || len(ds) < 2 {
		t.Fatalf("prompt example parses to %#v, %v; want at least 2 entries", ds, found)
	}
	for i, d := range ds {
		if d.Title == "" || d.Context == "" || d.Decision == "" || d.Alternatives == "" {
			t.Errorf("prompt example entry %d has an empty field: %#v", i, d)
		}
	}
}
