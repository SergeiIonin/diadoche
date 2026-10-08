// Package onfail names diadoche's --on-fail policies: what happens to a
// task whose --ITER iterations ran out without a DONE verdict. The policies
// form an ordered chain, applied one after another until one of them ends
// the task (DONE, BLOCKED, or split into smaller tasks); a chain that runs out leaves the task FAILED,
// as an empty chain always does.
//
// The package only knows the names and parses the flag, so config and the
// orchestrator share them without importing each other. Adding a policy
// means adding its name to Names and registering its implementation in the
// orchestrator's failPolicies (internal/orchestrator/onfail.go); no other
// call site changes.
package onfail

import (
	"fmt"
	"slices"
	"strings"
)

const (
	// Escalate runs one more complete planner -> worker -> tester cycle,
	// with planner and worker on the escalation model.
	Escalate = "escalate"
	// Split calls the preprocessor agent with the task's spec and every
	// review of it to cut the task into 2–4 smaller tasks, queued in its
	// place; the original becomes BLOCKED. A call that fails or yields
	// fewer than two new specs hands the task on to the next policy.
	Split = "split"
	// Block marks the task BLOCKED, keeps its changes and blocks every task
	// that declares a dependency on it. Terminal: nothing may follow it.
	Block = "block"
)

// Names are the accepted policy names, in the order shown in errors and
// help.
var Names = []string{Escalate, Split, Block}

// DefaultChain is --on-fail's default value.
const DefaultChain = Escalate + "," + Block

// DefaultEscalationModel is --escalation-model's default.
const DefaultEscalationModel = "claude-fable-5-1"

// IsTerminal reports whether policy name always ends the task, so no policy
// may follow it in a chain.
func IsTerminal(name string) bool { return name == Block }

// Parse parses an --on-fail value: a comma-separated, ordered list of
// policy names. An empty (or blank) value is the empty chain, nil: the task
// goes FAILED and is parked, as before policies existed. An empty item, an
// unknown name, a name given twice or any policy after a terminal one is an
// error.
func Parse(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var chain []string
	for item := range strings.SplitSeq(s, ",") {
		name := strings.TrimSpace(item)
		switch {
		case name == "":
			return nil, fmt.Errorf("--on-fail: empty policy name in %q (accepted: %s)", s, strings.Join(Names, ", "))
		case !slices.Contains(Names, name):
			return nil, fmt.Errorf("--on-fail: unknown policy %q (accepted: %s)", name, strings.Join(Names, ", "))
		case slices.Contains(chain, name):
			return nil, fmt.Errorf("--on-fail: policy %q given more than once", name)
		case len(chain) > 0 && IsTerminal(chain[len(chain)-1]):
			return nil, fmt.Errorf("--on-fail: %q after %q: %s ends the task, so it must be the last policy", name, chain[len(chain)-1], chain[len(chain)-1])
		}
		chain = append(chain, name)
	}
	return chain, nil
}
