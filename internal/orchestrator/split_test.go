package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"diadoche/internal/agents"
	"diadoche/internal/onfail"
	"diadoche/internal/specs"
)

var threeParts = []splitFile{
	{"004_part_a.md", "# Part A\n\nDo part A.\n"},
	{"005_part_b.md", "# Part B\n\nDo part B.\n\nDepends-on: 004\n"},
	{"006_part_c.md", "# Part C\n\nDo part C.\n"},
}

// seedSplitRepo is seedRepo plus 002_two.md and 003_three.md, queued
// behind 001_example.md.
func seedSplitRepo(t *testing.T) (string, specs.Layout) {
	t.Helper()
	dir, layout := seedRepo(t)
	writeFile(t, filepath.Join(layout.SpecDir, "002_two.md"), "# Two\n")
	writeFile(t, filepath.Join(layout.SpecDir, "003_three.md"), "# Three\n")
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-m", "more specs")
	return dir, layout
}

// approveAllBut approves every spec the split tests use except 001.
func approveAllBut() map[string]bool {
	return map[string]bool{"002_two.md": true, "003_three.md": true, "004_part_a.md": true, "005_part_b.md": true, "006_part_c.md": true}
}

// specOrder is the specs runner.calls worked on, in order, each once.
func specOrder(calls []escCall) []string {
	var out []string
	for _, c := range calls {
		if len(out) == 0 || out[len(out)-1] != c.spec {
			out = append(out, c.spec)
		}
	}
	return out
}

func TestOnFailSplitCallsPreprocessorWithSpecAndAllReviews(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &escalationRunner{layout: layout, splitFiles: threeParts[:2],
		approve: map[string]bool{"004_part_a.md": true, "005_part_b.md": true}}
	o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Split}, false))
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	prompts := runner.prompts["preprocessor"]
	if len(prompts) != 1 {
		t.Fatalf("preprocessor calls = %d, want 1", len(prompts))
	}
	for _, want := range []string{"# Example\n", "001_example.md",
		"001_REVIEW_1.md", "review 001_REVIEW_1.md: not done yet",
		"001_REVIEW_2.md", "review 001_REVIEW_2.md: not done yet", "2–4", layout.SpecDir} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("preprocessor prompt lacks %q:\n%s", want, prompts[0])
		}
	}
	if i1, i2 := strings.Index(prompts[0], "001_REVIEW_1.md"), strings.Index(prompts[0], "001_REVIEW_2.md"); i1 > i2 {
		t.Error("reviews not in iteration order")
	}

	found := false
	for _, rec := range readUsage(t, layout) {
		if rec.Agent == "preprocessor" && rec.Task == "001_example.md" {
			found = true
		}
	}
	if !found {
		t.Error("usage.jsonl has no preprocessor record for 001_example.md")
	}
	log := readRunLog(t, layout)
	requireEvent(t, log, "agent_start", "task=001_example.md", "agent=preprocessor", "on_fail=split")
	requireEvent(t, log, "agent_end", "task=001_example.md", "agent=preprocessor")
	requireEvent(t, log, "on_fail_policy", "policy=split", "outcome=split", "into=004_part_a.md,005_part_b.md")
	requireEvent(t, log, "split", "task=001_example.md")
	if !fileExists(filepath.Join(taskArtifactDir(t, layout), "iter-0-preprocessor.stdout")) {
		t.Error("no artifact of the preprocessor call")
	}
}

