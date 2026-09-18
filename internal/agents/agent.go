// Package agents loads diadoche agent definitions (.claude/agents/*.md)
// and runs them as independent "claude --dangerously-skip-permissions"
// subprocesses. Editing an agent's model or prompt only touches these
// markdown files, never the diadoche binary.
package agents

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultsFS holds the built-in agent prompts, embedded into the diadoche
// binary so the tool works out of the box in any project without first
// copying .claude/agents/*.md into it. A project's own .claude/agents/*.md,
// when present, always takes precedence — that's how prompts are customized
// without rebuilding the binary.
//
//go:embed defaults/*.md
var defaultsFS embed.FS

// DefaultNames lists every agent diadoche ships a built-in default prompt
// for, in the order `--initialize` should scaffold them.
var DefaultNames = []string{"planner", "worker", "tester", "preprocessor"}

// DefaultPromptFile returns the raw bytes of the built-in default agent
// definition file for name (frontmatter included), for scaffolding a
// project's .claude/agents/<name>.md via `--initialize`.
func DefaultPromptFile(name string) ([]byte, error) {
	return defaultsFS.ReadFile("defaults/" + name + ".md")
}

// Agent is one role (planner/worker/tester) as described by a markdown file
// with YAML-ish frontmatter:
//
//	---
//	name: planner
//	model: claude-fable-5-1
//	description: ...
//	---
//	<system prompt body>
type Agent struct {
	Name         string
	Model        string
	Description  string
	SystemPrompt string
}

// Load reads and parses an agent definition file.
func Load(path string) (*Agent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(string(data)), nil
}

// LoadOrDefault loads <agentsDir>/<name>.md if present, and otherwise falls
// back to the built-in default prompt for <name> embedded in the binary.
func LoadOrDefault(agentsDir, name string) (*Agent, error) {
	data, err := os.ReadFile(filepath.Join(agentsDir, name+".md"))
	if err == nil {
		return parse(string(data)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	data, derr := defaultsFS.ReadFile("defaults/" + name + ".md")
	if derr != nil {
		return nil, fmt.Errorf("no %q agent definition in %s and no built-in default", name, agentsDir)
	}
	return parse(string(data)), nil
}

func parse(content string) *Agent {
	a := &Agent{}

	if strings.HasPrefix(content, "---\n") {
		rest := content[len("---\n"):]
		if frontmatter, body, ok := strings.Cut(rest, "\n---"); ok {
			body = strings.TrimPrefix(body, "\n")

			for line := range strings.SplitSeq(frontmatter, "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				key = strings.TrimSpace(key)
				value = strings.TrimSpace(value)
				switch key {
				case "name":
					a.Name = value
				case "model":
					a.Model = value
				case "description":
					a.Description = value
				}
			}
			a.SystemPrompt = strings.TrimSpace(body)
			return a
		}
	}

	a.SystemPrompt = strings.TrimSpace(content)
	return a
}
