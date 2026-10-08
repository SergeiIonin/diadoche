package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/questions"
	"diadoche/internal/runlog"
	"diadoche/internal/runstate"
	"diadoche/internal/specs"
)

// The two questions 001's agent asks in these tests, unanswered.
const askedQuestions = `# Questions

## Question 001-1: Where to store the data
Task: 001_example.md
Asked-by: planner
Question: Store the data in memory or on disk?
Options:
- A: memory
- B: disk
Default: A
Why: simpler
Reversible: no
Answer:

## Question 001-2: Name of the flag
Task: 001_example.md
Asked-by: planner
Question: Call the flag --fast or --quick?
Options:
- A: --fast
- B: --quick
Default: A
Why: shorter
Reversible: no
Answer:
`

const theAnswer = "B, on disk: it must survive restarts."

// askingRunner is a taskRunner whose agent (planner or worker) of 001
// writes askedQuestions to QUESTIONS.md and answers NEEDS_HUMAN on its
// first call; onAsk, when set, runs first (e.g. to change files).
func askingRunner(layout specs.Layout, agent string, onAsk func(dir string) error) *taskRunner {
	r := &taskRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, n int) (*agents.RunResult, error) {
		if c.agent != agent || c.spec != "001_example.md" || n != 1 {
			return nil, nil
		}
		if agent == "worker" && onAsk == nil {
			return nil, nil
		}
		if onAsk != nil {
			if err := onAsk(layout.SpecDir); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(layout.QuestionsPath, []byte(askedQuestions), 0o644); err != nil {
			return nil, err
		}
		return &agents.RunResult{Output: "I can't decide where the data lives; asked in QUESTIONS.md.\n\nNEEDS_HUMAN\n"}, nil
	}
	return r
}

// answerFirstQuestion writes theAnswer to the first entry of QUESTIONS.md
// and leaves the second unanswered.
func answerFirstQuestion(t *testing.T, layout specs.Layout) {
	t.Helper()
	content := strings.Replace(readFile(t, layout.QuestionsPath), "Answer:\n", "Answer: "+theAnswer+"\n", 1)
	writeFile(t, layout.QuestionsPath, content)
}

// allRunLogs is every run's run.log, in run order.
func allRunLogs(t *testing.T, layout specs.Layout) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(layout.Root, "runs", "*", runlog.LogName))
	var b strings.Builder
	for _, m := range matches {
		b.WriteString(readFile(t, m))
	}
	return b.String()
}

func promptsOf(r *taskRunner, spec, agent string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for i, c := range r.calls {
		if c.spec == spec && c.agent == agent {
			out = append(out, r.prompts[i])
		}
	}
	return out
}

