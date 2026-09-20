// Package preprocessor implements diadoche's --preprocess mode: split a
// "raw", possibly multi-part user task file into atomic task spec files
// under .claude/diadoche/spec. It is deliberately independent from the
// main orchestrator loop — it only writes new spec files and exits; it never
// starts work on the tasks it creates, to avoid racing the main loop.
package preprocessor

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

	agent, err := agents.LoadOrDefault(layout.AgentsDir, "preprocessor")
	if err != nil {
		return fmt.Errorf("load preprocessor agent: %w", err)
	}
	if err := agents.ApplyModelOverride(agent, cfg.PreprocessorModel); err != nil {
		return err
	}
	logger.Printf("model: preprocessor=%s", agent.Model)

	var runner agents.Runner
	if cfg.DryRun {
		runner = agents.DryRunRunner{Log: logger}
	} else {
		runner = agents.CLIRunner{Bin: cfg.ClaudeBin, MaxCallDuration: time.Duration(cfg.MaxCallMinutes) * time.Minute}
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
	result, runErr := runner.Run(ctx, cfg.Dir, agent, prompt)
	recordUsage(logger, layout, cfg.DryRun, taskPath, agent.Model, result)
	if runErr != nil {
		return fmt.Errorf("preprocessor agent: %w", runErr)
	}
	if cfg.MaxCallCostUSD > 0 && result.Parsed && result.TotalCostUSD > cfg.MaxCallCostUSD {
		return fmt.Errorf("preprocessor call cost $%.4f, exceeding --max-call-cost=$%.4f", result.TotalCostUSD, cfg.MaxCallCostUSD)
	}
	logger.Printf("preprocessing done; task work is NOT started — run diadoche normally to pick up the new specs")
	return nil
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

func buildPrompt(taskPath, rawContent, specDir string, existing []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Raw task file (%s):\n%s\n\n", taskPath, rawContent)
	if len(existing) > 0 {
		fmt.Fprintf(&b, "Existing task spec files already in %s (do not reuse or collide with their numbers):\n", specDir)
		for _, f := range existing {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		b.WriteString("\n")
	} else {
		fmt.Fprintf(&b, "There are no existing task spec files in %s yet; start numbering from 001.\n\n", specDir)
	}
	fmt.Fprintf(&b, "Split this raw task into one or more atomic task specs and write each as its own file "+
		"directly in %s (not in spec/raw), following the spec/NNN_<slug>.md naming convention.", specDir)
	return b.String()
}
