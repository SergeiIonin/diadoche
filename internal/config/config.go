// Package config parses diadoche's CLI flags.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"diadoche/internal/agents"
	"diadoche/internal/redgreen"
)

const (
	DefaultIter = 3
	MaxIter     = 8

	// DefaultMaxDiffBytes bounds the diff handed to tester — the raw
	// `git diff` plus the contents of any new untracked text files. Above
	// it, tester instead gets `git diff --stat`, the list of untracked
	// files, the diff restricted to the plan's "## Files" (when that one
	// fits the limit) and an instruction to open the files it needs
	// itself. 200 KB, as required by 004_tester_gets_diff.md.
	DefaultMaxDiffBytes = 204_800
)

// ErrHelp is returned by Parse when the user asked for help (-h, -help or
// --help). The help text has already been printed to stdout by the time
// Parse returns it, so callers should just exit(0) without printing
// anything else — unlike every other error Parse can return.
var ErrHelp = flag.ErrHelp

const helpName = `NAME
    diadoche - autonomous planner/worker/tester loop over task specs

`

const helpSynopsis = `SYNOPSIS
    diadoche [--dir <path>] [--ITER <n>] [--langs <list>] [--frameworks <list>]
             [--dry-run] [--claude-bin <path>] [--max-diff-bytes <n>]
             [--tester-diff-limit <n>]
             [--planner-model <name>] [--worker-model <name>]
             [--tester-model <name>] [--max-call-minutes <n>]
             [--max-call-cost <usd>] [--max-cost-per-task <usd>]
             [--max-cost-per-run <usd>] [--allow-dirty]
             [--stable-system-prompt=<bool>] [--worktree=<bool>]
             [--red-green=<bool>] [--test-globs <list>]
    diadoche --preprocess --task <path> [--dir <path>] [--claude-bin <path>]
             [--preprocessor-model <name>]
    diadoche --initialize <project-path>
    diadoche -h | -help | --help

`

