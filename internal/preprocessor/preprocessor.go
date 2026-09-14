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
	"strings"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/speclock"
	"diadoche/internal/specs"
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

	var runner agents.Runner
	if cfg.DryRun {
		runner = agents.DryRunRunner{Log: logger}
	} else {
		runner = agents.CLIRunner{Bin: cfg.ClaudeBin}
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
	if _, err := runner.Run(ctx, cfg.Dir, agent, prompt); err != nil {
		return fmt.Errorf("preprocessor agent: %w", err)
	}
	logger.Printf("preprocessing done; task work is NOT started — run diadoche normally to pick up the new specs")
	return nil
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
