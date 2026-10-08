package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"diadoche/internal/adr"
	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/decisions"
	"diadoche/internal/gitutil"
	"diadoche/internal/specs"
)

// adrRunner is a fake claude: planner writes a valid plan, worker writes
// <num>.txt and answers with answer, tester marks the task DONE, and
// adr-writer runs onADR in its work dir (it never calls a model).
type adrRunner struct {
	layout     specs.Layout
	answer     string
	onADR      func(dir string) error
	adrFails   bool
	adrPrompts []string
	adrDirs    []string
}

func (r *adrRunner) Run(_ context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	if a.Name == "adr-writer" {
		r.adrPrompts = append(r.adrPrompts, prompt)
		r.adrDirs = append(r.adrDirs, dir)
		if r.onADR != nil {
			if err := r.onADR(dir); err != nil {
				return agents.RunResult{ExitCode: 1}, err
			}
		}
		if r.adrFails {
			return agents.RunResult{ExitCode: 3, Stdout: "boom"}, errors.New("adr-writer exited 3")
		}
		return agents.RunResult{Output: "wrote ADRs", Stdout: "adr-writer stdout"}, nil
	}
	row, err := (&hookTaskRunner{layout: r.layout}).current()
	if err != nil {
		return agents.RunResult{}, err
	}
	num := specs.TaskNumber(row.Spec)
	switch a.Name {
	case "planner":
		err = os.WriteFile(filepath.Join(r.layout.SpecDir, num+"_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644)
	case "worker":
		err = os.WriteFile(filepath.Join(dir, num+".txt"), []byte("work of "+row.Spec+"\n"), 0o644)
		return agents.RunResult{Output: r.answer}, err
	case "tester":
		err = specs.SetStatus(r.layout, row.Spec, specs.StatusDone)
	}
	return agents.RunResult{}, err
}

// adrDoc is an ADR file as the agent (with a TMP number) or diadoche
// writes it.
func adrDoc(number, title, status, summary, extra string) string {
	return "# " + number + ". " + title + "\n\n" +
		"Status: " + status + "\n" +
		"Date: 2026-10-08\n" +
		"Summary: " + summary + "\n" +
		"Task: [001_example.md](../../.claude/diadoche/spec/001_example.md)\n\n" +
		"## Context\n\nContext of " + title + ". " + extra + "\n\n" +
		"## Decision\n\n" + summary + "\n\n" +
		"## Alternatives considered\n\nNone recorded.\n\n" +
		"## Consequences\n\nSome.\n"
}

// writeTwoTMPADRs is a fake adr-writer for twoDecisionsAnswer.
func writeTwoTMPADRs(dir string) error {
	adrDir := filepath.Join(dir, "docs", "adr")
	if err := os.MkdirAll(adrDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(adrDir, "TMP1-index-as-jsonl.md"),
		[]byte(adrDoc("TMP1", "Store the index as JSON lines", "Accepted", "Append to index.jsonl.", "See TMP2.")), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(adrDir, "TMP2-healthz.md"),
		[]byte(adrDoc("TMP2", "Add a /healthz endpoint", "Accepted", "Serve GET /healthz.", "Supersedes 0001.")), 0o644)
}

var (
	existingADR1 = adrDoc("0001", "Store the index in SQLite", "Accepted", "Use SQLite.", "")
	existingADR2 = adrDoc("0002", "Log to stderr", "Accepted", "Log to stderr.", "")
)

// seedADRs commits two existing ADRs into the repository.
func seedADRs(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "docs", "adr", "0001-sqlite-index.md"), existingADR1)
	writeFile(t, filepath.Join(dir, "docs", "adr", "0002-log-to-stderr.md"), existingADR2)
	if err := gitutil.CommitAll(context.Background(), dir, "existing ADRs"); err != nil {
		t.Fatal(err)
	}
}

func adrFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "docs", "adr"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestADRStepWithoutDecisionsDoesNothing(t *testing.T) {
	for name, answer := range map[string]string{
		"none":    "Done.\n\n## Decisions\n\nnone\n",
		"empty":   "Done.\n\n## Decisions\n\n",
		"missing": "Done.\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir, layout := seedRepo(t)
			runner := &adrRunner{layout: layout, answer: answer, onADR: writeTwoTMPADRs}
			o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true, ADR: true})
			o.Now = func() time.Time { return artifactNow }
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(runner.adrPrompts) != 0 {
				t.Fatalf("adr-writer was called %d time(s)", len(runner.adrPrompts))
			}
			for _, p := range []string{filepath.Join(dir, "docs"), filepath.Join(dir, "CLAUDE.md")} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("%s exists (err = %v)", p, err)
				}
			}
			if u := readFile(t, filepath.Join(layout.Root, "usage.jsonl")); strings.Contains(u, "adr-writer") {
				t.Errorf("usage.jsonl has an adr-writer record:\n%s", u)
			}
			if row := statusRow(t, layout, "001_example.md"); row.Status != specs.StatusDone || row.Reason != "" {
				t.Errorf("row = %+v, want DONE without a reason", row)
			}
			if n := onTaskDoneSubjects(t, dir); n != 0 {
				t.Errorf("%d on-task-done commit(s), want none", n)
			}
		})
	}
}

func TestADRStepWritesNumberedADRsAndIndex(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree=%v", worktree), func(t *testing.T) {
			dir, layout := seedRepo(t)
			seedADRs(t, dir)
			oldIndex := "# old index\n| 0001 | 0002 |\n"
			writeFile(t, filepath.Join(dir, "docs", "adr", "README.md"), oldIndex)
			if err := gitutil.CommitAll(context.Background(), dir, "old index"); err != nil {
				t.Fatal(err)
			}
			hookOut := filepath.Join(t.TempDir(), "ls.txt")
			hook := writeHook(t, fmt.Sprintf("ls docs/adr > %q", hookOut))
			runner := &adrRunner{layout: layout, answer: twoDecisionsAnswer, onADR: writeTwoTMPADRs}
			o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: worktree, ADR: true, OnTaskDone: []string{hook}})
			o.Now = func() time.Time { return artifactNow }
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if len(runner.adrPrompts) != 1 {
				t.Fatalf("adr-writer called %d times, want 1", len(runner.adrPrompts))
			}
			root, _ := filepath.Abs(dir)
			if runner.adrDirs[0] != root {
				t.Errorf("adr-writer ran in %q, want the repository root %q", runner.adrDirs[0], root)
			}
			want := []string{"0001-sqlite-index.md", "0002-log-to-stderr.md", "0003-index-as-jsonl.md", "0004-healthz.md", "README.md"}
			if got := adrFiles(t, dir); !slices.Equal(got, want) {
				t.Fatalf("docs/adr = %v, want %v", got, want)
			}
			if readFile(t, filepath.Join(dir, "docs", "adr", "0001-sqlite-index.md")) != existingADR1 ||
				readFile(t, filepath.Join(dir, "docs", "adr", "0002-log-to-stderr.md")) != existingADR2 {
				t.Error("existing ADRs changed")
			}
			a3 := readFile(t, filepath.Join(dir, "docs", "adr", "0003-index-as-jsonl.md"))
			if !strings.HasPrefix(a3, "# 0003. Store the index as JSON lines\n") || !strings.Contains(a3, "See 0004.") || strings.Contains(a3, "TMP") {
				t.Errorf("0003 =\n%s", a3)
			}
			if a4 := readFile(t, filepath.Join(dir, "docs", "adr", "0004-healthz.md")); !strings.HasPrefix(a4, "# 0004. Add a /healthz endpoint\n") {
				t.Errorf("0004 =\n%s", a4)
			}
			index := readFile(t, filepath.Join(dir, "docs", "adr", "README.md"))
			for _, row := range []string{
				"| [0001](0001-sqlite-index.md) | Store the index in SQLite | Accepted | Use SQLite. |",
				"| [0002](0002-log-to-stderr.md) | Log to stderr | Accepted | Log to stderr. |",
				"| [0003](0003-index-as-jsonl.md) | Store the index as JSON lines | Accepted | Append to index.jsonl. |",
				"| [0004](0004-healthz.md) | Add a /healthz endpoint | Accepted | Serve GET /healthz. |",
			} {
				if !strings.Contains(index, row) {
					t.Errorf("index lacks %q:\n%s", row, index)
				}
			}
			if got := readFile(t, filepath.Join(dir, "CLAUDE.md")); got != adr.ClaudeLine+"\n" {
				t.Errorf("CLAUDE.md = %q", got)
			}

			// The hook ran after the ADR step and saw its files.
			if got := readFile(t, hookOut); !strings.Contains(got, "0004-healthz.md") {
				t.Errorf("the on-task-done hook didn't see the new ADRs: %q", got)
			}

			// Everything landed in the one on-task-done commit.
			if n := onTaskDoneSubjects(t, dir); n != 1 {
				t.Errorf("%d on-task-done commits, want 1", n)
			}
			// usage.jsonl gets adr-writer's record after the task's commit;
			// diadoche's bookkeeping is never part of the on-task-done commit.
			if st := gitOut(t, dir, "status", "--porcelain", "--", ".", ":(exclude).claude/diadoche"); st != "" {
				t.Errorf("main tree not clean:\n%s", st)
			}
			files := gitOut(t, dir, "show", "--name-only", "--format=", "HEAD")
			for _, f := range []string{"CLAUDE.md", "docs/adr/0003-index-as-jsonl.md", "docs/adr/0004-healthz.md", "docs/adr/README.md"} {
				if !strings.Contains(files, f) {
					t.Errorf("on-task-done commit lacks %s:\n%s", f, files)
				}
			}

			prompt := runner.adrPrompts[0]
			wantJSON, _ := decisions.Marshal(twoDecisions)
			for _, want := range []string{
				string(wantJSON), "+work of 001_example.md", oldIndex, "0001-sqlite-index.md",
				"2026-10-08", "../../.claude/diadoche/spec/001_example.md", "TMP<k>",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("adr-writer prompt lacks %q:\n%s", want, prompt)
				}
			}
			if strings.Contains(prompt, "diadoche/spec/STATUS.md") {
				t.Error("the diff in the prompt includes diadoche's bookkeeping")
			}

			if u := readFile(t, filepath.Join(layout.Root, "usage.jsonl")); !strings.Contains(u, `"agent":"adr-writer","iteration":0`) {
				t.Errorf("usage.jsonl lacks the adr-writer record:\n%s", u)
			}
			taskDir := filepath.Join(layout.Root, "runs", artifactRunID, "001-example")
			if got := readFile(t, filepath.Join(taskDir, "iter-0-adr-writer.stdout")); got != "adr-writer stdout" {
				t.Errorf("iter-0-adr-writer.stdout = %q", got)
			}
			runLog := readRunLog(t, layout)
			requireEvent(t, runLog, "adr_start", "decisions=2")
			requireEvent(t, runLog, "adr", "result=ok", "written=2", "numbers=0003,0004")
			requireEvent(t, runLog, "agent_end", "adr-writer")
			if row := statusRow(t, layout, "001_example.md"); row.Status != specs.StatusDone || row.Reason != "" {
				t.Errorf("row = %+v, want DONE without a reason", row)
			}
		})
	}
}

