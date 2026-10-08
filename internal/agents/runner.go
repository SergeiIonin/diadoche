package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// fields, but DurationMS and ExitCode are always the real observed values.
type RunResult struct {
	// Output is the agent's text answer: the JSON result object's "result"
	// field when the claude output parsed and carried one. It falls back to
	// the raw stdout+stderr otherwise, so an unparseable or crashed call
	// still leaves something to debug with.
	Output string

	Usage        Usage
	TotalCostUSD float64
	DurationMS   int64

	// ExitCode is the claude process's exit status: 0 on success, >0 for
	// the CLI's own non-zero exit, and -1 when no status was ever produced
	// (the binary couldn't be started, or the process was killed by a
	// signal — e.g. the --max-call-minutes timeout).
	ExitCode int

	Parsed bool
}

// Total is every token the turn consumed: input, output and both kinds of
// prompt-cache traffic.
func (u Usage) Total() int {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// Summary renders the turn's duration, token breakdown and cost as one
// human-readable log fragment, plus the exit code when it isn't 0.
// Tokens/cost are reported as unknown when the claude JSON result couldn't
// be parsed (e.g. --dry-run).
func (r RunResult) Summary() string {
	d := (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second)
	var exit string
	if r.ExitCode != 0 {
		exit = fmt.Sprintf(", exit=%d", r.ExitCode)
	}
	if !r.Parsed {
		return fmt.Sprintf("took %s, tokens: unknown%s", d, exit)
	}
	u := r.Usage
	return fmt.Sprintf("took %s, tokens: %d (in=%d out=%d cache_write=%d cache_read=%d), cost $%.4f%s",
		d, u.Total(), u.InputTokens, u.OutputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens, r.TotalCostUSD, exit)
}

// Runner launches one independent agent turn and returns its combined
// output plus, when parseable, its resource usage.
type Runner interface {
	Run(ctx context.Context, workDir string, a *Agent, prompt string) (RunResult, error)
}

// CLIRunner shells out to the claude CLI, one independent process per call:
//
//	claude --dangerously-skip-permissions --model <model> \
//	    [--exclude-dynamic-system-prompt-sections] \
//	    --append-system-prompt <shared prefix> \
//	    --output-format json -p <role prompt + task prompt>
//
// The system prompt holds only the run's shared prefix, so it is identical
// for every role and the API's prompt cache can serve it to the next agent
// (see Assemble and docs/prompt-caching.md); everything role- or
// task-specific goes to -p.
//
// --output-format json is what makes usage/cost accounting (RunResult.Usage,
// TotalCostUSD) possible, and carries the agent's text answer in the result
// object's "result" field (RunResult.Output); the raw stdout/stderr is still
// captured as-is for error messages and as a fallback output.
//
// On Unix claude runs in its own process group, so a terminal Ctrl-C
// reaches only diadoche, which lets the call finish (see --shutdown-timeout);
// cancelling ctx kills claude's whole group with SIGKILL.
type CLIRunner struct {
	Bin string // defaults to "claude"

	// ExcludeDynamicSystemPromptSections passes claude's
	// --exclude-dynamic-system-prompt-sections, which moves per-machine
	// sections (cwd, env info, memory paths, git status) out of the system
	// prompt and into the first user message — without it those sections
	// can differ between calls and spoil the cached prefix. Set from
	// --stable-system-prompt, which exists to turn it back off for a
	// claude CLI too old to know the flag.
	ExcludeDynamicSystemPromptSections bool

	// MaxCallDuration, when positive, kills the claude process if a single
	// call runs longer than this (--max-call-minutes) — a runaway call
	// (stuck in its own long tool-call loop) is capped at a bounded cost
	// instead of running until an external limit (e.g. the account's usage
	// window) stops it. Zero/negative means no limit.
	MaxCallDuration time.Duration
}

// callWaitDelay bounds how long Run waits for a killed call's output pipes
// to close (exec.Cmd.WaitDelay).
const callWaitDelay = 5 * time.Second

func (r CLIRunner) Run(ctx context.Context, workDir string, a *Agent, prompt string) (RunResult, error) {
	bin := r.Bin
	if bin == "" {
		bin = "claude"
	}

	p := Assemble(a, prompt)

	args := []string{"--dangerously-skip-permissions"}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if r.ExcludeDynamicSystemPromptSections {
		args = append(args, "--exclude-dynamic-system-prompt-sections")
	}
	if p.System != "" {
		args = append(args, "--append-system-prompt", p.System)
	}
	args = append(args, "--output-format", "json", "-p", p.User)

	callCtx := ctx
	if r.MaxCallDuration > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, r.MaxCallDuration)
		defer cancel()
	}

	cmd := exec.CommandContext(callCtx, bin, args...)
	cmd.Dir = workDir
	configureProcessGroup(cmd)
	// A grandchild that outlives a killed claude may still hold its
	// stdout/stderr pipes open; don't let that block Wait.
	cmd.WaitDelay = callWaitDelay

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
		ExitCode:   exitCode(cmd),
	}
	if parsed, ok := parseClaudeJSON(stdout.Bytes()); ok {
		result.Usage = parsed.Usage
		result.TotalCostUSD = parsed.TotalCostUSD
		result.Parsed = true
		if parsed.Result != "" {
			result.Output = parsed.Result
		}
	}

	if runErr != nil {
		if r.MaxCallDuration > 0 && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return result, fmt.Errorf("agent %s: call killed after exceeding --max-call-minutes=%s", a.Name, r.MaxCallDuration)
		}
		// The error deliberately carries the *raw* stdout+stderr tail, not
		// the parsed result text: usage-limit detection
		// (usagelimit.Mentions here, usagelimit.IsLimitLine over diadoche's
		// log in diadoche-restarter) needs the claude JSON line with
		// "is_error":true and the limit message on one single line.
		return result, fmt.Errorf("agent %s: %w: %s", a.Name, runErr, tail(combined, 2000))
	}
	return result, nil
}