func TestOnFailSplitQueuesNewSpecsInPlace(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree=%v", worktree), func(t *testing.T) {
			dir, layout := seedSplitRepo(t)
			seed := gitOut(t, dir, "rev-parse", "HEAD")
			runner := &escalationRunner{layout: layout, splitFiles: threeParts, approve: approveAllBut()}
			o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Split}, worktree))
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			// Fresh numbers above every pre-existing spec, a Split-from line.
			for _, f := range threeParts {
				content := readFile(t, filepath.Join(layout.SpecDir, f.name))
				if !strings.HasPrefix(content, f.content) || specs.ParseSplitFrom([]byte(content)) != "001_example.md" {
					t.Errorf("%s = %q, want its content plus Split-from: 001_example.md", f.name, content)
				}
				if n, _ := strconv.Atoi(specs.TaskNumber(f.name)); n <= 3 {
					t.Errorf("%s collides with an existing number", f.name)
				}
			}

			// QUEUED in STATUS.md, at the head of QUEUE.md, ahead of 002/003.
			if got := runner.queueAtFirstCall["004_part_a.md"]; strings.Join(got, ",") != "005_part_b.md,006_part_c.md,002_two.md,003_three.md" {
				t.Errorf("QUEUE.md when 004 started = %v", got)
			}
			if got := specOrder(runner.calls); strings.Join(got, ",") != "001_example.md,004_part_a.md,005_part_b.md,006_part_c.md,002_two.md,003_three.md" {
				t.Errorf("task order = %v", got)
			}
			rows, _ := specs.ReadStatus(layout.StatusPath)
			var order []string
			for _, r := range rows {
				order = append(order, r.Spec)
			}
			if strings.Join(order, ",") != "001_example.md,004_part_a.md,005_part_b.md,006_part_c.md,002_two.md,003_three.md" {
				t.Errorf("STATUS.md rows = %v, want the parts right after the original", order)
			}
			for _, f := range threeParts {
				if row := taskRow(t, layout, f.name); row.Status != specs.StatusDone {
					t.Errorf("%s = %s, want DONE (it was queued and run)", f.name, row.Status)
				}
			}

			// The parts start from a clean base; 001's changes are parked.
			if got := runner.editAtFirstCall["004_part_a.md"]; got != "edit.txt original\n" {
				t.Errorf("edit.txt when 004 started = %q, want the original", got)
			}
			branch := "diadoche/split/001-example"
			if got := gitOut(t, dir, "show", branch+":edit.txt"); !strings.HasPrefix(got, "edited by worker") {
				t.Errorf("edit.txt on %s = %q", branch, got)
			}
			if got := gitOut(t, dir, "merge-base", branch, seed); got != seed {
				t.Errorf("%s does not start from the task's base", branch)
			}
			if worktree && fileExists(filepath.Join(layout.Root, "worktrees", "001-example")) {
				t.Error("the split task's worktree was kept")
			}
			report := readFile(t, filepath.Join(layout.SpecDir, "001_BLOCKED.md"))
			for _, want := range []string{"split", "004_part_a.md", "005_part_b.md", "006_part_c.md", branch, "001_REVIEW_2.md"} {
				if !strings.Contains(report, want) {
					t.Errorf("001_BLOCKED.md lacks %q:\n%s", want, report)
				}
			}
			if fileExists(filepath.Join(layout.SpecDir, "001_FAILED.md")) {
				t.Error("001_FAILED.md written for a split task")
			}
			if subjects := gitOut(t, dir, "log", "--format=%s"); !strings.Contains(subjects, "diadoche: split 001_example.md") {
				t.Errorf("no bookkeeping commit; git log:\n%s", subjects)
			}
		})
	}
}

func TestOnFailSplitOriginalIsTerminalAndNotRetried(t *testing.T) {
	dir, layout := seedSplitRepo(t)
	runner := &escalationRunner{layout: layout, splitFiles: threeParts, approve: approveAllBut()}
	o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Escalate, onfail.Split, onfail.Block}, false))
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	row := taskRow(t, layout, "001_example.md")
	if row.Status != specs.StatusBlocked {
		t.Fatalf("status = %s, want BLOCKED", row.Status)
	}
	for _, want := range []string{"split into", "004_part_a.md", "005_part_b.md", "006_part_c.md", "escalate, split"} {
		if !strings.Contains(row.Reason, want) {
			t.Errorf("reason %q lacks %q", row.Reason, want)
		}
	}
	if slices.ContainsFunc(eventLines(readRunLog(t, layout), "on_fail_policy"), func(l string) bool { return strings.Contains(l, "policy=block") }) {
		t.Error("block ran after a successful split")
	}
	if pending, err := specs.PendingQueue(layout); err != nil || slices.Contains(pending, "001_example.md") {
		t.Errorf("PendingQueue = %v, %v; the original must not be pending", pending, err)
	}

	again := &escalationRunner{layout: layout}
	o2 := newTestOrchestrator(dir, layout, again, onFailConfig([]string{onfail.Split}, false))
	if err := o2.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(again.calls) != 0 {
		t.Errorf("the second run called agents %v; a split task is never retried", again.calls)
	}
	if got := taskRow(t, layout, "001_example.md"); got.Status != specs.StatusBlocked || got.Reason != row.Reason {
		t.Errorf("after the second run: %s: %s", got.Status, got.Reason)
	}
}