func TestADRStepPlaceholderNumbersAreReplacedNotTrusted(t *testing.T) {
	dir, layout := seedRepo(t)
	seedADRs(t, dir)
	// The agent "guesses" numbers that collide with existing ADRs in its
	// placeholders; diadoche still numbers from 0003 on.
	runner := &adrRunner{layout: layout, answer: twoDecisionsAnswer, onADR: func(d string) error {
		adrDir := filepath.Join(d, "docs", "adr")
		if err := os.WriteFile(filepath.Join(adrDir, "TMP2-second.md"), []byte(adrDoc("TMP2", "Second", "Accepted", "two", "")), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(adrDir, "TMP1-first.md"), []byte(adrDoc("TMP1", "First", "Accepted", "one", "")), 0o644)
	}}
	o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true, ADR: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"0001-sqlite-index.md", "0002-log-to-stderr.md", "0003-first.md", "0004-second.md", "README.md"}
	if got := adrFiles(t, dir); !slices.Equal(got, want) {
		t.Fatalf("docs/adr = %v, want %v", got, want)
	}
	if got := readFile(t, filepath.Join(dir, "docs", "adr", "0004-second.md")); !strings.HasPrefix(got, "# 0004. Second\n") {
		t.Errorf("0004 =\n%s", got)
	}
}