const helpDescription = `DESCRIPTION
    diadoche drives an autonomous planner -> worker -> tester agent cycle on
    top of independent "claude --dangerously-skip-permissions" processes,
    over task spec files found in <project>/.claude/diadoche/spec/
    (NNN_<slug>.md, e.g. 001_add-login.md).

    For each task pulled off the queue, diadoche runs up to --ITER rounds
    of planner (writes an implementation plan) -> worker (implements it) ->
    tester (reviews it and, once the task is genuinely done, marks it DONE
    in spec/STATUS.md). Once the tester marks a task DONE, diadoche commits
    the changes (and, with --worktree, merges them; see WORKTREES below)
    and moves on to the next task; if --ITER rounds run out
    without a DONE verdict, diadoche itself marks the task FAILED. Once the
    queue drains, diadoche checks spec/ for new task files once more before
    exiting, so tasks added concurrently (e.g. by --preprocess) are picked
    up without a manual restart.

    Agent prompts and models for planner/worker/tester/preprocessor are
    built into the diadoche binary, so it works in any project without
    extra setup. Drop a .claude/agents/<name>.md file (same frontmatter
    format) into a project to override the built-in prompt/model for that
    one agent; every other agent keeps using its built-in default.

    diadoche is safe to interrupt and restart at any point: task status
    lives in spec/STATUS.md and per-task progress (implementation plan,
    reviews) lives in spec/ itself, so a restarted run resumes the
    lowest-numbered pending task exactly where it left off, reusing any
    implementation plan and reviews already on disk instead of redoing
    completed work.

    spec/NNN_IMPL_PLAN.md must contain two sections, with these exact
    headings: "## Files" (every path to create or modify) and
    "## Test Command" (how to run this project's tests/linter). worker
    then works strictly from that file list instead of re-exploring the
    repository, and tester is given a git diff (see DIFF SCOPE below)
    plus that test command instead of the whole repository. If planner's
    output is missing either section, the iteration is treated as failed
    and planner is asked again, still within the same --ITER budget.

    Each agent call's model, token usage and cost (parsed from the claude
    CLI's --output-format json result) is appended as one JSON line to
    spec/../usage.jsonl (i.e. .claude/diadoche/usage.jsonl), and a summary
    (overall, by agent, by task) is printed once the run finishes. A line
    that couldn't be parsed is still recorded (zeroed) and logs a warning
    — usage accounting never fails the run.

    DIFF SCOPE: before a task's first iteration, diadoche records the
    current commit (or, pre-first-commit, git's empty-tree hash) as that
    task's diff base. Before each tester call it computes the diff (plus
    any untracked files) since that base. An empty diff means tester is
    not called at all — the iteration is immediately unsuccessful. What
    tester gets is the diff plus the contents of any new untracked text
    files (binary ones are listed by name and size only). If that is over
    --max-diff-bytes (alias --tester-diff-limit, default 200 KB) it is
    replaced with 'git diff --stat', the list of untracked files, the diff
    restricted to the files listed in "## Files" of the plan when that one
    fits the limit, the base commit, and an instruction for tester to open
    the files it needs itself.

    WORKTREES (--worktree, on by default): each task runs in its own git
    worktree, .claude/diadoche/worktrees/<NNN>-<slug>, on its own branch
    diadoche/<NNN>-<slug>, created from the current HEAD of the branch
    checked out in the main working tree. planner, worker and tester all
    run with that worktree as their working directory, and tester's diff is
    computed inside it; diadoche's bookkeeping (spec/, STATUS.md, QUEUE.md,
    usage.jsonl, plans, reviews) stays in the main working tree.
    .claude/diadoche/worktrees/ is added to .git/info/exclude.
      DONE: the task's changes are committed on its branch, with the
      task's heading (its first line, without "# " and the number) as the
      message, then merged into the current branch — a fast-forward when
      possible, otherwise a merge commit. The worktree and the branch are
      then removed.
      Merge conflict: the merge is aborted, the task becomes BLOCKED with
      the reason in STATUS.md ("BLOCKED: <reason>"), its worktree and
      branch are kept for a human to resolve, and the run moves on to the
      next task. BLOCKED tasks are never re-queued.
      FAILED: the task's changes are committed on its branch, the worktree
      is removed and the branch is renamed to
      diadoche/failed/<NNN>-<slug>; spec/<NNN>_FAILED.md names it.
    A usage-limit error pauses the task in place (see USAGE LIMITS); a run
    interrupted while a task is pending leaves its worktree in place, and
    the next run resumes the task in it. A repository without any commit yet has
    nothing to branch from, so such a run falls back to --worktree=false.

    FAILED TASKS (--worktree=false): every task starts from a clean working
    tree. When a task
    becomes FAILED (iterations exhausted, an agent error, --max-call-cost,
    --max-cost-per-task),
    its changes since the diff base (outside .claude/diadoche/ and
    .claude/agents/) are committed to a new branch
    diadoche/failed/<NNN>-<slug> without switching branches, then reverted
    in the working tree. The task's bookkeeping (plan, reviews, STATUS.md,
    ...) is committed to the current branch as "diadoche: FAILED <spec>",
    and spec/<NNN>_FAILED.md names the branch and how to bring the changes
    back. A usage-limit error is not a failure: it pauses the task in place
    (see USAGE LIMITS); nothing is parked, and a run stopped meanwhile
    leaves the task pending with its changes in place.

    PROMPT CACHING: every agent of a run is given the same shared prompt
    prefix — byte for byte — as its system prompt (claude's
    --append-system-prompt), with the role instructions and the task data
    following in the user turn, in that order. Consecutive turns therefore
    hit the API's prompt cache instead of paying for the common context
    again. The prefix text is built into the binary; drop a
    <project>/.claude/diadoche/prefix.md to override it, and keep anything
    that varies between calls (times, iteration numbers, task ids) out of
    it. --stable-system-prompt (on by default) additionally passes claude's
    --exclude-dynamic-system-prompt-sections, which moves per-machine
    sections out of the system prompt; turn it off for a claude CLI that
    doesn't know that flag. See docs/prompt-caching.md for what cannot be
    unified and why.

    RED-GREEN CHECK (--red-green, on by default): after worker and before
    tester, in the same iteration, diadoche checks that the task's tests
    actually test something. The test files are the files added or changed
    since the diff base that match *_test.go or one of --test-globs (when
    --test-globs is not given, globs are inferred from the plan's
    "## Test Command", e.g. pytest -> test_*.py, *_test.py). diadoche
    creates a temporary detached worktree at the base commit in the OS temp
    directory, copies only those test files into it, and runs the plan's
    Test Command there; the worktree is always removed afterwards, also
    when the command fails or times out. The task's own working tree is
    never touched.
      Tests pass on the base commit: they check nothing — the iteration is
      unsuccessful, tester is not called, and the review handed to the
      next planner says so and lists the test files (and test names).
      Tests fail or don't compile on the base commit: the normal red
      result, the iteration continues to tester.
      No test files in the diff: the iteration is unsuccessful ("worker
      wrote no tests"), unless the spec contains the line
      <!-- diadoche: no-tests -->, which skips the check.
      Test files that existed before the task were changed but are not in
      the plan's "## Files": tester's prompt gets an extra item asking it
      to check that those existing tests were not weakened.
    The check is skipped in --dry-run and for a repository without
    commits. --red-green=false disables it entirely.

    DIRTY TREE: with --worktree, the run refuses to start if the main
    working tree has uncommitted changes outside .claude/diadoche/ and
    .claude/agents/ (each task's changes are merged into the current
    branch, so it has to be clean); diadoche never stashes them — commit
    them first. With --worktree=false the same check runs before each task
    that starts from scratch (the changes would otherwise be parked with
    the task's own), and such files are never parked under --allow-dirty.
    --allow-dirty skips the check in both modes. A task resumed after a
    restart keeps its own changes and the diff base recorded in
    spec/<NNN>_BASE at its first start.

`

