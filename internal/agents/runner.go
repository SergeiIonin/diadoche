package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Usage is the token accounting for one agent turn, as reported by the
// claude CLI's `--output-format json` result.
type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// RunResult is everything diadoche learns from one agent turn: its output,
// plus (when available) how much it cost. Parsed is false whenever the
// underlying claude invocation's JSON usage/cost fields couldn't be read
// (e.g. --dry-run, a malformed response, or a hard failure before any JSON
// was produced) — callers must check it before trusting the token/cost
// fields, but DurationMS is always the real measured wall-clock time.
type RunResult struct {
	Output       string
	Usage        Usage
	TotalCostUSD float64
	DurationMS   int64
	Parsed       bool
}

// Runner launches one independent agent turn and returns its combined
// output plus, when parseable, its resource usage.
type Runner interface {
	Run(ctx context.Context, workDir string, a *Agent, prompt string) (RunResult, error)
}

// CLIRunner shells out to the claude CLI, one independent process per call:
//
//	claude --dangerously-skip-permissions --model <model> \
//	    --append-system-prompt <role prompt> --output-format json -p <task prompt>
//
// --output-format json is what makes usage/cost accounting (RunResult.Usage,
// TotalCostUSD) possible; the raw stdout/stderr is still captured as-is for
// error messages and RunResult.Output.
type CLIRunner struct {
	Bin string // defaults to "claude"
}

func (r CLIRunner) Run(ctx context.Context, workDir string, a *Agent, prompt string) (RunResult, error) {
	bin := r.Bin
	if bin == "" {
		bin = "claude"
	}

	args := []string{"--dangerously-skip-permissions"}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", a.SystemPrompt)
	}
	args = append(args, "--output-format", "json", "-p", prompt)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	combined := stdout.String() + stderr.String()
	result := RunResult{
		Output:     combined,
		DurationMS: duration.Milliseconds(),
	}
	if usage, cost, ok := parseUsageJSON(stdout.Bytes()); ok {
		result.Usage = usage
		result.TotalCostUSD = cost
		result.Parsed = true
	}

	if runErr != nil {
		return result, fmt.Errorf("agent %s: %w: %s", a.Name, runErr, tail(combined, 2000))
	}
	return result, nil
}

// claudeJSONResult is the subset of the claude CLI's `--output-format json`
// result object diadoche cares about for usage accounting.
type claudeJSONResult struct {
	TotalCostUSD *float64 `json:"total_cost_usd"`
	Usage        *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// parseUsageJSON extracts usage/cost from a claude CLI JSON result. It
// returns ok=false (never an error) whenever the output isn't parseable
// JSON or is missing the fields diadoche needs — usage accounting is
// best-effort and must never be the reason an agent turn is treated as
// having failed.
func parseUsageJSON(stdout []byte) (Usage, float64, bool) {
	var result claudeJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &result); err != nil {
		return Usage{}, 0, false
	}
	if result.Usage == nil || result.TotalCostUSD == nil {
		return Usage{}, 0, false
	}
	return Usage{
		InputTokens:              result.Usage.InputTokens,
		OutputTokens:             result.Usage.OutputTokens,
		CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
	}, *result.TotalCostUSD, true
}

// tail returns at most the last n bytes of s, so a failed agent's error
// message (rate limit, auth, etc.) actually ends up in the logs instead of
// being lost behind a bare "exit status 1".
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// DryRunRunner logs what would be sent to the agent instead of invoking
// claude. Used for --dry-run and for exercising the orchestration loop
// without spending real API calls. It never has real usage to report:
// RunResult.Parsed is always false.
type DryRunRunner struct {
	Log *log.Logger
}

func (r DryRunRunner) Run(_ context.Context, workDir string, a *Agent, prompt string) (RunResult, error) {
	logger := r.Log
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}
	logger.Printf("[dry-run] agent=%s model=%s dir=%s\n----- prompt -----\n%s\n------------------", a.Name, a.Model, workDir, prompt)
	return RunResult{}, nil
}
