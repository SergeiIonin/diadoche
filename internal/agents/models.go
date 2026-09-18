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
var KnownModels = map[string]bool{
	"claude-opus-5":             true,
	"claude-sonnet-5":           true,
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

// ApplyModelOverride sets a.Model to override when override is non-empty
// (a resolved --<agent>-model flag or DIADOCHE_<AGENT>_MODEL env var
// value takes precedence over whatever LoadOrDefault picked up from the
// project's .claude/agents/<name>.md or the built-in default — the prompt
// itself, a.SystemPrompt, is untouched either way). It then validates the
// final model name against KnownModels, so an unrecognized model is an
// error at startup, before any `claude` process is ever launched.
func ApplyModelOverride(a *Agent, override string) error {
	if strings.TrimSpace(override) != "" {
		a.Model = override
	}
	if !KnownModels[a.Model] {
		return fmt.Errorf("agent %s: unknown model %q (known models: %s)", a.Name, a.Model, strings.Join(KnownModelNames(), ", "))
	}
	return nil
}