func TestADRStepSupersede(t *testing.T) {
	dir, layout := seedRepo(t)
	seedADRs(t, dir)
	runner := &adrRunner{layout: layout, answer: twoDecisionsAnswer, onADR: func(d string) error {
		if err := writeTwoTMPADRs(d); err != nil {
			return err
		}
		// Supersede 0001 — and, against the rules, also rewrite its body.
		p := filepath.Join(d, "docs", "adr", "0001-sqlite-index.md")
		edited := strings.Replace(existingADR1, "Status: Accepted", "Status: Superseded by TMP1", 1)
		edited = strings.Replace(edited, "Use SQLite.", "Rewritten by the agent.", -1)
		return os.WriteFile(p, []byte(edited), 0o644)
	}}
	o := newTestOrchestrator(dir, layout, runner, config.Config{Worktree: true, ADR: true})
	o.Now = func() time.Time { return artifactNow }
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := readFile(t, filepath.Join(dir, "docs", "adr", "0001-sqlite-index.md"))
	if want := strings.Replace(existingADR1, "Status: Accepted", "Status: Superseded by 0003", 1); got != want {
		t.Errorf("0001 =\n%s\nwant only its status changed:\n%s", got, want)
	}
	if a4 := readFile(t, filepath.Join(dir, "docs", "adr", "0004-healthz.md")); !strings.Contains(a4, "Supersedes 0001.") {
		t.Errorf("0004 doesn't reference 0001:\n%s", a4)
	}
	index := readFile(t, filepath.Join(dir, "docs", "adr", "README.md"))
	if !strings.Contains(index, "| [0001](0001-sqlite-index.md) | Store the index in SQLite | Superseded by 0003 | Use SQLite. |") {
		t.Errorf("index doesn't show 0001 superseded:\n%s", index)
	}
	runLog := readRunLog(t, layout)
	requireEvent(t, runLog, "adr", "result=ok", "superseded=1")
	requireEvent(t, runLog, "warning", "what=adr", "0001-sqlite-index.md")
}

func TestADRStepClaudeLineOnce(t *testing.T) {
	for _, existing := range []string{"", "# Project notes\nRun make check."} {
		t.Run(fmt.Sprintf("existing=%v", existing != ""), func(t *testing.T) {
			dir, layout := seedRepo(t)
			if existing != "" {
				writeFile(t, filepath.Join(dir, "CLAUDE.md"), existing)
			}
			runner := &adrRunner{layout: layout, onADR: func(d string) error {
				adrDir := filepath.Join(d, "docs", "adr")
				if err := os.MkdirAll(adrDir, 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(adrDir, "TMP1-one.md"), []byte(adrDoc("TMP1", "One", "Accepted", "one", "")), 0o644)
			}}
			o := newTestOrchestrator(dir, layout, runner, config.Config{ADR: true})
			o.Log = log.New(&bytes.Buffer{}, "", 0)
			if _, _, _, err := o.loadAgents(); err != nil {
				t.Fatal(err)
			}
			decisionsPath := filepath.Join(t.TempDir(), "decisions.json")
			data, _ := decisions.Marshal(twoDecisions[:1])
			writeFile(t, decisionsPath, string(data))
			o.decisions = map[string]string{"001_example.md": decisionsPath}

			for i := range 2 {
				if out := o.runADRStep(context.Background(), "001_example.md", "001", &workspace{}, nil); out.fragment != "" {
					t.Fatalf("run %d: %s (%v)", i+1, out.fragment, out.err)
				}
			}
			got := readFile(t, filepath.Join(dir, "CLAUDE.md"))
			if n := strings.Count(got, adr.ClaudeLine); n != 1 {
				t.Errorf("CLAUDE.md has the line %d times:\n%s", n, got)
			}
			if existing != "" && !strings.HasPrefix(got, existing+"\n") {
				t.Errorf("CLAUDE.md lost its content:\n%s", got)
			}
			if strings.Contains(got, "0001") || strings.Contains(got, "0002") {
				t.Errorf("CLAUDE.md links individual ADRs:\n%s", got)
			}
			if got := adrFiles(t, dir); !slices.Equal(got, []string{"0001-one.md", "0002-one.md", "README.md"}) {
				t.Errorf("docs/adr = %v", got)
			}
		})
	}
}

func TestADRStepFailureIsWarningAndRollsBack(t *testing.T) {
	for name, runner := range map[string]func(specs.Layout) *adrRunner{
		"agent error": func(l specs.Layout) *adrRunner {
			return &adrRunner{layout: l, answer: twoDecisionsAnswer, onADR: writeTwoTMPADRs, adrFails: true}
		},
		"no ADR written": func(l specs.Layout) *adrRunner {
			return &adrRunner{layout: l, answer: twoDecisionsAnswer}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, layout := seedRepo(t)
			marker := filepath.Join(t.TempDir(), "ran")
			hook := writeHook(t, fmt.Sprintf("touch %q", marker))
			r := runner(layout)
			o := newTestOrchestrator(dir, layout, r, config.Config{Worktree: true, ADR: true, OnTaskDone: []string{hook}})
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			row := statusRow(t, layout, "001_example.md")
			if row.Status != specs.StatusDone || !strings.Contains(row.Reason, "adr step failed") {
				t.Errorf("row = %+v, want DONE with an adr step warning", row)
			}
			if name == "agent error" && !strings.Contains(row.Reason, "adr step failed (adr-writer exit 3)") {
				t.Errorf("reason = %q", row.Reason)
			}
			if name == "no ADR written" && !strings.Contains(row.Reason, "adr step failed (no ADR written)") {
				t.Errorf("reason = %q", row.Reason)
			}
			for _, p := range []string{filepath.Join(dir, "docs"), filepath.Join(dir, "CLAUDE.md")} {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("%s left behind (err = %v)", p, err)
				}
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("the on-task-done hook didn't run after the failed ADR step: %v", err)
			}
			requireEvent(t, readRunLog(t, layout), "adr", "result=failed")
		})
	}
}

