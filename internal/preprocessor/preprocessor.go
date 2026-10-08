// Package preprocessor implements diadoche's --preprocess mode: split a
// "raw", possibly multi-part user task file into atomic task spec files
// under .claude/diadoche/spec. It is deliberately independent from the
// main orchestrator loop — it only writes new spec files and exits; it never
// starts work on the tasks it creates, to avoid racing the main loop.
//
// The same agent serves --on-fail split: the orchestrator loads it with
// LoadAgent and makes that call itself, inside its own loop, with the
// prompt SplitPrompt builds — a failing task's spec and every review of it,
// to be cut into smaller tasks.
package preprocessor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/speclock"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
)

// lockTimeout bounds how long --preprocess waits for the spec directory
// lock (e.g. held by the main loop's own sync) before giving up.
const lockTimeout = 2 * time.Minute

// Run reads cfg.TaskPath(), asks the preprocessor agent to split it into
// atomic spec/NNN_<slug>.md files, and returns once that single agent turn
// finishes. It does not touch STATUS.md/QUEUE.md and does not run planner,
// worker or tester.
func Run(ctx context.Context, cfg config.Config) error {
	logger := log.New(os.Stdout, "", log.LstdFlags)
	layout := specs.NewLayout(cfg.Dir)
	if err := layout.Ensure(); err != nil {
		return fmt.Errorf("ensure layout: %w", err)
	}

	taskPath := cfg.TaskPath()
	rawContent, err := os.ReadFile(taskPath)
	if err != nil {
		return fmt.Errorf("read raw task file: %w", err)
	}

	agent, err := LoadAgent(layout, cfg.PreprocessorModel)
	if err != nil {
		return err
	}
	logger.Printf("model: preprocessor=%s", agent.ModelDescription())

	// The same shared prompt prefix the main loop's agents get, so a
	// --preprocess run and a following regular run can share the cached
	// system prompt. No skills note: --preprocess doesn't resolve skills
	// (it is its own run and never implements anything), and the prefix
	// must stay identical across calls, not vary per invocation.
	basePrefix, prefixSource, err := agents.LoadPrefixOrDefault(layout.Root)
	if err != nil {
		return fmt.Errorf("load prompt prefix: %w", err)
	}
	agent.Prefix = agents.BuildPrefix(basePrefix, nil)
	logger.Printf("prompt prefix: %d bytes (source: %s)", len(agent.Prefix), prefixSource)

	var runner agents.Runner
	if cfg.DryRun {
		runner = agents.DryRunRunner{Log: logger}
	} else {
		runner = agents.CLIRunner{
			Bin:                                cfg.ClaudeBin,
			MaxCallDuration:                    time.Duration(cfg.MaxCallMinutes) * time.Minute,
			ExcludeDynamicSystemPromptSections: cfg.StableSystemPrompt,
		}
	}

	// Held for the whole agent turn: the agent may write several files over
	// several tool calls, and the main loop's Sync must never observe a
	// half-written spec file.
	lock, err := speclock.Acquire(layout.SpecDir, lockTimeout)
	if err != nil {
		return fmt.Errorf("acquire spec lock: %w", err)
	}
	defer lock.Release()

	existing, err := specs.DiscoverTaskFiles(layout.SpecDir)
	if err != nil {
		return fmt.Errorf("list existing task specs: %w", err)
	}

	prompt := buildPrompt(taskPath, string(rawContent), layout.SpecDir, existing)

	logger.Printf("=== preprocessing %s ===", taskPath)
	logger.Printf("[preprocessor] started (model=%s)", agent.Model)
	result, runErr := runner.Run(ctx, cfg.Dir, agent, prompt)
	recordUsage(logger, layout, cfg.DryRun, taskPath, agent.Model, result)
	outcome := "finished"
	if runErr != nil {
		outcome = "failed"
	}
	logger.Printf("[preprocessor] %s — %s", outcome, result.Summary())
	if runErr != nil {
		return fmt.Errorf("preprocessor agent: %w", runErr)
	}
	if cfg.MaxCallCostUSD > 0 && result.Parsed && result.TotalCostUSD > cfg.MaxCallCostUSD {
		return fmt.Errorf("preprocessor call cost $%.4f, exceeding --max-call-cost=$%.4f", result.TotalCostUSD, cfg.MaxCallCostUSD)
	}
	logger.Printf("preprocessing done; task work is NOT started — run diadoche normally to pick up the new specs")
	return nil
}

// LoadAgent loads the preprocessor agent (the project's own, or the
// built-in default) on its model, overridden by override when set.
func LoadAgent(layout specs.Layout, override agents.ModelOverride) (*agents.Agent, error) {
	agent, err := agents.LoadOrDefault(layout.AgentsDir, "preprocessor")
	if err != nil {
		return nil, fmt.Errorf("load preprocessor agent: %w", err)
	}
	if err := agents.ResolveModel(agent, override); err != nil {
		return nil, err
	}
	return agent, nil
}

