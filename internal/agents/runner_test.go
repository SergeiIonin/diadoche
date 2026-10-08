package agents

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"diadoche/internal/usagelimit"
)

func TestParseClaudeJSONExtractsResultUsageAndCost(t *testing.T) {
	stdout := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"hello","total_cost_usd":0.1234,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}`)
	parsed, ok := parseClaudeJSON(stdout)
	if !ok {
		t.Fatal("expected ok=true for well-formed JSON")
	}
	u := parsed.Usage
	if u.InputTokens != 100 || u.OutputTokens != 50 || u.CacheCreationInputTokens != 10 || u.CacheReadInputTokens != 5 {
		t.Fatalf("usage = %+v, unexpected", u)
	}
	if parsed.TotalCostUSD != 0.1234 {
		t.Fatalf("cost = %v, want 0.1234", parsed.TotalCostUSD)
	}
	if parsed.Result != "hello" {
		t.Fatalf("result = %q, want %q", parsed.Result, "hello")
	}
	if parsed.IsError {
		t.Error("IsError = true, want false")
	}
}

func TestParseClaudeJSONReadsIsError(t *testing.T) {
	stdout := []byte(`{"is_error":true,"result":"boom","total_cost_usd":0,"usage":{"input_tokens":1,"output_tokens":0}}`)
	parsed, ok := parseClaudeJSON(stdout)
	if !ok || !parsed.IsError || parsed.Result != "boom" {
		t.Fatalf("parsed = %+v ok=%v, want an is_error result carrying \"boom\"", parsed, ok)
	}
}

func TestParseClaudeJSONHandlesLeadingTrailingWhitespace(t *testing.T) {
	stdout := []byte("\n\n  {\"total_cost_usd\":0.5,\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}  \n")
	parsed, ok := parseClaudeJSON(stdout)
	if !ok || parsed.TotalCostUSD != 0.5 {
		t.Fatalf("ok=%v cost=%v, want ok=true cost=0.5", ok, parsed.TotalCostUSD)
	}
}

func TestParseClaudeJSONFailsOnMalformedJSON(t *testing.T) {
	if _, ok := parseClaudeJSON([]byte("not json at all")); ok {
		t.Fatal("expected ok=false for malformed JSON")
	}
}

func TestParseClaudeJSONFailsWhenFieldsMissing(t *testing.T) {
	if _, ok := parseClaudeJSON([]byte(`{"type":"result"}`)); ok {
		t.Fatal("expected ok=false when usage/total_cost_usd are absent")
	}
}

func TestParseClaudeJSONFailsOnEmptyOutput(t *testing.T) {
	if _, ok := parseClaudeJSON([]byte("")); ok {
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
	result, err := r.Run(context.Background(), t.TempDir(), a, "do the thing")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when the call exceeds MaxCallDuration")
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a process killed by the timeout", result.ExitCode)
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

// fakeClaude writes an executable shell script standing in for the claude
// CLI, with the given body, and returns its path. Tests use it to pin down
// exactly what diadoche does with a given claude JSON response — no real
// model is ever invoked.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeJSONResponse stores a claude JSON response in a file, so a fake
// claude can `cat` it verbatim instead of echoing it through the shell
// (real claude output contains apostrophes and other shell metacharacters).
func writeJSONResponse(t *testing.T, payload string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "response.json")
	if err := os.WriteFile(path, []byte(payload+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const fullClaudeJSON = `{"type":"result","subtype":"success","is_error":false,"result":"hello from agent","total_cost_usd":0.1234,"usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":10,"cache_read_input_tokens":5}}`

func TestCLIRunnerParsesFullJSONResult(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv("DIADOCHE_TEST_ARGS_FILE", argsFile)
	stub := fakeClaude(t, `printf '%s\n' "$@" > "$DIADOCHE_TEST_ARGS_FILE"
