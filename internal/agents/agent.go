// Package agents loads entelecheia agent definitions (.claude/agents/*.md)
// and runs them as independent "claude --dangerously-skip-permissions"
// subprocesses. Editing an agent's model or prompt only touches these
// markdown files, never the entelecheia binary.
package agents

import (
	"os"
	"strings"
)

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
	content := string(data)
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
			return a, nil
		}
	}

	a.SystemPrompt = strings.TrimSpace(content)
	return a, nil
}