// recordUsage appends one usage.jsonl line for the preprocessor's single
// agent call, so its cost shows up alongside planner/worker/tester's in the
// same log — best-effort and never fatal, same as the main loop's
// accounting (see orchestrator.recordUsage).
func recordUsage(logger *log.Logger, layout specs.Layout, dryRun bool, taskPath, model string, result agents.RunResult) {
	if dryRun {
		return
	}
	w, err := usage.Open(filepath.Join(layout.Root, "usage.jsonl"))
	if err != nil {
		logger.Printf("failed to open usage log: %v", err)
		return
	}
	defer w.Close()

	rec := usage.Record{
		Time:                     time.Now(),
		Task:                     taskPath,
		Agent:                    "preprocessor",
		Model:                    model,
		InputTokens:              result.Usage.InputTokens,
		OutputTokens:             result.Usage.OutputTokens,
		CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
		CostUSD:                  result.TotalCostUSD,
		DurationMS:               result.DurationMS,
		ExitCode:                 result.ExitCode,
		Parsed:                   result.Parsed,
	}
	if err := w.Append(rec); err != nil {
		logger.Printf("failed to write usage record: %v", err)
		return
	}
	if !result.Parsed {
		logger.Printf("could not parse usage/cost from preprocessor's claude output; recorded a zeroed usage line")
	}
}

// dependsOnRule is the sentence of both prompts that makes every task spec
// the preprocessor writes declare its dependencies explicitly.
const dependsOnRule = `Every spec you write must contain, on a line of its own, "Depends-on: NNN[, NNN...]" naming the tasks it really depends on, ` +
	`or "Depends-on: none" when it depends on nothing.`

func buildPrompt(taskPath, rawContent, specDir string, existing []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Raw task file (%s):\n%s\n\n", taskPath, rawContent)
	writeExisting(&b, specDir, existing)
	fmt.Fprintf(&b, "Split this raw task into one or more atomic task specs and write each as its own file "+
		"directly in %s (not in spec/raw), following the spec/NNN_<slug>.md naming convention. %s", specDir, dependsOnRule)
	return b.String()
}

// writeExisting lists the task spec files already in specDir, whose
// numbers the new ones must not reuse.
func writeExisting(b *strings.Builder, specDir string, existing []string) {
	if len(existing) > 0 {
		fmt.Fprintf(b, "Existing task spec files already in %s (do not reuse or collide with their numbers):\n", specDir)
		for _, f := range existing {
			fmt.Fprintf(b, "- %s\n", f)
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(b, "There are no existing task spec files in %s yet; start numbering from 001.\n\n", specDir)
	}
}

// Review is one review of a task: its file name and content.
type Review struct{ Name, Content string }

// SplitRequest is what --on-fail split asks the preprocessor to cut up: a
// task that did not reach DONE in Iterations iterations, with every review
// of it (oldest first) and the task spec files already in SpecDir.
type SplitRequest struct {
	SpecFile, SpecContent, SpecDir string
	Iterations                     int
	Reviews                        []Review
	Existing                       []string
}

// SplitPrompt builds the preprocessor prompt of --on-fail split: cut the
// failing task into 2–4 smaller tasks that together cover its whole spec.
func SplitPrompt(req SplitRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Failing task spec (%s):\n%s\n\n", filepath.Join(req.SpecDir, req.SpecFile), req.SpecContent)
	if len(req.Reviews) > 0 {
		b.WriteString("Reviews of every iteration, oldest first:\n\n")
		for _, r := range req.Reviews {
			fmt.Fprintf(&b, "### %s\n%s\n\n", r.Name, strings.TrimRight(r.Content, "\n"))
		}
	} else {
		b.WriteString("The task has no reviews.\n\n")
	}
	writeExisting(&b, req.SpecDir, req.Existing)
	fmt.Fprintf(&b, "Split mode: the task %s did not reach DONE in %d iteration(s), most likely because it is too big. "+
		"Cut it into 2–4 smaller, atomic, self-contained tasks that together cover the WHOLE original spec. "+
		"Its code changes have been set aside, so the new tasks start from the code as it was before it. "+
		"Use the reviews to see where it got stuck. "+
		"Write each new task as its own file directly in %s, following the spec/NNN_<slug>.md naming convention, "+
		"numbered after the highest existing number, in the order they should run. %s "+
		"Do not modify the original spec, its plan or reviews, STATUS.md or QUEUE.md.",
		req.SpecFile, req.Iterations, req.SpecDir, dependsOnRule)
	return b.String()
}

// NewSpecs returns the files in after that are not in before, sorted.
func NewSpecs(before, after []string) []string {
	var out []string
	for _, f := range after {
		if !slices.Contains(before, f) {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}