func TestOnFailSplitNoopFallsThroughToBlock(t *testing.T) {
	cases := []struct {
		name   string
		files  []splitFile
		err    error
		reason string
	}{
		{"call fails", threeParts[:1], errors.New("preprocessor crashed"), "reason=preprocessor_failed"},
		{"no new spec", nil, nil, "reason=no_new_specs"},
		{"single spec", threeParts[:1], nil, "reason=single_spec"},
		{"bad numbering", []splitFile{{"001_dup.md", "# dup\n"}, {"007_ok.md", "# ok\n"}}, nil, "reason=bad_numbering"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, layout := seedRepo(t)
			runner := &escalationRunner{layout: layout, splitFiles: c.files, splitErr: c.err}
			o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Split, onfail.Block}, false))
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			row := taskRow(t, layout, "001_example.md")
			if row.Status != specs.StatusBlocked || !strings.Contains(row.Reason, "split, block") {
				t.Fatalf("001 = %s: %s, want BLOCKED by split, block", row.Status, row.Reason)
			}
			if len(runner.prompts["preprocessor"]) != 1 {
				t.Errorf("preprocessor calls = %d, want 1", len(runner.prompts["preprocessor"]))
			}
			policies := eventLines(readRunLog(t, layout), "on_fail_policy")
			if len(policies) != 2 || !strings.Contains(policies[0], "policy=split") || !strings.Contains(policies[0], "outcome=no_op") ||
				!strings.Contains(policies[0], c.reason) || !strings.Contains(policies[1], "policy=block") || !strings.Contains(policies[1], "outcome=blocked") {
				t.Errorf("on_fail_policy events = %v", policies)
			}
			files, _ := specs.DiscoverTaskFiles(layout.SpecDir)
			if strings.Join(files, ",") != "001_example.md" {
				t.Errorf("spec files = %v, want the split's files removed", files)
			}
			rows, _ := specs.ReadStatus(layout.StatusPath)
			if len(rows) != 1 {
				t.Errorf("STATUS.md rows = %v, want only 001", rows)
			}
		})
	}
}

func TestOnFailChainEscalateSplitBlock(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &escalationRunner{layout: layout}
	o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Escalate, onfail.Split, onfail.Block}, true))
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if row := taskRow(t, layout, "001_example.md"); row.Status != specs.StatusBlocked || !strings.Contains(row.Reason, "escalate, split, block") {
		t.Fatalf("001 = %s: %s, want BLOCKED after all three policies", row.Status, row.Reason)
	}
	policies := eventLines(readRunLog(t, layout), "on_fail_policy")
	want := [][]string{
		{"policy=escalate", "step=1", "outcome=not_done"},
		{"policy=split", "step=2", "outcome=no_op"},
		{"policy=block", "step=3", "outcome=blocked"},
	}
	if len(policies) != len(want) {
		t.Fatalf("on_fail_policy events = %v, want 3", policies)
	}
	for i, w := range want {
		for _, f := range w {
			if !strings.Contains(policies[i], f) {
				t.Errorf("event %d = %q lacks %q", i+1, policies[i], f)
			}
		}
	}
	// The split saw the escalated iteration's review too.
	if p := runner.prompts["preprocessor"]; len(p) != 1 || !strings.Contains(p[0], "001_REVIEW_3.md") {
		t.Errorf("preprocessor prompts = %v, want one naming 001_REVIEW_3.md", p)
	}
	report := readFile(t, filepath.Join(layout.SpecDir, "001_BLOCKED.md"))
	if !strings.Contains(report, "2. split: no_op") {
		t.Errorf("001_BLOCKED.md does not list the no-op split:\n%s", report)
	}
}

func TestOnFailSplitUnknownPreprocessorModelFailsRun(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &escalationRunner{layout: layout}
	cfg := onFailConfig([]string{onfail.Split, onfail.Block}, false)
	cfg.PreprocessorModel = agents.ModelOverride{Value: "nope", Source: agents.ModelSourceFlag}
	o := newTestOrchestrator(dir, layout, runner, cfg)
	if err := o.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Run = %v, want an unknown-model error", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("agents called despite the startup error: %v", runner.calls)
	}
}