const helpModes = `MODES
    Normal mode (default)
        Runs the full planner -> worker -> tester loop described above
        until the task queue is empty and no new task specs have appeared.

    --preprocess (requires --task)
        Splits one raw, possibly multi-part task file into atomic
        spec/NNN_<slug>.md files and exits. Runs only a single
        preprocessor agent; never implements anything and never touches
        STATUS.md/QUEUE.md. The new tasks are picked up by the next
        regular run (one without --preprocess).

    --initialize <project-path>
        Scaffolds .claude/ inside <project-path>: an empty
        .claude/diadoche/spec/STATUS.md, spec/raw/ (input for
        --preprocess), .claude/skills/, a copy of each built-in agent
        prompt under .claude/agents/*.md and the built-in shared prompt
        prefix as .claude/diadoche/prefix.md, so they're easy to customize.
        Never overwrites a file that's already there, so it's safe to run
        again later (e.g. after upgrading diadoche). Does only this, then
        exits immediately — every other flag is ignored.

`

const helpModels = `MODEL SELECTION
    Each agent's model is resolved in this order, first match wins:
        1. --<agent>-model flag
        2. DIADOCHE_<AGENT>_MODEL environment variable
        3. model: field in the project's .claude/agents/<name>.md, if any
        4. the agent's built-in default

    This only ever picks the model; a .claude/agents/<name>.md prompt
    override (its body, not its model: field) still applies regardless of
    which of the above set the model. A project .claude/agents/<name>.md
    with no model: field keeps its prompt and takes the built-in default
    model. The resolved model and its source (flag, env, frontmatter or
    built-in default) for every agent are logged once at the start of the
    run. An unrecognized model name, or an explicitly empty one (an empty
    flag value, an env var set to the empty string, or a blank model:
    field), is a startup error, before any claude process runs.

`

