package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/decisions"
	"diadoche/internal/specs"
)

// twoDecisionsAnswer is a fake worker answer reporting two decisions.
const twoDecisionsAnswer = `Implemented the plan; all tests pass.

## Decisions

### Store the index as JSON lines
Context: The index must survive restarts.
Decision: Append one JSON object per line to index.jsonl.
Alternatives: SQLite — rejected, a new dependency for one table.

### Add a /healthz endpoint
Context: The deploy needs a liveness probe.
Decision: Serve GET /healthz returning 200.
Alternatives: none considered
`

var twoDecisions = []decisions.Decision{
	{
		Title:        "Store the index as JSON lines",
		Context:      "The index must survive restarts.",
		Decision:     "Append one JSON object per line to index.jsonl.",
		Alternatives: "SQLite — rejected, a new dependency for one table.",
	},
	{
		Title:        "Add a /healthz endpoint",
		Context:      "The deploy needs a liveness probe.",
		Decision:     "Serve GET /healthz returning 200.",
		Alternatives: "none considered",
	},
}

func decisionsFile(layout specs.Layout) string {
	return filepath.Join(layout.Root, "runs", artifactRunID, "001-example", decisions.FileName)
}

func TestDoneTaskWritesDecisionsJSON(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree=%v", worktree), func(t *testing.T) {
			dir, layout := seedRepo(t)
			runner := newArtifactRunner(layout, 1)
			runner.workerOutput = twoDecisionsAnswer
			o := newTestOrchestrator(dir, layout, runner, config.Config{Iter: 1, Worktree: worktree})
			o.Now = func() time.Time { return artifactNow }

			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			assertStatus(t, layout, specs.StatusDone)

			path := decisionsFile(layout)
			var got []decisions.Decision
			if err := json.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
				t.Fatalf("decisions.json: %v", err)
			}
			if len(got) != len(twoDecisions) {
				t.Fatalf("decisions.json has %d entries, want %d: %#v", len(got), len(twoDecisions), got)
			}
			for i, want := range twoDecisions {
				if got[i] != want {
					t.Errorf("entry %d = %#v, want %#v", i, got[i], want)
				}
			}
			if p := o.DecisionsPath("001_example.md"); p != path {
				t.Errorf("DecisionsPath = %q, want %q", p, path)
			}
			if _, err := os.Stat(o.DecisionsPath("001_example.md")); err != nil {
				t.Errorf("decisions.json gone after the task finished: %v", err)
			}
			log := readRunLog(t, layout)
			requireEvent(t, log, "decisions", "agent=worker", "count=2", "file=decisions.json")
			if strings.Contains(log, "Store the index") {
				t.Errorf("run.log carries decision text:\n%s", log)
			}
			if lg := gitOut(t, dir, "log", "--all", "--name-only", "--format="); strings.Contains(lg, decisions.FileName) {
				t.Errorf("decisions.json was committed:\n%s", lg)
			}
		})
	}
}

func TestDoneTaskWithoutDecisionsSectionWarns(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := newArtifactRunner(layout, 1)
	runner.workerOutput = "Implemented the plan; all tests pass.\n"
	o, buf := bufferedOrchestrator(dir, layout, runner, config.Config{Iter: 1})
	o.Now = func() time.Time { return artifactNow }

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertStatus(t, layout, specs.StatusDone)
	if got := readFile(t, decisionsFile(layout)); got != "[]\n" {
		t.Errorf("decisions.json = %q, want \"[]\\n\"", got)
	}
	if !strings.Contains(buf.String(), "worker did not report its decisions") {
		t.Errorf("no warning about the missing section:\n%s", buf.String())
	}
	log := readRunLog(t, layout)
	requireEvent(t, log, "decisions_missing", "agent=worker")
	requireEvent(t, log, "task_end", "status=DONE")
}

func TestDoneTaskWithNoneDecisions(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := newArtifactRunner(layout, 1)
	runner.workerOutput = "Done.\n\n## Decisions\n\nnone\n"
	o := newTestOrchestrator(dir, layout, runner, config.Config{Iter: 1})
	o.Now = func() time.Time { return artifactNow }

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := readFile(t, decisionsFile(layout)); got != "[]\n" {
		t.Errorf("decisions.json = %q, want \"[]\\n\"", got)
	}
	log := readRunLog(t, layout)
	requireEvent(t, log, "decisions", "count=0")
	if lines := eventLines(log, "decisions_missing"); len(lines) != 0 {
		t.Errorf("unexpected decisions_missing: %v", lines)
	}
}

func TestNotDoneTaskWritesNoDecisions(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := newArtifactRunner(layout, 99)
	runner.workerOutput = twoDecisionsAnswer
	o := newTestOrchestrator(dir, layout, runner, config.Config{Iter: 1})
	o.Now = func() time.Time { return artifactNow }

	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertStatus(t, layout, specs.StatusFailed)
	if _, err := os.Stat(decisionsFile(layout)); !os.IsNotExist(err) {
		t.Errorf("decisions.json exists for a FAILED task: %v", err)
	}
	if p := o.DecisionsPath("001_example.md"); p != "" {
		t.Errorf("DecisionsPath = %q, want \"\"", p)
	}
}

// decisionsRecorder answers every call; worker's answer reports decisions.
type decisionsRecorder struct{}

func (decisionsRecorder) Run(_ context.Context, _ string, a *agents.Agent, _ string) (agents.RunResult, error) {
	if a.Name == "worker" {
		return agents.RunResult{Output: twoDecisionsAnswer, Parsed: true}, nil
	}
	return agents.RunResult{Output: a.Name, Parsed: true}, nil
}

func TestDryRunWritesNoDecisions(t *testing.T) {
	dir, layout := dryRunRepo(t)
	before := treeSnapshot(t, dir)
	o := newDryRunOrchestrator(dir, layout, decisionsRecorder{}, log.New(&bytes.Buffer{}, "", 0))
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	diffSnapshots(t, "files", before, treeSnapshot(t, dir))
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == decisions.FileName {
			t.Errorf("--dry-run wrote %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range []string{"001_a.md", "002_b.md"} {
		if p := o.DecisionsPath(spec); p != "" {
			t.Errorf("DecisionsPath(%s) = %q after --dry-run", spec, p)
		}
	}
}
