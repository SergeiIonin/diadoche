package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/onfail"
	"diadoche/internal/runstate"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
)

const testEscalationModel = "claude-fable-5-1"

// escCall is one agent call escalationRunner saw.
type escCall struct {
	agent, model, spec string
}

// escalationRunner is a fake claude for the --on-fail tests. It works on
// whichever task is IN_PROGRESS: planner writes a valid plan, worker
// writes edit.txt (with a counter) in its work dir, tester writes the
// review its prompt asks for. Tester marks the task DONE only when the
// last worker call ran on approveModel, or when approve lists the spec.
type escalationRunner struct {
	layout          specs.Layout
	approveModel    string
	approve         map[string]bool
	calls           []escCall
	lastWorkerModel string
}

var reviewPathRe = regexp.MustCompile(`(\d+_REVIEW_\d+\.md)`)

func (r *escalationRunner) Run(_ context.Context, dir string, a *agents.Agent, prompt string) (agents.RunResult, error) {
	rows, err := specs.ReadStatus(r.layout.StatusPath)
	if err != nil {
		return agents.RunResult{}, err
	}
	spec := ""
	for _, row := range rows {
		if row.Status == specs.StatusInProgress {
			spec = row.Spec
		}
	}
	if spec == "" {
		return agents.RunResult{}, fmt.Errorf("no task IN_PROGRESS")
	}
	r.calls = append(r.calls, escCall{agent: a.Name, model: a.Model, spec: spec})
	num := specs.TaskNumber(spec)
	result := agents.RunResult{Parsed: true, Stdout: a.Name + " on " + a.Model + "\n"}
	switch a.Name {
	case "planner":
		err = os.WriteFile(filepath.Join(r.layout.SpecDir, num+"_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644)
	case "worker":
		r.lastWorkerModel = a.Model
		err = os.WriteFile(filepath.Join(dir, "edit.txt"), fmt.Appendf(nil, "edited by worker %d on %s\n", len(r.calls), a.Model), 0o644)
	case "tester":
		m := reviewPathRe.FindStringSubmatch(prompt)
		if m == nil {
			return result, fmt.Errorf("tester prompt names no review file")
		}
		if err = os.WriteFile(filepath.Join(r.layout.SpecDir, m[1]), []byte("review: not done yet\n"), 0o644); err != nil {
			return result, err
		}
		if r.approve[spec] || (r.approveModel != "" && r.lastWorkerModel == r.approveModel) {
			err = specs.SetStatus(r.layout, spec, specs.StatusDone)
		}
	}
	return result, err
}

// callsOf returns the models of agent's calls, in order.
func (r *escalationRunner) callsOf(agent string) []string {
	var out []string
	for _, c := range r.calls {
		if c.agent == agent {
			out = append(out, c.model)
		}
	}
	return out
}

func onFailConfig(chain []string, worktree bool) config.Config {
	return config.Config{
		Iter:            2,
		Worktree:        worktree,
		OnFail:          chain,
		EscalationModel: agents.ModelOverride{Value: testEscalationModel, Source: agents.ModelSourceFlag},
	}
}

// taskArtifactDir is the run directory of task 001_example.md.
func taskArtifactDir(t *testing.T, layout specs.Layout) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(layout.Root, "runs", "*", "001-example"))
	if len(matches) != 1 {
		t.Fatalf("task artifact dirs: %v", matches)
	}
	return matches[0]
}