const helpCallBudget = `PER-CALL BUDGET
    --max-call-minutes and --max-call-cost cap a single planner/worker/
    tester call, so one runaway agent (stuck exploring, or retrying its own
    nested claude invocations) can't quietly burn the whole account usage
    window before diadoche notices. Both are 0 (no limit) by default.

    --max-call-minutes kills the claude process once a call runs longer
    than this; the iteration is then treated as failed, same as any other
    agent error. This is the only one of the two that can actually stop
    money being spent mid-call.

    --max-call-cost is checked only after a call returns — cost is only
    known once the claude CLI's JSON result comes back, so this cannot
    preempt an in-flight call. If the call's cost exceeds it, the task is
    failed right away instead of continuing to the next agent/iteration,
    so you find out immediately rather than after the fact.

    BUDGETS: --max-cost-per-task and --max-cost-per-run cap the sum of the
    costs recorded in usage.jsonl, in USD; both are 0 (no limit) by
    default, are checked after every agent call, and are off in --dry-run.
    --max-cost-per-task counts every call of the current task, previous
    runs included; once exceeded the task becomes FAILED with the reason
    "budget exceeded: ..." in STATUS.md and is parked like any other
    failed task. --max-cost-per-run counts this run's calls only; once
    exceeded diadoche stops after the current agent call, the current task
    stays pending (not FAILED) with its changes in place, the reason is
    logged, and diadoche exits non-zero. A task the tester already marked
    DONE in that call is still finished (committed/merged) first.

`

const helpUsageLimits = `USAGE LIMITS
    An agent call refused by a claude usage limit (subscription window
    exhausted) or by rate limiting (HTTP 429) - recognized by the call's
    exit code plus the error text / JSON result - does not spend an
    iteration. The task becomes "PAUSED: claude <kind>, resumes at <time>"
    in STATUS.md (without the time when the reply doesn't carry one), and
    diadoche waits: until the reset time from the reply ("resets 7pm",
    legacy "usage limit reached|<unix time>"), else 5 minutes, doubling on
    each further time-less pause up to 1 hour. Then the same call of the
    same agent in the same iteration is retried; after a successful call
    the task is IN_PROGRESS again and the backoff starts over at 5 minutes.
    SIGINT/SIGTERM interrupts the wait at once: the task stays PAUSED and
    is resumed (like IN_PROGRESS) by the next run.

    diadoche-restarter works alongside: diadoche's own pause log lines are
    never claude limit messages and the claude error text of a refused call
    is not printed, so the restarter does not kill a pausing diadoche; it
    remains a fallback for crashes and limit errors that escape the pause.

`

const helpFlagsHeader = `FLAGS

`

const helpFiles = `
FILES
    <project>/.claude/diadoche/spec/STATUS.md
        Source of truth for every task's status: QUEUED, IN_PROGRESS,
        PAUSED, DONE, FAILED or BLOCKED. A BLOCKED or PAUSED row (and a
        task FAILED by its budget) carries its reason in the Status cell
        ("BLOCKED: <reason>").

    <project>/.claude/diadoche/spec/QUEUE.md
        This run's work queue, rebuilt from STATUS.md on every sync. A
        trailing " [no-tests]" on a line is tolerated and ignored; mark the
        spec itself instead.

    <project>/.claude/diadoche/spec/NNN_<slug>.md
        A task spec. diadoche also writes NNN_IMPL_PLAN.md and
        NNN_REVIEW_<M>.md alongside it as the task progresses. A line
        <!-- diadoche: no-tests --> in the spec marks a task that needs no
        tests (see RED-GREEN CHECK).

    <project>/.claude/agents/<name>.md
        Optional per-project override of a built-in agent's prompt/model
        (name is one of planner, worker, tester, preprocessor).

    <project>/.claude/diadoche/prefix.md
        Optional override of the shared prompt prefix all agents get as
        their system prompt. Must not contain anything that varies between
        calls, or the prompt cache never hits.

    <project>/.claude/skills/<name>/...
        Optional skill folders; matched against --langs/--frameworks and
        mentioned to the agents when relevant.

    <project>/.claude/diadoche/spec/NNN_BASE
        The diff base commit of a task in progress (plus, with
        --allow-dirty, the files that were already dirty when it started).
        Removed once the task is DONE or parked.

    <project>/.claude/diadoche/spec/NNN_FAILED.md
        Written when a task FAILED: the branch its changes were parked on,
        their diff --stat, the last review, and how to restore them.

    <project>/.claude/diadoche/worktrees/<NNN>-<slug>/
        With --worktree, the git worktree a task runs in (branch
        diadoche/<NNN>-<slug>). Removed once the task is DONE or FAILED;
        kept while it is IN_PROGRESS or BLOCKED.

    <project>/.claude/diadoche/usage.jsonl
        One JSON line per agent call: time, task, agent, iteration, model,
        token counts, cost (USD), duration.

`

