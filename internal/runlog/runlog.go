// Package runlog keeps the per-run artifacts of a diadoche run:
//
//	.claude/diadoche/runs/<run-id>/run.log
//	.claude/diadoche/runs/<run-id>/<NNN>-<slug>/iter-<N>-IMPL_PLAN.md
//	.claude/diadoche/runs/<run-id>/<NNN>-<slug>/iter-<N>-REVIEW.md
//	.claude/diadoche/runs/<run-id>/<NNN>-<slug>/iter-<N>-<agent>.stdout|.stderr
//	.claude/diadoche/runs/<run-id>/<NNN>-<slug>/final.diff
//	.claude/diadoche/runs/<run-id>/<NNN>-<slug>/decisions.json
//
// decisions.json is written by the orchestrator for DONE tasks only (see
// package decisions).
//
// <run-id> is the run's start time as 20060102-150405, with a -2, -3...
// suffix when that directory already exists. run.log holds one line per
// event (see Format), each written with a single unbuffered write, so
// `tail -f` shows it the moment it happens.
//
// Every method is safe on a nil *Run (a no-op), so a run whose directory
// couldn't be created just goes on without artifacts.
package runlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// IDLayout is the time layout of a run id.
const IDLayout = "20060102-150405"

// LogName is the run log's file name inside the run directory.
const LogName = "run.log"

// Run is one run's directory and its open run.log.
type Run struct {
	// Dir is the run directory, ID its base name.
	Dir string
	ID  string

	mu    sync.Mutex
	log   *os.File
	calls map[string]int
}

// Open creates a new run directory under runsDir, named after now, and
// opens its run.log for appending.
func Open(runsDir string, now time.Time) (*Run, error) {
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		return nil, err
	}
	base := now.Format(IDLayout)
	id := base
	for n := 2; ; n++ {
		err := os.Mkdir(filepath.Join(runsDir, id), 0o755)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
	dir := filepath.Join(runsDir, id)
	f, err := os.OpenFile(filepath.Join(dir, LogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Run{Dir: dir, ID: id, log: f, calls: map[string]int{}}, nil
}

// Field is one key=value detail of an event.
type Field struct{ Key, Value string }

// F builds a Field, formatting value with fmt's %v.
func F(key string, value any) Field { return Field{key, fmt.Sprint(value)} }

// Event is one run.log line: where in the run it happened (task, iteration,
// agent — each optional) and what happened (Name plus details).
type Event struct {
	Task   string
	Iter   int
	Agent  string
	Name   string
	Fields []Field
}

// Format renders e at t as one line, without the trailing newline:
//
//	<RFC3339 ms time> task=<spec> iter=<N> agent=<name> event=<name> k=v...
//
// A missing task, iteration or agent is "-"; a value with whitespace, '"'
// or '=' is quoted (strconv.Quote), so a line never spans two.
func Format(t time.Time, e Event) string {
	var b strings.Builder
	b.WriteString(t.Format("2006-01-02T15:04:05.000Z07:00"))
	iter := "-"
	if e.Iter > 0 {
		iter = strconv.Itoa(e.Iter)
	}
	fields := append([]Field{
		{"task", e.Task}, {"iter", iter}, {"agent", e.Agent}, {"event", e.Name},
	}, e.Fields...)
	for _, f := range fields {
		b.WriteByte(' ')
		b.WriteString(f.Key)
		b.WriteByte('=')
		b.WriteString(quote(f.Value))
	}
	return b.String()
}

func quote(v string) string {
	if v == "" {
		return "-"
	}
	if strings.ContainsFunc(v, func(r rune) bool {
		return r == '"' || r == '=' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
	}) {
		return strconv.Quote(v)
	}
	return v
}

// Write appends line plus a newline to run.log in a single write.
func (r *Run) Write(line string) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := r.log.Write([]byte(line + "\n"))
	return err
}

// LogPath is the path of run.log.
func (r *Run) LogPath() string {
	if r == nil {
		return ""
	}
	return filepath.Join(r.Dir, LogName)
}

// TaskDir is the directory of task taskID's artifacts.
func (r *Run) TaskDir(taskID string) string {
	if r == nil {
		return ""
	}
	return filepath.Join(r.Dir, taskID)
}

// WriteArtifact writes data to <task dir>/name, creating the task
// directory first if needed.
func (r *Run) WriteArtifact(taskID, name string, data []byte) error {
	if r == nil {
		return nil
	}
	dir := r.TaskDir(taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), data, 0o644)
}

// CopyArtifact copies the file src to <task dir>/name. A missing src
// writes nothing and is not an error.
func (r *Run) CopyArtifact(taskID, name, src string) error {
	if r == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.WriteArtifact(taskID, name, data)
}

// CallName is the base name of an agent call's output files:
// iter-<N>-<agent> for the first call of agent in iteration iter of the
// task, iter-<N>-<agent>-<k> for its k-th (a retry after a limit pause).
func (r *Run) CallName(taskID string, iter int, agent string) string {
	name := fmt.Sprintf("iter-%d-%s", iter, agent)
	if r == nil {
		return name
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := taskID + "/" + name
	r.calls[key]++
	if k := r.calls[key]; k > 1 {
		return fmt.Sprintf("%s-%d", name, k)
	}
	return name
}

// Close closes run.log.
func (r *Run) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.log.Close()
}