func TestADRStepDryRunCallsNoAgent(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &agentNameRunner{}
	var logBuf bytes.Buffer
	o := newTestOrchestrator(dir, layout, runner, config.Config{DryRun: true, ADR: true})
	o.Log = log.New(&logBuf, "", 0)
	if _, _, err := specs.Sync(layout); err != nil {
		t.Fatal(err)
	}
	if err := gitutil.CommitAll(context.Background(), dir, "sync"); err != nil {
		t.Fatal(err)
	}
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if slices.Contains(runner.names, "adr-writer") {
		t.Errorf("dry run called adr-writer: %v", runner.names)
	}
	if !strings.Contains(logBuf.String(), "would run the ADR step") {
		t.Errorf("dry run doesn't mention the ADR step:\n%s", logBuf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "docs")); !os.IsNotExist(err) {
		t.Errorf("dry run wrote docs/: %v", err)
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("dry run changed the tree:\n%s", st)
	}
}

// agentNameRunner records the agents it was asked to run.
type agentNameRunner struct{ names []string }

func (r *agentNameRunner) Run(_ context.Context, _ string, a *agents.Agent, _ string) (agents.RunResult, error) {
	r.names = append(r.names, a.Name)
	return agents.RunResult{}, nil
}

func TestADRWriterModelResolution(t *testing.T) {
	dir, layout := seedRepo(t)
	unknown := agents.ModelOverride{Value: "no-such-model", Source: agents.ModelSourceFlag}
	for _, cfg := range []config.Config{{ADR: true, ADRModel: unknown}, {ADRModel: unknown}} {
		runner := &agentNameRunner{}
		o := newTestOrchestrator(dir, layout, runner, cfg)
		if err := o.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "no-such-model") {
			t.Errorf("ADR=%v: Run err = %v, want an unknown-model error", cfg.ADR, err)
		}
		if len(runner.names) != 0 {
			t.Errorf("ADR=%v: agents ran before the startup error: %v", cfg.ADR, runner.names)
		}
	}

	load := func(cfg config.Config) *agents.Agent {
		t.Helper()
		o := newTestOrchestrator(dir, layout, nil, cfg)
		if _, _, _, err := o.loadAgents(); err != nil {
			t.Fatal(err)
		}
		return o.adrWriter
	}
	if a := load(config.Config{ADR: true}); a.Model != "claude-sonnet-5-5" || a.ModelSource != agents.ModelSourceDefault {
		t.Errorf("default: %s (%s)", a.Model, a.ModelSource)
	}
	if a := load(config.Config{}); a != nil {
		t.Errorf("adr-writer loaded without --adr or an override: %+v", a)
	}
	writeFile(t, filepath.Join(layout.AgentsDir, "adr-writer.md"), "---\nname: adr-writer\nmodel: claude-haiku-4-5-20251001\n---\nwrite ADRs")
	if a := load(config.Config{ADR: true}); a.Model != "claude-haiku-4-5-20251001" || a.ModelSource != agents.ModelSourceFrontmatter {
		t.Errorf("frontmatter: %s (%s)", a.Model, a.ModelSource)
	}
	t.Setenv("DIADOCHE_ADR_MODEL", "claude-opus-5-5")
	env, err := config.Parse([]string{"--adr"})
	if err != nil {
		t.Fatal(err)
	}
	if a := load(*env); a.Model != "claude-opus-5-5" || a.ModelSource != agents.ModelSourceEnv {
		t.Errorf("env: %s (%s)", a.Model, a.ModelSource)
	}
	flag, err := config.Parse([]string{"--adr", "--adr-model", "claude-sonnet-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if a := load(*flag); a.Model != "claude-sonnet-5-5" || a.ModelSource != agents.ModelSourceFlag {
		t.Errorf("flag: %s (%s)", a.Model, a.ModelSource)
	}
}