const helpExamples = `EXAMPLES
    Run the main loop over a project, with a 5-iteration budget:
        diadoche --dir /path/to/project --ITER 5

    Restrict which skills are mentioned to the agents:
        diadoche --dir /path/to/project --langs go,typescript --frameworks gin,next

    Preview what would happen without invoking claude at all:
        diadoche --dir /path/to/project --dry-run

    Split a raw, multi-part task file into atomic specs:
        diadoche --dir /path/to/project --preprocess --task .claude/diadoche/spec/raw/csrf.md

    Scaffold a brand new project:
        diadoche --initialize /path/to/project

    Save cost by running worker on a cheaper model:
        diadoche --dir /path/to/project --worker-model claude-sonnet-5-5
        DIADOCHE_WORKER_MODEL=claude-sonnet-5-5 diadoche --dir /path/to/project

    Keep going across claude usage-limit windows (restarts diadoche when
    the window reopens; see diadoche-restarter --help):
        diadoche-restarter -- --dir /path/to/project --ITER 5
`

// Config holds all runtime settings for a single diadoche run.
type Config struct {
	Dir        string
	Iter       int
	Langs      []string
	Frameworks []string
	DryRun     bool
	ClaudeBin  string
	Preprocess bool
	Task       string
	Initialize string

	// MaxDiffBytes bounds the diff handed to tester, set by either
	// --max-diff-bytes or its synonym --tester-diff-limit (passing both is
	// a startup error); see DefaultMaxDiffBytes.
	MaxDiffBytes int

	// PlannerModel, WorkerModel, TesterModel, PreprocessorModel are model
	// overrides resolved from --<agent>-model (if passed) else
	// DIADOCHE_<AGENT>_MODEL (if set), with Source recording which of the
	// two it was. The zero value means "no override — use whatever
	// LoadOrDefault picks up from .claude/agents/<name>.md or the built-in
	// default"; resolution continues in agents.ResolveModel, which is also
	// where the final model name is validated.
	PlannerModel      agents.ModelOverride
	WorkerModel       agents.ModelOverride
	TesterModel       agents.ModelOverride
	PreprocessorModel agents.ModelOverride

	// MaxCallMinutes, when > 0, kills a single agent call if it runs longer
	// than this many minutes — caps a runaway call (stuck in its own long
	// tool-call loop) at a bounded cost instead of letting it run until an
	// external limit (e.g. the account's usage window) stops it. 0 = no
	// limit (default): a new safety feature, off by default so it can
	// never unexpectedly kill a legitimately long-running call on upgrade.
	MaxCallMinutes int

	// MaxCallCostUSD, when > 0, fails the task if a single agent call's
	// cost exceeds it. Checked only after a call returns (cost is only
	// known once the claude CLI's JSON result comes back), so unlike
	// MaxCallMinutes it cannot preempt the spend — it can only stop the
	// task from continuing once a call has already turned out to be far
	// more expensive than expected. 0 = no limit (default).
	MaxCallCostUSD float64

	// MaxCostPerTaskUSD, when > 0, fails the current task once the total
	// cost recorded for it in usage.jsonl (previous runs included) exceeds
	// it. Checked after each agent call. 0 = no limit (default).
	MaxCostPerTaskUSD float64

	// MaxCostPerRunUSD, when > 0, stops the run after the current agent
	// call once this run's recorded cost exceeds it; the current task stays
	// pending and diadoche exits non-zero. 0 = no limit (default).
	MaxCostPerRunUSD float64

	// StableSystemPrompt passes claude's
	// --exclude-dynamic-system-prompt-sections, so per-machine sections
	// (cwd, env info, memory paths, git status) move out of the system
	// prompt and the shared prefix stays identical between calls and
	// machines. On by default; --stable-system-prompt=false turns it off
	// for a claude CLI that doesn't have that flag.
	StableSystemPrompt bool

	// AllowDirty skips the dirty-working-tree check: at run start with
	// Worktree, before each task without it. Without Worktree those files
	// are then left alone when a FAILED task is parked.
	AllowDirty bool

	// Worktree runs each task in its own git worktree and branch, merging
	// the branch into the current one once the task is DONE (default on).
	// false gives the old in-place behaviour: tasks work in the main
	// working tree and a FAILED task's changes are parked.
	Worktree bool

	// RedGreen runs the red-green check after worker: the task's new and
	// changed test files are run on the base commit, and tests that pass
	// there fail the iteration. The zero value is off; Parse defaults it
	// on (--red-green).
	RedGreen bool

	// TestGlobs are extra test-file globs (--test-globs) for non-Go
	// projects; *_test.go always counts. Empty means: inferred from the
	// plan's "## Test Command".
	TestGlobs []string
}

