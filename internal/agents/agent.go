// Package agents loads diadoche agent definitions (.claude/agents/*.md)
// and runs them as independent "claude --dangerously-skip-permissions"
// subprocesses. Editing an agent's model or prompt only touches these
// markdown files, never the diadoche binary.
//
// What goes where in a call: --append-system-prompt carries the run's
// shared prompt prefix (Agent.Prefix), identical for every role so the
// prompt cache can be reused between consecutive turns, and -p carries the
// role instructions (Agent.SystemPrompt) followed by the turn's data. See
// Assemble and docs/prompt-caching.md.
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
// for, in the order `--initialize` should scaffold them. adr-writer is the
// agent of the optional --adr step.
var DefaultNames = []string{"planner", "worker", "tester", "preprocessor", "adr-writer"}

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
	Name        string
	Model       string
	Description string

	// SystemPrompt is the role instructions: the markdown body of the
	// agent definition file. Despite the name — kept for compatibility —
	// it is not sent as a system prompt but at the start of the user turn,
	// right after Prefix; only Prefix goes to --append-system-prompt.
	SystemPrompt string

	// Prefix is the run's shared prompt prefix, byte-for-byte identical
	// for every agent of the run, sent as --append-system-prompt. It is
	// set by the orchestrator/preprocessor after loading (see
	// LoadPrefixOrDefault and BuildPrefix), never parsed from frontmatter.
	Prefix string

	// ModelSource records where Model ended up coming from, for the
	// startup log. Set by LoadOrDefault and then possibly overwritten by
	// ResolveModel when a flag or env var overrides the model.
	ModelSource ModelSource

	// modelKeyPresent says whether the parsed frontmatter had a `model:`
	// key at all, even with a blank value. It's what lets LoadOrDefault
	// tell "this project file only overrides the prompt" (fall back to the
	// built-in default model) apart from "this project file asks for an
	// empty model" (a startup error).
	modelKeyPresent bool
}

// Load reads and parses an agent definition file.
func Load(path string) (*Agent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a := parse(string(data))
	if a.modelKeyPresent {
		a.ModelSource = ModelSourceFrontmatter
	}
	return a, nil
}

// LoadOrDefault loads <agentsDir>/<name>.md if present, and otherwise falls
// back to the built-in default prompt for <name> embedded in the binary.
//
// A project file that overrides only the prompt — no `model:` key in its
// frontmatter — still gets the built-in default's model, so customizing a
// prompt never silently leaves an agent without a model.
func LoadOrDefault(agentsDir, name string) (*Agent, error) {
	data, err := os.ReadFile(filepath.Join(agentsDir, name+".md"))
	if err == nil {
		a := parse(string(data))
		if a.modelKeyPresent {
			a.ModelSource = ModelSourceFrontmatter
			return a, nil
		}
		a.ModelSource = ModelSourceDefault
		if def, derr := loadDefault(name); derr == nil {
			a.Model = def.Model
		}
		// A missing built-in default leaves Model empty; ResolveModel then
		// reports it as an "empty model" error at startup.
		return a, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	a, derr := loadDefault(name)
	if derr != nil {
		return nil, fmt.Errorf("no %q agent definition in %s and no built-in default", name, agentsDir)
	}
	return a, nil
}

// loadDefault parses the agent definition embedded in the binary.
func loadDefault(name string) (*Agent, error) {
	data, err := defaultsFS.ReadFile("defaults/" + name + ".md")
	if err != nil {
		return nil, err
	}
	a := parse(string(data))
	a.ModelSource = ModelSourceDefault
	return a, nil
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
					a.modelKeyPresent = true
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
