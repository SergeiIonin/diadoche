package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriterAppendsOneLinePerRecordAndBuildsSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "usage.jsonl")

	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	records := []Record{
		{Task: "001_a.md", Agent: "planner", Iteration: 1, Model: "claude-opus-5", InputTokens: 100, OutputTokens: 50, CostUSD: 0.01, DurationMS: 1000, Parsed: true},
		{Task: "001_a.md", Agent: "worker", Iteration: 1, Model: "claude-opus-5", InputTokens: 200, OutputTokens: 150, CostUSD: 0.05, DurationMS: 5000, Parsed: true},
		{Task: "002_b.md", Agent: "planner", Iteration: 1, Model: "claude-opus-5", InputTokens: 80, OutputTokens: 40, CostUSD: 0.008, DurationMS: 800, Parsed: true},
	}
	for _, r := range records {
		if err := w.Append(r); err != nil {
			t.Fatal(err)
		}
	}

	if got := w.Summary.Calls(); got != 3 {
		t.Fatalf("Summary.Calls() = %d, want 3", got)
	}
	if w.Summary.ByAgent["planner"].Calls != 2 {
		t.Fatalf("planner calls = %d, want 2", w.Summary.ByAgent["planner"].Calls)
	}
	if w.Summary.ByAgent["worker"].Calls != 1 {
		t.Fatalf("worker calls = %d, want 1", w.Summary.ByAgent["worker"].Calls)
	}
	if w.Summary.ByTask["001_a.md"].Calls != 2 {
		t.Fatalf("001_a.md calls = %d, want 2", w.Summary.ByTask["001_a.md"].Calls)
	}
	wantCost := 0.01 + 0.05 + 0.008
	if diff := w.Summary.Overall.CostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("Overall.CostUSD = %v, want %v", w.Summary.Overall.CostUSD, wantCost)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines++
		var rec Record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("line %d not valid JSON: %v", lines, err)
		}
	}
	if lines != 3 {
		t.Fatalf("usage.jsonl has %d lines, want 3", lines)
	}
}

func TestSummaryFormatContainsBreakdowns(t *testing.T) {
	s := NewSummary()
	s.add(Record{Task: "001_a.md", Agent: "planner", CostUSD: 0.01, InputTokens: 10, OutputTokens: 5})
	s.add(Record{Task: "001_a.md", Agent: "tester", CostUSD: 0.02, InputTokens: 20, OutputTokens: 8})

	out := s.Format()
	for _, want := range []string{"usage summary", "planner", "tester", "001_a.md", "$0.03"} {
		if !strings.Contains(out, want) {
			t.Errorf("Format() missing %q; got:\n%s", want, out)
		}
	}
}

func TestOpenCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "usage.jsonl")
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected usage.jsonl to exist: %v", err)
	}
}