// TaskPath resolves --task relative to --dir, unless it's already absolute.
func (c Config) TaskPath() string {
	if filepath.IsAbs(c.Task) {
		return c.Task
	}
	return filepath.Join(c.Dir, c.Task)
}

// Parse parses CLI args (excluding argv[0]) into a Config. On -h/-help/
// --help it prints the full man-page-style help to stdout and returns
// ErrHelp (see its doc comment for how callers should handle that).
func Parse(args []string) (*Config, error) {
	fs := flag.NewFlagSet("diadoche", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // suppress the flag package's own terse usage dump; we print our own below
	dir := fs.String("dir", ".", "project root directory (must contain .claude/diadoche)")
	iter := fs.Int("ITER", DefaultIter, fmt.Sprintf("max iterations per task (1-%d)", MaxIter))
	langs := fs.String("langs", "", "comma-separated languages, matched against .claude/skills subfolders")
	frameworks := fs.String("frameworks", "", "comma-separated frameworks, matched against .claude/skills subfolders")
	dryRun := fs.Bool("dry-run", false, "log what would be sent to each agent instead of invoking the claude CLI")
	claudeBin := fs.String("claude-bin", "claude", "path to the claude CLI binary")
	preprocess := fs.Bool("preprocess", false, "split a raw task file into atomic task specs and exit, instead of running the main loop")
	task := fs.String("task", "", "path to the raw task file for --preprocess, e.g. .claude/diadoche/spec/raw/foo.md")
	initialize := fs.String("initialize", "", "scaffold .claude/ in the given project path and exit, doing nothing else")
	maxDiffBytes := fs.Int("max-diff-bytes", DefaultMaxDiffBytes, "above this many bytes of diff (`git diff` plus the contents of new untracked files), tester gets `git diff --stat`, the untracked file list and an instruction to open what it needs itself")
	testerDiffLimit := fs.Int("tester-diff-limit", DefaultMaxDiffBytes, "synonym of --max-diff-bytes (pass only one of the two)")
	plannerModel := fs.String("planner-model", "", "override the planner agent's model (else $DIADOCHE_PLANNER_MODEL, else .claude/agents/planner.md, else built-in default)")
	workerModel := fs.String("worker-model", "", "override the worker agent's model (else $DIADOCHE_WORKER_MODEL, else .claude/agents/worker.md, else built-in default)")
	testerModel := fs.String("tester-model", "", "override the tester agent's model (else $DIADOCHE_TESTER_MODEL, else .claude/agents/tester.md, else built-in default)")
	preprocessorModel := fs.String("preprocessor-model", "", "override the preprocessor agent's model (else $DIADOCHE_PREPROCESSOR_MODEL, else .claude/agents/preprocessor.md, else built-in default)")
	maxCallMinutes := fs.Int("max-call-minutes", 0, "kill a single agent call if it runs longer than this many minutes (0 = no limit)")
	allowDirty := fs.Bool("allow-dirty", false, "skip the dirty-working-tree check (at run start with --worktree, before each task with --worktree=false); with --worktree=false those files are never parked when the task FAILS")
	worktree := fs.Bool("worktree", true, "run each task in its own git worktree .claude/diadoche/worktrees/<NNN>-<slug> on branch diadoche/<NNN>-<slug>, merging it into the current branch once DONE; --worktree=false works in the main working tree and parks FAILED changes instead")
	stableSystemPrompt := fs.Bool("stable-system-prompt", true, "pass claude --exclude-dynamic-system-prompt-sections, keeping the shared prompt prefix identical between calls; set to false for a claude CLI without that flag")
	redGreen := fs.Bool("red-green", true, "after worker, run the task's new/changed test files on the base commit in a temporary worktree; tests that pass there fail the iteration")
	testGlobs := fs.String("test-globs", "", "comma-separated test-file globs for non-Go projects, e.g. 'test_*.py,*.spec.ts'; default: inferred from the plan's ## Test Command; *_test.go always counts")
	maxCallCost := fs.Float64("max-call-cost", 0, "fail the task if a single agent call's cost in USD exceeds this — checked after the call returns, so it cannot preempt the spend (0 = no limit)")
	maxCostPerTask := fs.Float64("max-cost-per-task", 0, "fail the task once the total recorded cost of the task in usage.jsonl, previous runs included, exceeds this many USD; checked after each agent call (0 = no limit)")
	maxCostPerRun := fs.Float64("max-cost-per-run", 0, "stop the run after the current agent call once this run's recorded cost exceeds this many USD; the current task stays pending and diadoche exits non-zero (0 = no limit)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(fs, os.Stdout)
			return nil, ErrHelp
		}
		fmt.Fprintln(os.Stderr, "Run 'diadoche --help' for usage.")
		return nil, err
	}

	// Which model flags were actually passed, as opposed to left at their
	// "" default — the difference between "no override" and an explicitly
	// empty override, which is an error.
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })

	if strings.TrimSpace(*initialize) != "" {
		// --initialize is a standalone mode: scaffolding only, independent
		// of every other flag, so no other validation applies.
		return &Config{Initialize: *initialize}, nil
	}

	if *iter < 1 || *iter > MaxIter {
		return nil, fmt.Errorf("--ITER must be between 1 and %d, got %d", MaxIter, *iter)
	}
	if *preprocess && strings.TrimSpace(*task) == "" {
		return nil, fmt.Errorf("--preprocess requires --task <path to raw task file>")
	}

	// --max-diff-bytes and --tester-diff-limit are two names for the same
	// setting (the former predates the latter, so neither can be dropped).
	// Passing both is rejected even when the values agree: silently
	// honouring one of two deliberately-set flags is worse than saying so.
	diffLimitFlag, diffLimit := "--max-diff-bytes", *maxDiffBytes
	switch {
	case passed["max-diff-bytes"] && passed["tester-diff-limit"]:
		return nil, fmt.Errorf("--max-diff-bytes and --tester-diff-limit are synonyms; pass only one of them")
	case passed["tester-diff-limit"]:
		diffLimitFlag, diffLimit = "--tester-diff-limit", *testerDiffLimit
	}
	if diffLimit < 0 {
		return nil, fmt.Errorf("%s must be >= 0, got %d", diffLimitFlag, diffLimit)
	}
	if *maxCallMinutes < 0 {
		return nil, fmt.Errorf("--max-call-minutes must be >= 0, got %d", *maxCallMinutes)
	}
	if *maxCallCost < 0 {
		return nil, fmt.Errorf("--max-call-cost must be >= 0, got %g", *maxCallCost)
	}
	if *maxCostPerTask < 0 {
		return nil, fmt.Errorf("--max-cost-per-task must be >= 0, got %g", *maxCostPerTask)
	}
	if *maxCostPerRun < 0 {
		return nil, fmt.Errorf("--max-cost-per-run must be >= 0, got %g", *maxCostPerRun)
	}

	globs := splitCSV(*testGlobs)
	if err := redgreen.ValidateGlobs(globs); err != nil {
		return nil, fmt.Errorf("--test-globs: %w", err)
	}

	plannerOverride, err := resolveModelOverride(passed, "planner-model", *plannerModel, "DIADOCHE_PLANNER_MODEL")
	if err != nil {
		return nil, err
	}
	workerOverride, err := resolveModelOverride(passed, "worker-model", *workerModel, "DIADOCHE_WORKER_MODEL")
	if err != nil {
		return nil, err
	}
	testerOverride, err := resolveModelOverride(passed, "tester-model", *testerModel, "DIADOCHE_TESTER_MODEL")
	if err != nil {
		return nil, err
	}
	preprocessorOverride, err := resolveModelOverride(passed, "preprocessor-model", *preprocessorModel, "DIADOCHE_PREPROCESSOR_MODEL")
	if err != nil {
		return nil, err
	}

	return &Config{
		Dir:                *dir,
		Iter:               *iter,
		Langs:              splitCSV(*langs),
		Frameworks:         splitCSV(*frameworks),
		DryRun:             *dryRun,
		ClaudeBin:          *claudeBin,
		Preprocess:         *preprocess,
		Task:               *task,
		MaxDiffBytes:       diffLimit,
		PlannerModel:       plannerOverride,
		WorkerModel:        workerOverride,
		TesterModel:        testerOverride,
		PreprocessorModel:  preprocessorOverride,
		MaxCallMinutes:     *maxCallMinutes,
		MaxCallCostUSD:     *maxCallCost,
		MaxCostPerTaskUSD:  *maxCostPerTask,
		MaxCostPerRunUSD:   *maxCostPerRun,
		AllowDirty:         *allowDirty,
		Worktree:           *worktree,
		StableSystemPrompt: *stableSystemPrompt,
		RedGreen:           *redGreen,
		TestGlobs:          globs,
	}, nil
}