cat "`+writeJSONResponse(t, fullClaudeJSON)+`"
`)

	r := CLIRunner{Bin: stub}
	a := &Agent{Name: "worker", Model: "claude-opus-5", SystemPrompt: "be a worker"}
	result, err := r.Run(context.Background(), t.TempDir(), a, "do the thing")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !result.Parsed {
		t.Fatal("expected Parsed=true for a well-formed claude JSON result")
	}
	u := result.Usage
	if u.InputTokens != 100 || u.OutputTokens != 50 || u.CacheCreationInputTokens != 10 || u.CacheReadInputTokens != 5 {
		t.Errorf("Usage = %+v, unexpected", u)
	}
	if result.TotalCostUSD != 0.1234 {
		t.Errorf("TotalCostUSD = %v, want 0.1234", result.TotalCostUSD)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.DurationMS < 0 {
		t.Errorf("DurationMS = %d, want >= 0", result.DurationMS)
	}
	// The text the rest of diadoche works with comes from the JSON
	// "result" field, not from the raw stdout it used to read.
	if result.Output != "hello from agent" {
		t.Errorf("Output = %q, want the JSON result field %q", result.Output, "hello from agent")
	}

	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("stub did not record its args: %v", err)
	}
	gotArgs := strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n")
	if !containsPair(gotArgs, "--output-format", "json") {
		t.Errorf("claude was not invoked with --output-format json; args: %v", gotArgs)
	}
	if !containsArg(gotArgs, "-p") {
		t.Errorf("claude was not invoked with -p; args: %v", gotArgs)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsPair(args []string, first, second string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == first && args[i+1] == second {
			return true
		}
	}
	return false
}

func TestCLIRunnerRecordsNonZeroExitCodeAndKeepsRawErrorTail(t *testing.T) {
	const limitMsg = "You've hit your session limit · resets 7pm (Europe/Moscow)"
	response := writeJSONResponse(t, `{"type":"result","is_error":true,"result":"`+limitMsg+`","total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0}}`)
	stub := fakeClaude(t, `cat "`+response+`"
exit 1
`)

	r := CLIRunner{Bin: stub}
	a := &Agent{Name: "worker", Model: "claude-opus-5"}
	result, err := r.Run(context.Background(), t.TempDir(), a, "do the thing")
	if err == nil {
		t.Fatal("expected an error when claude exits non-zero")
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
	if !result.Parsed {
		t.Error("expected usage to still parse from a failed call's JSON output")
	}
	if result.Output != limitMsg {
		t.Errorf("Output = %q, want the JSON result field %q", result.Output, limitMsg)
	}
	// The returned error must keep the raw JSON line, so usage-limit
	// detection (and diadoche-restarter, scanning diadoche's log) still
	// sees the limit message on the same line as "error".
	if !usagelimit.Mentions(err.Error()) {
		t.Errorf("expected the error to still carry the raw usage-limit output; got: %v", err)
	}
}

func TestCLIRunnerUnparseableOutputFallsBackToRawOutput(t *testing.T) {
	stub := fakeClaude(t, "echo 'not json'\n")
	r := CLIRunner{Bin: stub}
	a := &Agent{Name: "worker"}
	result, err := r.Run(context.Background(), t.TempDir(), a, "do the thing")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if result.Parsed {
		t.Error("expected Parsed=false for output that isn't a claude JSON result")
	}
	if !strings.Contains(result.Output, "not json") {
		t.Errorf("Output = %q, want it to fall back to the raw output", result.Output)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
}

func TestCLIRunnerMissingBinaryReportsExitCodeMinusOne(t *testing.T) {
	r := CLIRunner{Bin: filepath.Join(t.TempDir(), "does-not-exist")}
	a := &Agent{Name: "worker"}
	result, err := r.Run(context.Background(), t.TempDir(), a, "do the thing")
	if err == nil {
		t.Fatal("expected an error when the claude binary doesn't exist")
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 when the process never started", result.ExitCode)
	}
}

func TestRunResultSummaryReportsNonZeroExitCode(t *testing.T) {
	got := RunResult{DurationMS: 1000, ExitCode: 1}.Summary()
	if !strings.Contains(got, "exit=1") {
		t.Errorf("Summary() = %q, want it to report exit=1", got)
	}
	ok := RunResult{DurationMS: 1000, Parsed: true}.Summary()
	if strings.Contains(ok, "exit=") {
		t.Errorf("Summary() = %q, want no exit code on a successful call", ok)
	}
}
