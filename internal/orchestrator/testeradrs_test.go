package orchestrator

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/specs"
)

// writeTestADRs writes three Accepted ADRs, 0001–0003, into root/docs/adr,
// each with a unique body marker.
func writeTestADRs(t *testing.T, root string) {
	t.Helper()
	for _, a := range []struct{ file, title, marker string }{
		{"0001-alpha.md", "Alpha", "ALPHA-BODY-MARKER"},
		{"0002-beta.md", "Beta", "BETA-BODY-MARKER"},
		{"0003-gamma.md", "Gamma", "GAMMA-BODY-MARKER"},
	} {
		content := "# " + a.file[:4] + ". " + a.title + "\n\nStatus: Accepted\n\n## Decision\n" + a.marker + "\n"
		writeFile(t, filepath.Join(root, "docs", "adr", a.file), content)
	}
}

// runADRTask runs one task whose planner writes plan, in a repo where the
// ADRs are committed (so they are not in the tester diff), and returns
// the runner, the log and the task's final status.
func runADRTask(t *testing.T, plan string) (*scriptedRunner, string, string) {
	t.Helper()
	requireGit(t)
	setTestGitEnv(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := gitutil.EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	layout := specs.NewLayout(dir)
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeAgentFixtures(t, layout.AgentsDir)
	specFile := "001_example.md"
	writeFile(t, filepath.Join(layout.SpecDir, specFile), "# Example\n")
	writeTestADRs(t, dir)
	if err := gitutil.CommitAll(ctx, dir, "seed"); err != nil {
		t.Fatal(err)
	}

	runner := &scriptedRunner{
		layout: layout,
		onPlanner: func(specDir string) error {
			return os.WriteFile(filepath.Join(specDir, "001_IMPL_PLAN.md"), []byte(plan), 0o644)
		},
		onWorker: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "new_feature.go"), []byte("package main\n"), 0o644)
		},
		markDoneOnTester: true,
	}
	var logBuf bytes.Buffer
	o := &Orchestrator{
		Cfg:    config.Config{Dir: dir, Iter: 1, MaxDiffBytes: config.DefaultMaxDiffBytes},
		Layout: layout,
		Runner: runner,
		Log:    log.New(&logBuf, "", 0),
	}
	if err := o.Run(ctx); err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	status, err := specs.GetStatus(layout, specFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.plannerPrompts) != 1 {
		t.Errorf("planner calls = %d, want 1 (the plan must pass validation)", len(runner.plannerPrompts))
	}
	if runner.testerCalls != 1 {
		t.Fatalf("testerCalls = %d, want 1", runner.testerCalls)
	}
	return runner, logBuf.String(), status
}

func TestTesterPromptCarriesRelevantADRs(t *testing.T) {
	plan := validPlanFixture + "\n## Relevant ADRs\n- 0001 — Alpha\n- 0003 — Gamma\n"
	runner, _, status := runADRTask(t, plan)
	prompt := runner.testerPrompts[0]
	for _, want := range []string{
		"ADRs listed in the plan: 0001, 0003",
		"--- ADR 0001: Alpha (Status: Accepted)",
		"--- ADR 0003: Gamma (Status: Accepted)",
		"ALPHA-BODY-MARKER", "GAMMA-BODY-MARKER",
		"Worker's ## Decisions in this iteration:",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("tester prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "BETA-BODY-MARKER") {
		t.Errorf("tester prompt carries an ADR the plan didn't list:\n%s", prompt)
	}
	if status != specs.StatusDone {
		t.Errorf("status = %s, want DONE", status)
	}
}

func TestEmptyRelevantADRsPassesValidationAndSaysNone(t *testing.T) {
	runner, logs, status := runADRTask(t, validPlanFixture+"\n## Relevant ADRs\nnone\n")
	prompt := runner.testerPrompts[0]
	if !strings.Contains(prompt, noRelevantADRsListed) {
		t.Errorf("tester prompt lacks %q:\n%s", noRelevantADRsListed, prompt)
	}
	if strings.Contains(prompt, "ALPHA-BODY-MARKER") {
		t.Errorf("tester prompt carries an ADR for an empty list:\n%s", prompt)
	}
	if strings.Contains(logs, `"## Relevant ADRs" section`) {
		t.Errorf("an empty Relevant ADRs section must not be warned about:\n%s", logs)
	}
	if status != specs.StatusDone {
		t.Errorf("status = %s, want DONE", status)
	}
}

func TestMissingRelevantADRsPassesValidationWarnsAndSaysNone(t *testing.T) {
	runner, logs, status := runADRTask(t, validPlanFixture)
	prompt := runner.testerPrompts[0]
	if !strings.Contains(prompt, noRelevantADRsSection) {
		t.Errorf("tester prompt lacks %q:\n%s", noRelevantADRsSection, prompt)
	}
	if !strings.Contains(logs, `warning: 001_example.md iteration 1: 001_IMPL_PLAN.md has no "## Relevant ADRs" section`) {
		t.Errorf("log lacks the missing-section warning:\n%s", logs)
	}
	if status != specs.StatusDone {
		t.Errorf("status = %s, want DONE", status)
	}
}

func TestRelevantADRsBlock(t *testing.T) {
	root := t.TempDir()
	writeTestADRs(t, root)
	const plan = "## Relevant ADRs\n- 0002 — Beta\n- 0009 — Missing\n"

	cases := []struct {
		name         string
		workerOutput string
		workerRan    bool
		want         []string
	}{
		{"worker not run", "", false, []string{"(not available: worker was not run in this iteration"}},
		{"decisions none", "done\n\n## Decisions\n\nnone\n", true, []string{"Worker's ## Decisions in this iteration:\nnone"}},
		{"no decisions section", "done\n", true, []string{"(worker's answer had no ## Decisions section)"}},
		{"decisions", "## Decisions\n\n### Drop ADR 0002\nContext: c\nDecision: d\nAlternatives: none considered\n", true,
			[]string{`"title": "Drop ADR 0002"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			o := &Orchestrator{Log: log.New(&logBuf, "", 0)}
			got := o.relevantADRsBlock(adrBlockInput{specFile: "001_x.md", iter: 1, plan: plan, root: root,
				workerOutput: c.workerOutput, workerRan: c.workerRan})
			want := append([]string{
				"ADRs listed in the plan: 0002, 0009",
				"--- ADR 0002: Beta (Status: Accepted) — docs/adr/0002-beta.md ---",
				"BETA-BODY-MARKER",
				"--- ADR 0009 — not found in docs/adr/ ---",
			}, c.want...)
			for _, w := range want {
				if !strings.Contains(got, w) {
					t.Errorf("block lacks %q:\n%s", w, got)
				}
			}
			if !strings.Contains(logBuf.String(), "warning: 001_x.md iteration 1: plan lists ADR 0009") {
				t.Errorf("log lacks the not-found warning:\n%s", logBuf.String())
			}
		})
	}

	o := &Orchestrator{Log: log.New(io.Discard, "", 0)}
	if got := o.relevantADRsBlock(adrBlockInput{plan: "## Files\n- a.go\n", root: root}); got != noRelevantADRsSection {
		t.Errorf("no section: block = %q, want %q", got, noRelevantADRsSection)
	}
	if got := o.relevantADRsBlock(adrBlockInput{plan: "## Relevant ADRs\n- none\n", root: root}); got != noRelevantADRsListed {
		t.Errorf("empty list: block = %q, want %q", got, noRelevantADRsListed)
	}
}
