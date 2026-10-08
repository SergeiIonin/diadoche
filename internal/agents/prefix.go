package agents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrefixFileName is the name of the optional per-project override of the
// shared prompt prefix, read from <project>/.claude/diadoche/prefix.md.
//
// It deliberately does not live in .claude/agents/: Claude Code reads that
// directory as subagent definitions, and the prefix is not an agent (it has
// no frontmatter and no role).
const PrefixFileName = "prefix.md"

// Prompt is one agent turn split the way the claude CLI is invoked: System
// goes to --append-system-prompt, User to -p.
//
// The split exists for prompt caching. The whole system prompt (Claude
// Code's own preset plus whatever --append-system-prompt adds) is sent to
// the API as a single block, so it is cached only when it matches the
// previous call byte for byte, in full. Keeping System identical across
// planner/worker/tester and pushing everything role- or task-specific into
// User is therefore what makes consecutive turns of one run read the cached
// prefix instead of paying for it again. See docs/prompt-caching.md.
type Prompt struct {
	System string
	User   string
}

// Full is the assembled prompt as one text: the shared prefix followed by
// the role instructions and this turn's data. Only used for logging and
// tests — the real call keeps the two halves apart.
func (p Prompt) Full() string {
	if p.System == "" {
		return p.User
	}
	return p.System + "\n\n" + p.User
}

// Assemble splits one agent turn into its cacheable and its per-call half:
// a.Prefix (identical for every agent of the run) becomes System, while the
// agent's role instructions and the task data become User, in that order.
//
// It is a pure function of its arguments: nothing time-, iteration- or
// task-dependent may ever reach System, or the cache breaks on every call.
func Assemble(a *Agent, task string) Prompt {
	p := Prompt{System: a.Prefix}
	switch {
	case a.SystemPrompt == "":
		p.User = task
	case task == "":
		p.User = a.SystemPrompt
	default:
		p.User = a.SystemPrompt + "\n\n" + task
	}
	return p
}

// DefaultPrefixFile returns the raw bytes of the built-in shared prompt
// prefix, for scaffolding a project's .claude/diadoche/prefix.md via
// `--initialize`.
func DefaultPrefixFile() ([]byte, error) {
	return defaultsFS.ReadFile("defaults/" + PrefixFileName)
}

// DefaultPrefix is the built-in shared prompt prefix every agent of a run
// gets when the project doesn't override it.
func DefaultPrefix() string {
	data, err := DefaultPrefixFile()
	if err != nil {
		// Unreachable in a correctly built binary: the file is embedded.
		return ""
	}
	return strings.TrimSpace(string(data))
}

// LoadPrefixOrDefault reads <root>/prefix.md (root being
// <project>/.claude/diadoche) and falls back to the built-in prefix when
// that file isn't there. source describes where the text came from, for the
// startup log.
//
// An override file that exists but is blank is a startup error rather than
// "no prefix": sending every agent an empty prefix would silently throw
// away both the shared instructions and the prompt cache.
func LoadPrefixOrDefault(root string) (prefix, source string, err error) {
	path := filepath.Join(root, PrefixFileName)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		text := strings.TrimSpace(string(data))
		if text == "" {
			return "", "", fmt.Errorf("%s is empty; delete it to use the built-in prompt prefix", path)
		}
		return text, path, nil
	case errors.Is(err, os.ErrNotExist):
		return DefaultPrefix(), "built-in default", nil
	default:
		return "", "", err
	}
}

// BuildPrefix assembles the run's shared prompt prefix: the base text plus,
// when the run has skills in scope, one note naming them.
//
// The skills note belongs here, not in the per-task prompts: it is the same
// for every call of a run, so keeping it in the cached half costs nothing
// and keeps the per-call prompts smaller. Given the same arguments it
// always returns the exact same bytes.
func BuildPrefix(base string, skills []string) string {
	base = strings.TrimSpace(base)
	if len(skills) == 0 {
		return base
	}
	note := fmt.Sprintf("Relevant skills available under .claude/skills: %s. Use them where applicable.", strings.Join(skills, ", "))
	if base == "" {
		return note
	}
	return base + "\n\n" + note
}
