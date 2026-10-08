package agents

import (
	"fmt"
	"sort"
	"strings"
)

// KnownModels is the set of Claude model names diadoche accepts for an
// agent's `model:` frontmatter field and for --<agent>-model /
// DIADOCHE_<AGENT>_MODEL overrides. Update this list when a new Claude
// model generation ships — it's the single place that needs to change.
// Anything not listed here fails fast at startup instead of silently
// reaching a `claude` CLI invocation that may or may not recognize it.
// Older generations stay listed so projects whose .claude/agents/*.md
// still name them keep working after an upgrade.
var KnownModels = map[string]bool{
	"claude-opus-5":             true,
	"claude-opus-5-5":           true,
	"claude-sonnet-5":           true,
	"claude-sonnet-5-5":         true,
	"claude-fable-5-1":          true,
	"claude-haiku-4-5-20251001": true,
}

// KnownModelNames returns KnownModels' keys, sorted, for error messages.
func KnownModelNames() []string {
	names := make([]string, 0, len(KnownModels))
	for m := range KnownModels {
		names = append(names, m)
	}
	sort.Strings(names)
	return names
}

// ModelSource says where an agent's final model name came from. It exists
// so the startup log can tell "you asked for this model" apart from "this
// is just the default", which is the difference between a working
// --worker-model flag and a typo in its name.
type ModelSource string

const (
	// ModelSourceFlag is a --<agent>-model command-line flag.
	ModelSourceFlag ModelSource = "flag"
	// ModelSourceEnv is a DIADOCHE_<AGENT>_MODEL environment variable.
	ModelSourceEnv ModelSource = "env"
	// ModelSourceFrontmatter is the model: field of the project's own
	// .claude/agents/<name>.md.
	ModelSourceFrontmatter ModelSource = "frontmatter"
	// ModelSourceDefault is the model: field of the agent definition
	// embedded in the diadoche binary.
	ModelSourceDefault ModelSource = "built-in default"
)

// ModelOverride is one agent's model as requested from outside the project
// (a flag or an environment variable), carrying the source along so it can
// be logged. The zero value means "not requested": resolution then falls
// through to the agent definition's own model: field. Built by
// config.Parse, consumed by ResolveModel.
type ModelOverride struct {
	Value  string
	Source ModelSource
}

// IsSet reports whether an override was actually requested.
func (o ModelOverride) IsSet() bool { return o.Source != "" }

// ResolveModel decides an agent's final model and records where it came
// from. An override (a resolved --<agent>-model flag or
// DIADOCHE_<AGENT>_MODEL env var) wins over whatever LoadOrDefault picked
// up from the project's .claude/agents/<name>.md or the built-in default;
// the prompt itself, a.SystemPrompt, is untouched either way.
//
// The resulting name is then validated, so an empty or unrecognized model
// is an error at startup, before any `claude` process is ever launched.
func ResolveModel(a *Agent, ov ModelOverride) error {
	if ov.IsSet() {
		a.Model = ov.Value
		a.ModelSource = ov.Source
	}
	if strings.TrimSpace(a.Model) == "" {
		return fmt.Errorf("agent %s: empty model (source: %s)", a.Name, a.ModelSource)
	}
	if !KnownModels[a.Model] {
		return fmt.Errorf("agent %s: unknown model %q (source: %s; known models: %s)",
			a.Name, a.Model, a.ModelSource, strings.Join(KnownModelNames(), ", "))
	}
	return nil
}

// ModelDescription renders the agent's model and its source for logging,
// e.g. `claude-opus-5-5 (built-in default)`.
func (a *Agent) ModelDescription() string {
	return fmt.Sprintf("%s (%s)", a.Model, a.ModelSource)
}