// exitCode reports the finished process's exit status, or -1 when it never
// produced one (failed to start, or killed by a signal — including the
// --max-call-minutes timeout).
func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

// claudeJSONResult is the subset of the claude CLI's `--output-format json`
// result object diadoche cares about: the agent's text answer plus its
// usage accounting.
type claudeJSONResult struct {
	Result       string   `json:"result"`
	Subtype      string   `json:"subtype"`
	IsError      bool     `json:"is_error"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	Usage        *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// claudeResult is a decoded claude CLI JSON result, in the shape diadoche
// uses it: the agent's text answer and its token/cost accounting.
type claudeResult struct {
	Usage        Usage
	TotalCostUSD float64
	Result       string
	IsError      bool
}

// parseClaudeJSON extracts the agent's text result plus usage/cost from a
// claude CLI JSON result. It returns ok=false (never an error) whenever the
// output isn't parseable JSON or is missing the usage/cost fields diadoche
// needs — usage accounting is best-effort and must never be the reason an
// agent turn is treated as having failed.
func parseClaudeJSON(stdout []byte) (claudeResult, bool) {
	var decoded claudeJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout), &decoded); err != nil {
		return claudeResult{}, false
	}
	if decoded.Usage == nil || decoded.TotalCostUSD == nil {
		return claudeResult{}, false
	}
	return claudeResult{
		Usage: Usage{
			InputTokens:              decoded.Usage.InputTokens,
			OutputTokens:             decoded.Usage.OutputTokens,
			CacheCreationInputTokens: decoded.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     decoded.Usage.CacheReadInputTokens,
		},
		TotalCostUSD: *decoded.TotalCostUSD,
		Result:       decoded.Result,
		IsError:      decoded.IsError,
	}, true
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
	p := Assemble(a, prompt)
	logger.Printf("[dry-run] agent=%s model=%s dir=%s\n"+
		"----- system prompt (shared prefix) -----\n%s\n"+
		"----- prompt -----\n%s\n------------------",
		a.Name, a.Model, workDir, p.System, p.User)
	return RunResult{}, nil
}