// runWaiting runs 001 (whose planner asks) and 002 once, in worktree mode.
func runWaiting(t *testing.T) (string, specs.Layout, *taskRunner, string) {
	t.Helper()
	dir, layout := twoTaskRepo(t)
	r := askingRunner(layout, "planner", nil)
	o, logBuf := bufferedOrchestrator(dir, layout, r, config.Config{Iter: 2, Worktree: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("Run = %v, want nil: a WAITING task is no reason to stop", err)
	}
	return dir, layout, r, logBuf.String()
}

func TestNeedsHumanPutsTaskWaitingWithoutSpendingAnIteration(t *testing.T) {
	dir, layout, r, logText := runWaiting(t)
	head := gitOut(t, dir, "rev-list", "--max-parents=0", "HEAD")

	row := statusRow(t, layout, "001_example.md")
	if row.Status != specs.StatusWaiting || !strings.Contains(row.Reason, "QUESTIONS.md") {
		t.Errorf("001 row = %+v, want WAITING with a reason pointing at QUESTIONS.md", row)
	}
	if got := callList(r.calls); got != "001:planner 002:planner 002:worker 002:tester" {
		t.Errorf("calls = %s: 001 must stop after its planner and the run go on with 002", got)
	}
	if fileExists(filepath.Join(layout.SpecDir, "001_REVIEW_1.md")) {
		t.Error("001_REVIEW_1.md written: asking a human spends no iteration")
	}
	if strings.Contains(logText, "001_example.md iteration 2/") {
		t.Errorf("001 went on to iteration 2:\n%s", logText)
	}
	if got := statusRow(t, layout, "002_other.md").Status; got != specs.StatusDone {
		t.Errorf("002 = %s, want DONE: the run continues with the next task", got)
	}

	st := readState(t, layout)
	want := runstate.Task{Spec: "001_example.md", Num: "001", Iteration: 1, Agent: "planner", AgentState: runstate.AgentPending,
		BaseCommit: head, Worktree: ".claude/diadoche/worktrees/001-example"}
	if got, ok := st.Waiting["001_example.md"]; !ok || got != want {
		t.Errorf("waiting record = %+v (present %v), want %+v", got, ok, want)
	}
	if firstTask(st) != nil {
		t.Errorf("current task = %+v, want none", firstTask(st))
	}

	if !fileExists(filepath.Join(layout.Root, "worktrees", "001-example")) {
		t.Error("001's worktree was removed")
	}
	gitOut(t, dir, "rev-parse", "--verify", "diadoche/001-example")
	if !fileExists(filepath.Join(layout.SpecDir, "001_BASE")) {
		t.Error("001_BASE removed: the resumed task needs its base")
	}
	report := readFile(t, filepath.Join(layout.SpecDir, "001_WAITING.md"))
	for _, want := range []string{"WAITING", "Asked-by: planner", "Iteration: 1", "001-1", "001-2", "--resume-waiting"} {
		if !strings.Contains(report, want) {
			t.Errorf("001_WAITING.md lacks %q:\n%s", want, report)
		}
	}
	runLog := readRunLog(t, layout)
	requireEvent(t, runLog, "waiting", "agent=planner", "open=2")
	requireEvent(t, runLog, "status", "status=WAITING", "reason=needs_human")
	if !strings.Contains(gitOut(t, dir, "log", "--format=%s"), "diadoche: WAITING 001_example.md") {
		t.Error("no WAITING bookkeeping commit")
	}
	if st := gitOut(t, dir, "status", "--porcelain", "--", "QUESTIONS.md"); st != "" {
		t.Errorf("QUESTIONS.md not committed with the bookkeeping: %s", st)
	}
}

func TestWaitingTaskIsNotPickedUpAgain(t *testing.T) {
	dir, layout, _, _ := runWaiting(t)
	stateBefore := readState(t, layout).Waiting

	if queue, err := specs.PendingQueue(layout); err != nil || len(queue) != 0 {
		t.Errorf("PendingQueue = %v, %v; want nothing pending", queue, err)
	}
	for _, cfg := range []config.Config{{Worktree: true}, {Worktree: true, Fresh: true}} {
		r := &taskRunner{layout: layout}
		o := newTestOrchestrator(dir, layout, r, cfg)
		if err := o.Run(context.Background()); err != nil {
			t.Fatalf("Run (fresh=%v): %v", cfg.Fresh, err)
		}
		if len(r.calls) != 0 {
			t.Errorf("fresh=%v: calls = %s, want none", cfg.Fresh, callList(r.calls))
		}
		if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusWaiting {
			t.Errorf("fresh=%v: 001 = %s, want WAITING", cfg.Fresh, got)
		}
		if got := readState(t, layout).Waiting["001_example.md"]; got != stateBefore["001_example.md"] {
			t.Errorf("fresh=%v: waiting record = %+v, want it kept as %+v", cfg.Fresh, got, stateBefore["001_example.md"])
		}
	}
}

func TestResumeWaitingPassesQuestionsAndAnswersToPlanner(t *testing.T) {
	dir, layout, _, _ := runWaiting(t)
	answerFirstQuestion(t, layout)

	r := &taskRunner{layout: layout}
	o, logBuf := bufferedOrchestrator(dir, layout, r, config.Config{Iter: 2, Worktree: true, ResumeWaiting: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	planner := promptsOf(r, "001_example.md", "planner")
	if len(planner) != 1 {
		t.Fatalf("001 planner calls = %d, want 1", len(planner))
	}
	for _, want := range []string{"Store the data in memory or on disk?", "Call the flag --fast or --quick?", "Answer: " + theAnswer, questions.UnansweredLabel, layout.QuestionsPath} {
		if !strings.Contains(planner[0], want) {
			t.Errorf("planner prompt lacks %q:\n%s", want, planner[0])
		}
	}
	if strings.Count(planner[0], questions.UnansweredLabel) != 1 {
		t.Errorf("want exactly one unanswered entry in the planner prompt:\n%s", planner[0])
	}
	if worker := promptsOf(r, "001_example.md", "worker"); len(worker) != 1 || !strings.Contains(worker[0], theAnswer) {
		t.Errorf("worker prompt lacks the answer: %q", worker)
	}
	if tester := promptsOf(r, "001_example.md", "tester"); len(tester) != 1 || !strings.Contains(tester[0], "001_REVIEW_1.md") {
		t.Errorf("001 didn't resume in iteration 1: tester prompts %q", tester)
	}
	if !strings.Contains(logBuf.String(), "001_example.md iteration 1/") {
		t.Errorf("log lacks iteration 1:\n%s", logBuf)
	}
	if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusDone {
		t.Errorf("001 = %s, want DONE", got)
	}
	requireEvent(t, allRunLogs(t, layout), "resume_waiting", "questions=2", "answered=1")
	if fileExists(filepath.Join(layout.SpecDir, "001_WAITING.md")) {
		t.Error("001_WAITING.md still there after the task resumed")
	}
	if _, ok := readState(t, layout).Waiting["001_example.md"]; ok {
		t.Error("the waiting record outlived the resume")
	}
}

func TestWorkerNeedsHumanResumesAtWorker(t *testing.T) {
	dir, layout := seedRepo(t)
	r := askingRunner(layout, "worker", func(string) error { return nil })
	if err := newTestOrchestrator(dir, layout, r, config.Config{Worktree: true}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusWaiting {
		t.Fatalf("001 = %s, want WAITING", got)
	}
	rec := readState(t, layout).Waiting["001_example.md"]
	if rec.Iteration != 1 || rec.Agent != "planner" || rec.AgentState != runstate.AgentDone {
		t.Errorf("waiting record = %+v, want iteration 1, planner done", rec)
	}
	if r.count("tester") != 0 {
		t.Error("tester ran after worker asked a human")
	}

	answerFirstQuestion(t, layout)
	r2 := &taskRunner{layout: layout}
	if err := newTestOrchestrator(dir, layout, r2, config.Config{Worktree: true, ResumeWaiting: true}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := callList(r2.calls); got != "001:worker 001:tester" {
		t.Errorf("calls = %s, want worker then tester (the plan is reused)", got)
	}
	if w := promptsOf(r2, "001_example.md", "worker"); len(w) != 1 || !strings.Contains(w[0], "Answer: "+theAnswer) {
		t.Errorf("worker prompt lacks the answer: %q", w)
	}
	if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusDone {
		t.Errorf("001 = %s, want DONE", got)
	}
}

func TestInlineNeedsHumanMentionDoesNotWait(t *testing.T) {
	dir, layout := seedRepo(t)
	r := &taskRunner{layout: layout}
	r.before = func(_ context.Context, c stepCall, _ int) (*agents.RunResult, error) {
		if c.agent != "planner" {
			return nil, nil
		}
		err := os.WriteFile(filepath.Join(layout.SpecDir, "001_IMPL_PLAN.md"), []byte(validPlanFixture), 0o644)
		return &agents.RunResult{Output: "Not a blocker, so no NEEDS_HUMAN marker is needed.\n`NEEDS_HUMAN`\n"}, err
	}
	if err := newTestOrchestrator(dir, layout, r, config.Config{Worktree: true}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusDone {
		t.Errorf("001 = %s, want DONE: an inline mention is not the marker", got)
	}
	if got := callList(r.calls); got != "001:planner 001:worker 001:tester" {
		t.Errorf("calls = %s", got)
	}
	if fileExists(filepath.Join(layout.SpecDir, "001_WAITING.md")) {
		t.Error("001_WAITING.md written")
	}
}

func TestDependentOfWaitingTaskIsSkipped(t *testing.T) {
	dir, layout := seedRepo(t)
	writeFile(t, filepath.Join(layout.SpecDir, "002_other.md"), "# Other\nDepends-on: 001\n")
	r := askingRunner(layout, "planner", nil)
	o, logBuf := bufferedOrchestrator(dir, layout, r, config.Config{Worktree: true})
	if err := o.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := callList(r.calls); got != "001:planner" {
		t.Errorf("calls = %s, want only 001's planner", got)
	}
	if got := statusRow(t, layout, "002_other.md").Status; got != specs.StatusQueued {
		t.Errorf("002 = %s, want QUEUED (not BLOCKED): it runs once 001 is resumed", got)
	}
	if !strings.Contains(logBuf.String(), "skipping 002_other.md: it depends on 001_example.md, which is WAITING") {
		t.Errorf("log lacks the skip:\n%s", logBuf)
	}
	requireEvent(t, readRunLog(t, layout), "skipped", "waiting_on=001_example.md")
}

func TestWaitingInPlaceParksAndRestoresChanges(t *testing.T) {
	dir, layout := seedRepo(t)
	const parked = "parked work\n"
	r := askingRunner(layout, "worker", func(string) error {
		return os.WriteFile(filepath.Join(dir, "out-001.txt"), []byte(parked), 0o644)
	})
	if err := newTestOrchestrator(dir, layout, r, config.Config{Worktree: false}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusWaiting {
		t.Fatalf("001 = %s, want WAITING", got)
	}
	if fileExists(filepath.Join(dir, "out-001.txt")) {
		t.Error("out-001.txt still in the tree: the changes must be parked")
	}
	gitOut(t, dir, "rev-parse", "--verify", "diadoche/waiting/001-example")
	if fileExists(filepath.Join(layout.SpecDir, "001_BASE")) {
		t.Error("001_BASE kept in place")
	}
	if !strings.Contains(readFile(t, filepath.Join(layout.SpecDir, "001_WAITING.md")), "Parked-branch: diadoche/waiting/001-example") {
		t.Error("001_WAITING.md lacks the Parked-branch line")
	}
	if st := gitOut(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("tree not clean after WAITING:\n%s", st)
	}

	answerFirstQuestion(t, layout)
	var seen string
	r2 := &taskRunner{layout: layout}
	r2.before = func(_ context.Context, c stepCall, _ int) (*agents.RunResult, error) {
		if c.agent == "worker" {
			b, _ := os.ReadFile(filepath.Join(dir, "out-001.txt"))
			seen = string(b)
		}
		return nil, nil
	}
	if err := newTestOrchestrator(dir, layout, r2, config.Config{Worktree: false, ResumeWaiting: true}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen != parked {
		t.Errorf("worker saw out-001.txt = %q, want the parked %q restored", seen, parked)
	}
	if got := callList(r2.calls); got != "001:worker 001:tester" {
		t.Errorf("calls = %s, want worker then tester", got)
	}
	if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusDone {
		t.Errorf("001 = %s, want DONE", got)
	}
	requireEvent(t, allRunLogs(t, layout), "waiting_restored", "branch=diadoche/waiting/001-example")
}

func TestEscalatedNeedsHumanWaits(t *testing.T) {
	dir, layout := seedRepo(t)
	r := &taskRunner{layout: layout, testerNotDone: true}
	r.before = func(_ context.Context, c stepCall, n int) (*agents.RunResult, error) {
		// The escalated iteration's planner is the third planner call.
		if c.agent != "planner" || n != 3 {
			return nil, nil
		}
		return &agents.RunResult{Output: "NEEDS_HUMAN"}, os.WriteFile(layout.QuestionsPath, []byte(askedQuestions), 0o644)
	}
	cfg := config.Config{Iter: 2, Worktree: true, OnFail: []string{"escalate", "block"}}
	if err := newTestOrchestrator(dir, layout, r, cfg).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := statusRow(t, layout, "001_example.md").Status; got != specs.StatusWaiting {
		t.Errorf("001 = %s, want WAITING, not BLOCKED", got)
	}
	if rec := readState(t, layout).Waiting["001_example.md"]; rec.Iteration != 3 || rec.Agent != "planner" {
		t.Errorf("waiting record = %+v, want the escalated iteration 3's planner", rec)
	}
}
