package usage

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		{Time: time.Now(), Task: "001_a.md", Agent: "planner", Iteration: 1, Model: "claude-opus-5", InputTokens: 100, OutputTokens: 50, CacheCreationInputTokens: 10, CacheReadInputTokens: 900, CostUSD: 0.01, DurationMS: 1000, ExitCode: 0, Parsed: true},
		{Time: time.Now(), Task: "001_a.md", Agent: "worker", Iteration: 1, Model: "claude-opus-5", InputTokens: 200, OutputTokens: 150, CacheCreationInputTokens: 20, CacheReadInputTokens: 1800, CostUSD: 0.05, DurationMS: 5000, ExitCode: 1, Parsed: true},
		{Time: time.Now(), Task: "002_b.md", Agent: "planner", Iteration: 1, Model: "claude-opus-5", InputTokens: 80, OutputTokens: 40, CostUSD: 0.008, DurationMS: 800, ExitCode: 0, Parsed: true},
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
	wantKeys := []string{
		"time", "task", "agent", "iteration", "model",
		"input_tokens", "output_tokens", "cache_creation_input_tokens",
		"cache_read_input_tokens", "cost_usd", "duration_ms", "exit_code", "parsed",
	}
	var lines int
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines++
		var rec Record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("line %d not valid JSON: %v", lines, err)
		}
		var fields map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &fields); err != nil {
			t.Fatalf("line %d not a JSON object: %v", lines, err)
		}
		for _, key := range wantKeys {
			if _, ok := fields[key]; !ok {
				t.Errorf("line %d is missing the %q field: %s", lines, key, scanner.Bytes())
			}
		}
		if rec.Agent == "worker" && rec.ExitCode != 1 {
			t.Errorf("worker record exit_code = %d, want 1", rec.ExitCode)
		}
	}
	if lines != 3 {
		t.Fatalf("usage.jsonl has %d lines, want 3", lines)
	}
}

func TestSummaryFormatReportsCacheReadShare(t *testing.T) {
	s := NewSummary()
	s.add(Record{Task: "001_a.md", Agent: "planner", InputTokens: 100, CacheCreationInputTokens: 100, CacheReadInputTokens: 800})

	out := s.Format()
	// 800 cache-read of 1000 total input tokens (output tokens excluded).
	if !strings.Contains(out, "80.0%") {
		t.Errorf("Format() missing the cache-read share 80.0%%; got:\n%s", out)
	}
	if !strings.Contains(out, "cache-read share of all input tokens") {
		t.Errorf("Format() missing the cache-read share line; got:\n%s", out)
	}

	empty := NewSummary()
	empty.add(Record{Task: "001_a.md", Agent: "planner"})
	if out := empty.Format(); !strings.Contains(out, "0.0%") {
		t.Errorf("a zero-token summary must print 0.0%%, not NaN; got:\n%s", out)
	}
}

func TestSummaryTotalsEqualSumOfRecords(t *testing.T) {
	records := []Record{
		{Task: "001_a.md", Agent: "planner", InputTokens: 10, OutputTokens: 1, CacheCreationInputTokens: 100, CacheReadInputTokens: 1000, CostUSD: 0.01, DurationMS: 100},
		{Task: "001_a.md", Agent: "worker", InputTokens: 20, OutputTokens: 2, CacheCreationInputTokens: 200, CacheReadInputTokens: 2000, CostUSD: 0.02, DurationMS: 200},
		{Task: "002_b.md", Agent: "worker", InputTokens: 30, OutputTokens: 3, CacheCreationInputTokens: 300, CacheReadInputTokens: 3000, CostUSD: 0.03, DurationMS: 300},
	}
	s := NewSummary()
	for _, r := range records {
		s.add(r)
	}

	sum := func(rs ...Record) totals {
		var t totals
		for _, r := range rs {
			t.Calls++
			t.InputTokens += r.InputTokens
			t.OutputTokens += r.OutputTokens
			t.CacheCreationInputTokens += r.CacheCreationInputTokens
			t.CacheReadInputTokens += r.CacheReadInputTokens
			t.CostUSD += r.CostUSD
			t.DurationMS += r.DurationMS
		}
		return t
	}

	cases := []struct {
		name string
		got  totals
		want totals
	}{
		{"overall", s.Overall, sum(records...)},
		{"agent planner", *s.ByAgent["planner"], sum(records[0])},
		{"agent worker", *s.ByAgent["worker"], sum(records[1], records[2])},
		{"task 001_a.md", *s.ByTask["001_a.md"], sum(records[0], records[1])},
		{"task 002_b.md", *s.ByTask["002_b.md"], sum(records[2])},
	}
	for _, c := range cases {
		if c.got.Calls != c.want.Calls || c.got.InputTokens != c.want.InputTokens ||
			c.got.OutputTokens != c.want.OutputTokens ||
			c.got.CacheCreationInputTokens != c.want.CacheCreationInputTokens ||
			c.got.CacheReadInputTokens != c.want.CacheReadInputTokens ||
			c.got.DurationMS != c.want.DurationMS {
			t.Errorf("%s: totals = %+v, want %+v", c.name, c.got, c.want)
		}
		if diff := c.got.CostUSD - c.want.CostUSD; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s: CostUSD = %v, want %v", c.name, c.got.CostUSD, c.want.CostUSD)
		}
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

func TestLoadCostsMissingFile(t *testing.T) {
	c, err := LoadCosts(filepath.Join(t.TempDir(), "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 0 || len(c.ByTask) != 0 {
		t.Errorf("got %+v, want empty", c)
	}
}

func TestLoadCostsSumsPerTaskAndSkipsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	content := `{"task":"001_a.md","agent":"planner","cost_usd":0.5}
{"task":"001_a.md","agent":"worker","cost_usd":0.25}
not json at all
{"task":"002_b.md","agent":"planner","cost_usd":1}
{"task":"","agent":"preprocessor","cost_usd":0.125}
{"task":"003_c.md","cost_u`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCosts(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 1.875 {
		t.Errorf("Total = %v, want 1.875", c.Total)
	}
	if c.ByTask["001_a.md"] != 0.75 || c.ByTask["002_b.md"] != 1 || len(c.ByTask) != 2 {
		t.Errorf("ByTask = %v", c.ByTask)
	}
}

func TestSummaryCostLookups(t *testing.T) {
	w, err := Open(filepath.Join(t.TempDir(), "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, r := range []Record{
		{Task: "001_a.md", Agent: "planner", CostUSD: 0.5},
		{Task: "001_a.md", Agent: "worker", CostUSD: 0.25},
		{Task: "002_b.md", Agent: "planner", CostUSD: 1},
	} {
		if err := w.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.Summary.CostUSD(); got != 1.75 {
		t.Errorf("CostUSD() = %v, want 1.75", got)
	}
	if got := w.Summary.TaskCostUSD("001_a.md"); got != 0.75 {
		t.Errorf("TaskCostUSD(001) = %v, want 0.75", got)
	}
	if got := w.Summary.TaskCostUSD("999_x.md"); got != 0 {
		t.Errorf("TaskCostUSD(unknown) = %v, want 0", got)
	}
}
