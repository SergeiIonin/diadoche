package agents

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// Runner launches one independent agent turn and returns its combined output.
type Runner interface {
	Run(ctx context.Context, workDir string, a *Agent, prompt string) (string, error)
}

// CLIRunner shells out to the claude CLI, one independent process per call:
//
//	claude --dangerously-skip-permissions --model <model> \
//	    --append-system-prompt <role prompt> -p <task prompt>
type CLIRunner struct {
	Bin string // defaults to "claude"
}

func (r CLIRunner) Run(ctx context.Context, workDir string, a *Agent, prompt string) (string, error) {
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
	args = append(args, "-p", prompt)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = workDir

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("agent %s: %w: %s", a.Name, err, tail(out.String(), 2000))
	}
	return out.String(), nil
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
// without spending real API calls.
type DryRunRunner struct {
	Log *log.Logger
}

func (r DryRunRunner) Run(_ context.Context, workDir string, a *Agent, prompt string) (string, error) {
	logger := r.Log
	if logger == nil {
		logger = log.New(os.Stdout, "", log.LstdFlags)
	}
	logger.Printf("[dry-run] agent=%s model=%s dir=%s\n----- prompt -----\n%s\n------------------", a.Name, a.Model, workDir, prompt)
	return "", nil
}
