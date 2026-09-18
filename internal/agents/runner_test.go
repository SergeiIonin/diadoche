package agents

import (
	"context"
	"testing"
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
