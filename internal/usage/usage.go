// Package usage records how much each agent invocation costs (tokens,
// USD, wall-clock time) to <project>/.claude/diadoche/usage.jsonl, one JSON
// line per call, and keeps a running summary for the end-of-run report.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Record is one line of usage.jsonl: everything diadoche knows about a
// single claude invocation's resource consumption. Parsed is false when the
// claude CLI's JSON output couldn't be read, in which case the numeric
// fields are zero rather than missing data.
type Record struct {
	Time                     time.Time `json:"time"`
	Task                     string    `json:"task"`
	Agent                    string    `json:"agent"`
	Iteration                int       `json:"iteration"`
	Model                    string    `json:"model"`
	InputTokens              int       `json:"input_tokens"`
	OutputTokens             int       `json:"output_tokens"`
	CacheCreationInputTokens int       `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int       `json:"cache_read_input_tokens"`
	CostUSD                  float64   `json:"cost_usd"`
	DurationMS               int64     `json:"duration_ms"`
	ExitCode                 int       `json:"exit_code"`
	Parsed                   bool      `json:"parsed"`
}

// Writer appends Records to a usage.jsonl file, one JSON object per line,
// and accumulates them into Summary for the end-of-run report.
type Writer struct {
	mu      sync.Mutex
	f       *os.File
	Summary *Summary
}

// Open creates path's parent directory if needed and opens it for
// appending (creating it if it doesn't exist yet).
func Open(path string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Writer{f: f, Summary: NewSummary()}, nil
}

// Append writes rec as one JSON line and folds it into w.Summary.
func (w *Writer) Append(rec Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := w.f.Write(data); err != nil {
		return err
	}
	w.Summary.add(rec)
	return nil
}

func (w *Writer) Close() error {
	return w.f.Close()
}

// totals is one row of aggregated usage, whether overall, per-agent or
// per-task.
type totals struct {
	Calls                    int
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	CostUSD                  float64
	DurationMS               int64
}

// allInputTokens is every token that entered the model: fresh input plus
// both kinds of prompt-cache traffic (output tokens excluded).
func (t totals) allInputTokens() int {
	return t.InputTokens + t.CacheCreationInputTokens + t.CacheReadInputTokens
}

// cacheReadShare is the fraction of all input tokens that were served from
// the prompt cache — the single number that says whether caching is
// working. It is 0 when no input tokens were recorded at all.
func (t totals) cacheReadShare() float64 {
	all := t.allInputTokens()
	if all == 0 {
		return 0
	}
	return float64(t.CacheReadInputTokens) / float64(all)
}

// Summary aggregates Records overall and broken down by agent and by task.
type Summary struct {
	mu      sync.Mutex
	Overall totals
	ByAgent map[string]*totals
	ByTask  map[string]*totals
}

func NewSummary() *Summary {
	return &Summary{ByAgent: map[string]*totals{}, ByTask: map[string]*totals{}}
}

func (s *Summary) add(rec Record) {
	s.mu.Lock()
	defer s.mu.Unlock()

	addTo := func(t *totals) {
		t.Calls++
		t.InputTokens += rec.InputTokens
		t.OutputTokens += rec.OutputTokens
		t.CacheCreationInputTokens += rec.CacheCreationInputTokens
		t.CacheReadInputTokens += rec.CacheReadInputTokens
		t.CostUSD += rec.CostUSD
		t.DurationMS += rec.DurationMS
	}
	addTo(&s.Overall)

	if s.ByAgent[rec.Agent] == nil {
		s.ByAgent[rec.Agent] = &totals{}
	}
	addTo(s.ByAgent[rec.Agent])

	if rec.Task != "" {
		if s.ByTask[rec.Task] == nil {
			s.ByTask[rec.Task] = &totals{}
		}
		addTo(s.ByTask[rec.Task])
	}
}

// Calls reports how many Records have been added, for callers deciding
// whether a summary is worth printing at all.
func (s *Summary) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Overall.Calls
}

// Format renders a human-readable end-of-run usage report.
func (s *Summary) Format() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	b.WriteString("=== usage summary ===\n")
	fmt.Fprintf(&b, "total: %d call(s), %d input / %d output tokens (+%d cache-write / %d cache-read), $%.4f, %s\n",
		s.Overall.Calls, s.Overall.InputTokens, s.Overall.OutputTokens,
		s.Overall.CacheCreationInputTokens, s.Overall.CacheReadInputTokens,
		s.Overall.CostUSD, time.Duration(s.Overall.DurationMS)*time.Millisecond)

	fmt.Fprintf(&b, "cache-read share of all input tokens: %.1f%% (%d of %d)\n",
		s.Overall.cacheReadShare()*100, s.Overall.CacheReadInputTokens, s.Overall.allInputTokens())

	b.WriteString("by agent:\n")
	for _, name := range sortedKeys(s.ByAgent) {
		t := s.ByAgent[name]
		fmt.Fprintf(&b, "  %-14s %3d call(s)  %8d in  %8d out  $%.4f  cache-read %5.1f%%\n",
			name, t.Calls, t.InputTokens, t.OutputTokens, t.CostUSD, t.cacheReadShare()*100)
	}

	b.WriteString("by task:\n")
	for _, name := range sortedKeys(s.ByTask) {
		t := s.ByTask[name]
		fmt.Fprintf(&b, "  %-30s %3d call(s)  %8d in  %8d out  $%.4f\n",
			name, t.Calls, t.InputTokens, t.OutputTokens, t.CostUSD)
	}

	return b.String()
}

func sortedKeys(m map[string]*totals) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