func readUsage(t *testing.T, layout specs.Layout) []usage.Record {
	t.Helper()
	f, err := os.Open(filepath.Join(layout.Root, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []usage.Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec usage.Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
	}
	return out
}

func taskRow(t *testing.T, layout specs.Layout, spec string) specs.Row {
	t.Helper()
	rows, err := specs.ReadStatus(layout.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Spec == spec {
			return r
		}
	}
	t.Fatalf("no STATUS.md row for %s", spec)
	return specs.Row{}
}

func TestOnFailEscalateFinishesTaskDone(t *testing.T) {
	for _, worktree := range []bool{false, true} {
		t.Run(fmt.Sprintf("worktree=%v", worktree), func(t *testing.T) {
			dir, layout := seedRepo(t)
			runner := &escalationRunner{layout: layout, approveModel: testEscalationModel}
			o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Escalate}, worktree))
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			row := taskRow(t, layout, "001_example.md")
			if row.Status != specs.StatusDone {
				t.Fatalf("status = %s: %s, want DONE", row.Status, row.Reason)
			}
			if !strings.Contains(row.Reason, "escalate") || !strings.Contains(row.Reason, testEscalationModel) {
				t.Errorf("DONE reason = %q, want it to name the escalation", row.Reason)
			}
			for _, agent := range []string{"planner", "worker"} {
				want := []string{testAgentModel, testAgentModel, testEscalationModel}
				if got := runner.callsOf(agent); !slices.Equal(got, want) {
					t.Errorf("%s models = %v, want %v", agent, got, want)
				}
			}
			if got := runner.callsOf("tester"); !slices.Equal(got, []string{testAgentModel, testAgentModel, testAgentModel}) {
				t.Errorf("tester models = %v, tester must keep its own model", got)
			}

			if !fileExists(filepath.Join(layout.SpecDir, "001_REVIEW_3.md")) {
				t.Error("no review of the escalated iteration (001_REVIEW_3.md)")
			}
			art := taskArtifactDir(t, layout)
			for _, f := range []string{"iter-3-IMPL_PLAN.md", "iter-3-REVIEW.md", "iter-3-planner.stdout", "iter-3-worker.stdout", "iter-3-tester.stdout"} {
				if !fileExists(filepath.Join(art, f)) {
					t.Errorf("artifact %s missing", f)
				}
			}
			if got := readFile(t, filepath.Join(art, "iter-3-planner.stdout")); !strings.Contains(got, testEscalationModel) {
				t.Errorf("iter-3-planner.stdout = %q, want the escalated call's output", got)
			}

			escalatedUsage := map[string]string{}
			for _, rec := range readUsage(t, layout) {
				if rec.Iteration == 3 {
					escalatedUsage[rec.Agent] = rec.Model
				}
			}
			if escalatedUsage["planner"] != testEscalationModel || escalatedUsage["worker"] != testEscalationModel || escalatedUsage["tester"] != testAgentModel {
				t.Errorf("usage.jsonl iteration 3 models = %v", escalatedUsage)
			}

			log := readRunLog(t, layout)
			requireEvent(t, log, "escalation_start", "iter=3", "model="+testEscalationModel)
			requireEvent(t, log, "agent_start", "iter=3", "agent=planner", "escalated=true")
			requireEvent(t, log, "verdict", "iter=3", "verdict=DONE", "escalated=true")
			if got := eventLines(log, "on_fail_policy"); len(got) != 1 || !strings.Contains(got[0], "policy=escalate") || !strings.Contains(got[0], "outcome=done") {
				t.Errorf("on_fail_policy events = %v, want exactly one escalate/done", got)
			}
			if len(eventLines(log, "agent_start", "iter=1", "escalated=true")) != 0 {
				t.Error("a regular iteration is marked escalated")
			}

			if worktree {
				requireEvent(t, log, "merge")
				if got := gitOut(t, dir, "show", "HEAD:edit.txt"); !strings.Contains(got, testEscalationModel) {
					t.Errorf("merged edit.txt = %q, want the escalated worker's change", got)
				}
			} else if subjects := gitOut(t, dir, "log", "--format=%s"); !strings.Contains(subjects, "diadoche: complete 001_example.md") {
				t.Errorf("task not committed; git log:\n%s", subjects)
			}
			for _, f := range []string{"001_BLOCKED.md", "001_FAILED.md"} {
				if fileExists(filepath.Join(layout.SpecDir, f)) {
					t.Errorf("%s written for a DONE task", f)
				}
			}
		})
	}
}

