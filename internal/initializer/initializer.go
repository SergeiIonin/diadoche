// Package initializer implements `diadoche --initialize <project-path>`:
// scaffolding the .claude/ layout diadoche needs in a target project, so a
// user can go straight to writing task specs instead of hand-creating
// directories. It does nothing else — no queue sync, no agent invocation —
// and never overwrites a file that already exists, so it's safe to run
// again later (e.g. after upgrading diadoche, to pick up new default agent
// files) without clobbering a project's customized prompts.
package initializer

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"diadoche/internal/agents"
	"diadoche/internal/specs"
)

// Run scaffolds projectDir/.claude: agent prompt files and the shared
// prompt prefix (copied from the built-in defaults, so they can be edited
// in place), the diadoche spec directory (with an empty STATUS.md) plus its
// raw/ subfolder for --preprocess input, and an empty skills directory.
func Run(projectDir string, logger *log.Logger) error {
	layout := specs.NewLayout(projectDir)

	if err := layout.Ensure(); err != nil {
		return fmt.Errorf("create spec directory: %w", err)
	}
	logger.Printf("ensured %s", layout.SpecDir)

	rawDir := filepath.Join(layout.SpecDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return fmt.Errorf("create raw task directory: %w", err)
	}
	logger.Printf("ensured %s", rawDir)

	if err := os.MkdirAll(layout.SkillsDir, 0o755); err != nil {
		return fmt.Errorf("create skills directory: %w", err)
	}
	logger.Printf("ensured %s", layout.SkillsDir)

	if err := os.MkdirAll(layout.AgentsDir, 0o755); err != nil {
		return fmt.Errorf("create agents directory: %w", err)
	}
	for _, name := range agents.DefaultNames {
		dst := filepath.Join(layout.AgentsDir, name+".md")
		if _, err := os.Stat(dst); err == nil {
			logger.Printf("skipped %s (already exists)", dst)
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat %s: %w", dst, err)
		}

		content, err := agents.DefaultPromptFile(name)
		if err != nil {
			return fmt.Errorf("load built-in %s agent: %w", name, err)
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		logger.Printf("wrote %s", dst)
	}

	// The shared prompt prefix all agents get as their system prompt,
	// copied out so a project can customize it the same way as the agent
	// prompts (see agents.LoadPrefixOrDefault).
	if _, err := os.Stat(layout.PrefixPath); err == nil {
		logger.Printf("skipped %s (already exists)", layout.PrefixPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", layout.PrefixPath, err)
	} else {
		content, err := agents.DefaultPrefixFile()
		if err != nil {
			return fmt.Errorf("load built-in prompt prefix: %w", err)
		}
		if err := os.WriteFile(layout.PrefixPath, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", layout.PrefixPath, err)
		}
		logger.Printf("wrote %s", layout.PrefixPath)
	}

	return nil
}