// resolveModelOverride implements the flag > environment variable priority
// for one agent's model override: a flag that was passed always wins, and
// only when it was left out does the environment variable apply. Returns
// the zero ModelOverride (no override — keep resolving further down the
// chain) when neither is present.
//
// "Passed but empty" and "set but empty" are deliberately errors rather
// than "no override": the user clearly meant to pick a model, and silently
// falling back to a default would hide the mistake until the bill arrives.
func resolveModelOverride(passed map[string]bool, flagName, flagVal, envName string) (agents.ModelOverride, error) {
	if passed[flagName] {
		value := strings.TrimSpace(flagVal)
		if value == "" {
			return agents.ModelOverride{}, fmt.Errorf("--%s must not be empty", flagName)
		}
		return agents.ModelOverride{Value: value, Source: agents.ModelSourceFlag}, nil
	}
	if envVal, ok := os.LookupEnv(envName); ok {
		value := strings.TrimSpace(envVal)
		if value == "" {
			return agents.ModelOverride{}, fmt.Errorf("%s is set but empty", envName)
		}
		return agents.ModelOverride{Value: value, Source: agents.ModelSourceEnv}, nil
	}
	return agents.ModelOverride{}, nil
}

// printHelp writes the full man-page-style help text to w, including the
// auto-generated flag list (fs.PrintDefaults) so it can never drift out of
// sync with the actual flag definitions above.
func printHelp(fs *flag.FlagSet, w io.Writer) {
	fmt.Fprint(w, helpName)
	fmt.Fprint(w, helpSynopsis)
	fmt.Fprint(w, helpDescription)
	fmt.Fprint(w, helpModes)
	fmt.Fprint(w, helpModels)
	fmt.Fprint(w, helpCallBudget)
	fmt.Fprint(w, helpUsageLimits)
	fmt.Fprint(w, helpFlagsHeader)
	fs.SetOutput(w)
	fs.PrintDefaults()
	fmt.Fprint(w, helpFiles)
	fmt.Fprint(w, helpExamples)
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