func TestOnFailBlockEndsBlockedAndIsNotRetried(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &escalationRunner{layout: layout}
	o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Block}, true))
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	row := taskRow(t, layout, "001_example.md")
	if row.Status != specs.StatusBlocked {
		t.Fatalf("status = %s, want BLOCKED", row.Status)
	}
	if !strings.Contains(row.Reason, "block") || !strings.Contains(row.Reason, "2 iteration(s)") {
		t.Errorf("BLOCKED reason = %q, want it to name the policies applied", row.Reason)
	}
	if got := len(runner.callsOf("planner")); got != 2 {
		t.Errorf("planner calls = %d, want --ITER (2): block runs no extra iteration", got)
	}
	report := readFile(t, filepath.Join(layout.SpecDir, "001_BLOCKED.md"))
	for _, want := range []string{"BLOCKED", "block", "diadoche/001-example", "001_REVIEW_2.md", "QUEUED"} {
		if !strings.Contains(report, want) {
			t.Errorf("001_BLOCKED.md lacks %q:\n%s", want, report)
		}
	}
	if fileExists(filepath.Join(layout.SpecDir, "001_FAILED.md")) {
		t.Error("001_FAILED.md written for a BLOCKED task")
	}

	// The worktree and branch are kept, with the worker's change committed.
	if !fileExists(filepath.Join(layout.Root, "worktrees", "001-example")) {
		t.Error("the BLOCKED task's worktree was removed")
	}
	if got := gitOut(t, dir, "show", "diadoche/001-example:edit.txt"); !strings.HasPrefix(got, "edited by worker") {
		t.Errorf("edit.txt on the kept branch = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edit.txt original\n" {
		t.Errorf("main tree edit.txt = %q; nothing may be merged", got)
	}

	log := readRunLog(t, layout)
	requireEvent(t, log, "on_fail_policy", "policy=block", "outcome=blocked")
	requireEvent(t, log, "status", "status=BLOCKED", "reason=on_fail")
	requireEvent(t, log, "task_end", "status=BLOCKED")
	if len(eventLines(log, "park")) != 0 || len(eventLines(log, "merge")) != 0 {
		t.Error("a BLOCKED worktree task must be neither parked nor merged")
	}

	saved, err := runstate.Load(runstate.Path(layout.Root))
	if err != nil || saved == nil || saved.Task != nil {
		t.Fatalf("run-state.json = %+v, %v; want no task", saved, err)
	}

	// A stale run-state.json pointing at the BLOCKED task must not bring
	// it back either.
	stale := runstate.State{RunID: "stale", Task: &runstate.Task{
		Spec: "001_example.md", Num: "001", Iteration: 1, Agent: "planner", AgentState: runstate.AgentRunning,
	}}
	if err := runstate.Save(runstate.Path(layout.Root), stale); err != nil {
		t.Fatal(err)
	}
	again := &escalationRunner{layout: layout}
	o2 := newTestOrchestrator(dir, layout, again, onFailConfig([]string{onfail.Block}, true))
	if err := o2.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(again.calls) != 0 {
		t.Errorf("the second run called agents %v; a BLOCKED task is never retried", again.calls)
	}
	if got := taskRow(t, layout, "001_example.md"); got.Status != specs.StatusBlocked || got.Reason != row.Reason {
		t.Errorf("after the second run: %s: %s, want unchanged BLOCKED: %s", got.Status, got.Reason, row.Reason)
	}
}

func TestOnFailBlockInPlaceParksOnBlockedBranch(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &escalationRunner{layout: layout}
	o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Block}, false))
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if row := taskRow(t, layout, "001_example.md"); row.Status != specs.StatusBlocked {
		t.Fatalf("status = %s, want BLOCKED", row.Status)
	}
	if got := gitOut(t, dir, "show", "diadoche/blocked/001-example:edit.txt"); !strings.HasPrefix(got, "edited by worker") {
		t.Errorf("edit.txt on diadoche/blocked/001-example = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "edit.txt")); got != "edit.txt original\n" {
		t.Errorf("working tree edit.txt = %q, want it reverted", got)
	}
	if fileExists(filepath.Join(layout.SpecDir, "001_FAILED.md")) {
		t.Error("001_FAILED.md written for a BLOCKED task")
	}
	if branches := gitOut(t, dir, "branch", "--list", "diadoche/failed/*"); branches != "" {
		t.Errorf("unexpected failed branches: %s", branches)
	}
	report := readFile(t, filepath.Join(layout.SpecDir, "001_BLOCKED.md"))
	if !strings.Contains(report, "diadoche/blocked/001-example") || !strings.Contains(report, "git checkout") {
		t.Errorf("001_BLOCKED.md does not say where the changes are:\n%s", report)
	}
	if subjects := gitOut(t, dir, "log", "--format=%s"); !strings.Contains(subjects, "diadoche: BLOCKED 001_example.md") {
		t.Errorf("no bookkeeping commit; git log:\n%s", subjects)
	}
	if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("working tree not clean after BLOCKED:\n%s", status)
	}
}

