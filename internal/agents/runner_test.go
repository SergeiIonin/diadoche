package agents

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseUsageJSONExtractsUsageAndCost(t *testing.T) {
	stdout := []byte(`{"type":"result","total_cost_usd":0.1234,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}`)
	usage, cost, ok := parseUsageJSON(stdout)
	if !ok {
		t.Fatal("expected ok=true for well-formed JSON")
	}
	if usage.InputTokens != 100 || usage.OutputTokens != 50 || usage.CacheCreationInputTokens != 10 || usage.CacheReadInputTokens != 5 {
		t.Fatalf("usage = %+v, unexpected", usage)
	}
	if cost != 0.1234 {
		t.Fatalf("cost = %v, want 0.1234", cost)
	}
}

func TestParseUsageJSONHandlesLeadingTrailingWhitespace(t *testing.T) {
	stdout := []byte("\n\n  {\"total_cost_usd\":0.5,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}  \n")
	_, cost, ok := parseUsageJSON(stdout)
	if !ok || cost != 0.5 {
		t.Fatalf("ok=%v cost=%v, want ok=true cost=0.5", ok, cost)
	}
}

func TestParseUsageJSONFailsOnMalformedJSON(t *testing.T) {
	_, _, ok := parseUsageJSON([]byte("not json at all"))
	if ok {
		t.Fatal("expected ok=false for malformed JSON")
	}
}

func TestParseUsageJSONFailsWhenFieldsMissing(t *testing.T) {
	_, _, ok := parseUsageJSON([]byte(`{"type":"result"}`))
	if ok {
		t.Fatal("expected ok=false when usage/total_cost_usd are absent")
	}
}

func TestParseUsageJSONFailsOnEmptyOutput(t *testing.T) {
	_, _, ok := parseUsageJSON([]byte(""))
	if ok {
		t.Fatal("expected ok=false for empty output")
	}
}

// sleepyClaudeStub writes a shell script standing in for `claude`: it
// ignores every argument (so it works regardless of what CLIRunner passes)
// and sleeps for sleepSeconds before printing a well-formed JSON result.
func sleepyClaudeStub(t *testing.T, sleepSeconds int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude.sh")
	script := "#!/bin/sh\nsleep " + strconv.Itoa(sleepSeconds) + "\necho '{\"total_cost_usd\":0.01,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCLIRunnerKillsCallExceedingMaxCallDuration(t *testing.T) {
	stub := sleepyClaudeStub(t, 5)
	r := CLIRunner{Bin: stub, MaxCallDuration: 150 * time.Millisecond}
	a := &Agent{Name: "worker", Model: "claude-opus-5"}

	start := time.Now()
	_, err := r.Run(context.Background(), t.TempDir(), a, "do the thing")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the call exceeds MaxCallDuration")
	}
	if !strings.Contains(err.Error(), "--max-call-minutes") {
		t.Errorf("expected error to mention --max-call-minutes, got: %v", err)
	}
	if elapsed >= 4*time.Second {
		t.Fatalf("Run took %v — the stub's 5s sleep was not actually killed by the timeout", elapsed)
	}
}

func TestCLIRunnerNoTimeoutWhenMaxCallDurationUnset(t *testing.T) {
	stub := sleepyClaudeStub(t, 0)
	r := CLIRunner{Bin: stub} // MaxCallDuration zero value: no limit
	a := &Agent{Name: "worker", Model: "claude-opus-5"}

	result, err := r.Run(context.Background(), t.TempDir(), a, "do the thing")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !result.Parsed || result.TotalCostUSD != 0.01 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestDryRunRunnerReturnsUnparsedZeroResult(t *testing.T) {
	r := DryRunRunner{}
	a := &Agent{Name: "planner", Model: "claude-opus-5"}
	result, err := r.Run(context.Background(), "/tmp", a, "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	if result.Parsed {
		t.Fatal("expected DryRunRunner's result to never report Parsed=true")
	}
}

func TestRunResultSummaryReportsTokensAndCost(t *testing.T) {
	r := RunResult{
		Usage:        Usage{InputTokens: 100, OutputTokens: 50, CacheCreationInputTokens: 10, CacheReadInputTokens: 5},
		TotalCostUSD: 0.1234,
		DurationMS:   61_400,
		Parsed:       true,
	}
	got := r.Summary()
	for _, want := range []string{"took 1m1s", "tokens: 165", "in=100", "out=50", "cache_write=10", "cache_read=5", "$0.1234"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary() missing %q; got: %s", want, got)
		}
	}
}

func TestRunResultSummaryUnparsed(t *testing.T) {
	got := RunResult{DurationMS: 2000}.Summary()
	if !strings.Contains(got, "tokens: unknown") {
		t.Errorf("Summary() = %q, want it to report unknown tokens", got)
	}
}
