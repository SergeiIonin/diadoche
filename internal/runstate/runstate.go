// Package runstate owns .claude/diadoche/run-state.json: where a diadoche
// run is, down to the agent. After every step the orchestrator records the
// current task, its iteration, which agent is up and whether that agent is
// pending, running or done, plus the task's base commit and worktree. A
// restarted run continues with the first agent that has not finished,
// instead of starting the iteration over.
//
// The file adds to spec/STATUS.md and spec/QUEUE.md, it does not replace
// them: a state that contradicts them is ignored, and without the file the
// spec/ files (plan, reviews, NNN_BASE) are still enough to resume a task
// at the right iteration.
//
// It also keeps the records of WAITING tasks (an agent answered
// NEEDS_HUMAN, see package questions) under "waiting", each exactly as it
// was before the call that asked a human, so --resume-waiting continues
// such a task at that same iteration and agent.
package runstate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"diadoche/internal/specs"
)

// FileName is the state file's name inside the diadoche root
// (<project>/.claude/diadoche).
const FileName = "run-state.json"

// Path returns the state file's path for the diadoche root root
// (specs.Layout.Root).
func Path(root string) string { return filepath.Join(root, FileName) }

// Agent states.
const (
	AgentPending = "pending"
	AgentRunning = "running"
	AgentDone    = "done"
)

// Agents are the agents of one iteration, in the order they run.
var Agents = []string{"planner", "worker", "tester"}

// Task is the record of the task in progress.
type Task struct {
	Spec       string `json:"spec"`
	Num        string `json:"num"`
	Iteration  int    `json:"iteration"`
	Agent      string `json:"agent"`
	AgentState string `json:"agent_state"`
	BaseCommit string `json:"base_commit"`

	// Worktree is the task's worktree relative to the project root, set
	// only when the task runs in its own worktree.
	Worktree string `json:"worktree,omitempty"`
}

// State is the whole file. A nil Task (JSON null) means no task is in
// progress: there is nothing to resume.
type State struct {
	RunID     string    `json:"run_id"`
	Task      *Task     `json:"task"`
	UpdatedAt time.Time `json:"updated_at"`

	// Waiting are the records of WAITING tasks, keyed by spec file, each as
	// it was right before the call that answered NEEDS_HUMAN. Omitted when
	// empty; a file written before it existed has none.
	Waiting map[string]Task `json:"waiting,omitempty"`
}

// Load reads the state file at path. A missing file is (nil, nil); invalid
// JSON or a state without a run id is an error.
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.RunID == "" {
		return nil, fmt.Errorf("parse %s: no run_id", path)
	}
	return &s, nil
}

// Save writes s to path atomically (temp file in the same directory +
// rename), so a process killed mid-write never leaves a truncated file.
func Save(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return specs.WriteFileAtomic(path, append(b, '\n'))
}

// NewRunID returns a fresh run id: the UTC start time plus 4 random bytes,
// e.g. "20261008T120000Z-1a2b3c4d".
func NewRunID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// Validate checks that t is a record diadoche could have written.
func (t Task) Validate() error {
	switch {
	case t.Spec == "":
		return errors.New("no spec")
	case t.Iteration < 1:
		return fmt.Errorf("iteration %d < 1", t.Iteration)
	case !slices.Contains(Agents, t.Agent):
		return fmt.Errorf("unknown agent %q", t.Agent)
	case t.AgentState != AgentPending && t.AgentState != AgentRunning && t.AgentState != AgentDone:
		return fmt.Errorf("unknown agent_state %q", t.AgentState)
	}
	return nil
}

// Next is where a restarted run continues: the agent after a done one (the
// next iteration's start, agent "", after tester), or the same agent again
// when it was pending or running (a running agent was cut off).
func (t Task) Next() (iter int, agent string) {
	if t.AgentState != AgentDone {
		return t.Iteration, t.Agent
	}
	i := slices.Index(Agents, t.Agent)
	if i < 0 || i == len(Agents)-1 {
		return t.Iteration + 1, ""
	}
	return t.Iteration, Agents[i+1]
}