func TestOnFailChainEscalateThenBlock(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &escalationRunner{layout: layout}
	o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Escalate, onfail.Block}, true))
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	row := taskRow(t, layout, "001_example.md")
	if row.Status != specs.StatusBlocked {
		t.Fatalf("status = %s, want BLOCKED (not FAILED)", row.Status)
	}
	if !strings.Contains(row.Reason, "escalate, block") {
		t.Errorf("BLOCKED reason = %q, want both policies", row.Reason)
	}

	log := readRunLog(t, layout)
	policies := eventLines(log, "on_fail_policy")
	if len(policies) != 2 {
		t.Fatalf("on_fail_policy events = %v, want 2", policies)
	}
	if !strings.Contains(policies[0], "policy=escalate") || !strings.Contains(policies[0], "outcome=not_done") || !strings.Contains(policies[0], "step=1") {
		t.Errorf("first policy event = %q, want escalate/not_done", policies[0])
	}
	if !strings.Contains(policies[1], "policy=block") || !strings.Contains(policies[1], "outcome=blocked") || !strings.Contains(policies[1], "step=2") {
		t.Errorf("second policy event = %q, want block/blocked", policies[1])
	}
	requireEvent(t, log, "verdict", "iter=3", "verdict=NOT_DONE", "escalated=true")

	art := taskArtifactDir(t, layout)
	for _, f := range []string{"iter-3-IMPL_PLAN.md", "iter-3-REVIEW.md", "iter-3-planner.stdout"} {
		if !fileExists(filepath.Join(art, f)) {
			t.Errorf("artifact %s missing", f)
		}
	}
	report := readFile(t, filepath.Join(layout.SpecDir, "001_BLOCKED.md"))
	esc, blk := strings.Index(report, "1. escalate: not_done"), strings.Index(report, "2. block: blocked")
	if esc < 0 || blk < esc {
		t.Errorf("001_BLOCKED.md does not list escalate then block:\n%s", report)
	}
	if !strings.Contains(report, "escalated iteration 3") || !strings.Contains(report, "001_REVIEW_3.md") {
		t.Errorf("001_BLOCKED.md does not mention the escalated iteration and its review:\n%s", report)
	}
	if fileExists(filepath.Join(layout.SpecDir, "001_FAILED.md")) {
		t.Error("001_FAILED.md written for a BLOCKED task")
	}
}

func TestOnFailBlockCascadesToDependsOn(t *testing.T) {
	dir, layout := seedRepo(t)
	writeFile(t, filepath.Join(layout.SpecDir, "002_dep.md"), "# Dep\n\nDepends-on: 001\n")
	writeFile(t, filepath.Join(layout.SpecDir, "003_free.md"), "# Free\n")
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-m", "more specs")

	runner := &escalationRunner{layout: layout, approve: map[string]bool{"003_free.md": true, "002_dep.md": true}}
	o := newTestOrchestrator(dir, layout, runner, onFailConfig([]string{onfail.Block}, true))
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if row := taskRow(t, layout, "001_example.md"); row.Status != specs.StatusBlocked {
		t.Fatalf("001 status = %s, want BLOCKED", row.Status)
	}
	dep := taskRow(t, layout, "002_dep.md")
	if dep.Status != specs.StatusBlocked || !strings.Contains(dep.Reason, "001_example.md") {
		t.Errorf("002 = %s: %q, want BLOCKED naming 001_example.md", dep.Status, dep.Reason)
	}
	if free := taskRow(t, layout, "003_free.md"); free.Status != specs.StatusDone || free.Reason != "" {
		t.Errorf("003 = %s: %q, want plain DONE", free.Status, free.Reason)
	}
	for _, c := range runner.calls {
		if c.spec == "002_dep.md" {
			t.Errorf("agent %s was called for the dependency-BLOCKED 002_dep.md", c.agent)
		}
	}
	if !slices.ContainsFunc(runner.calls, func(c escCall) bool { return c.spec == "003_free.md" }) {
		t.Error("003_free.md was never processed")
	}

	log := readRunLog(t, layout)
	requireEvent(t, log, "status", "task=002_dep.md", "status=BLOCKED", "reason=dependency", "blocked_by=001_example.md")
	report := readFile(t, filepath.Join(layout.SpecDir, "001_BLOCKED.md"))
	if !strings.Contains(report, "002_dep.md") {
		t.Errorf("001_BLOCKED.md does not list the dependant:\n%s", report)
	}
}

func TestOnFailEmptyChainIsTodaysFailed(t *testing.T) {
	for name, chain := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			dir, layout := seedRepo(t)
			runner := &escalationRunner{layout: layout}
			o := newTestOrchestrator(dir, layout, runner, onFailConfig(chain, false))
			if err := o.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if row := taskRow(t, layout, "001_example.md"); row.Status != specs.StatusFailed || row.Reason != "" {
				t.Fatalf("status = %s: %q, want plain FAILED", row.Status, row.Reason)
			}
			if got := readFile(t, filepath.Join(layout.SpecDir, "001_FAILED.md")); !strings.Contains(got, "not DONE after 2 iteration(s)") || !strings.Contains(got, parkedBranch) {
				t.Errorf("001_FAILED.md:\n%s", got)
			}
			if got := gitOut(t, dir, "show", parkedBranch+":edit.txt"); !strings.HasPrefix(got, "edited by worker") {
				t.Errorf("edit.txt on %s = %q", parkedBranch, got)
			}
			if fileExists(filepath.Join(layout.SpecDir, "001_BLOCKED.md")) {
				t.Error("001_BLOCKED.md written with an empty chain")
			}
			if got := len(runner.callsOf("planner")); got != 2 {
				t.Errorf("planner calls = %d, want exactly --ITER (2)", got)
			}
			log := readRunLog(t, layout)
			if len(eventLines(log, "on_fail_policy")) != 0 || len(eventLines(log, "escalation_start")) != 0 {
				t.Error("an empty chain applied a policy")
			}
			requireEvent(t, log, "task_end", "status=FAILED")
		})
	}
}

func TestFailPoliciesRegistryCoversAllNames(t *testing.T) {
	for _, name := range onfail.Names {
		if failPolicies[name] == nil {
			t.Errorf("policy %q has no implementation in failPolicies", name)
		}
	}
	for name := range failPolicies {
		if !slices.Contains(onfail.Names, name) {
			t.Errorf("failPolicies has %q, which onfail.Names doesn't list", name)
		}
	}
}

func TestOnFailUnknownEscalationModelFailsRun(t *testing.T) {
	dir, layout := seedRepo(t)
	runner := &escalationRunner{layout: layout}
	cfg := onFailConfig([]string{onfail.Escalate}, false)
	cfg.EscalationModel = agents.ModelOverride{Value: "nope", Source: agents.ModelSourceFlag}
	o := newTestOrchestrator(dir, layout, runner, cfg)
	err := o.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), testEscalationModel) {
		t.Fatalf("Run = %v, want an unknown-model error listing the known models", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("agents called despite the startup error: %v", runner.calls)
	}
}
