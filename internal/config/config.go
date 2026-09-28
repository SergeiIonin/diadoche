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
)

const (
	DefaultIter = 3
	MaxIter     = 8

	// DefaultMaxDiffBytes bounds the raw `git diff` handed to tester before
	// diadoche falls back to `git diff --stat` plus a diff restricted to the
	// files the implementation plan says were meant to change. Kept modest
	// on purpose — this whole flag exists to cut token spend, so the
	// default should actually cut it on anything but a small change.
	DefaultMaxDiffBytes = 50_000
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
             [--planner-model <name>] [--worker-model <name>]
             [--tester-model <name>] [--max-call-minutes <n>]
             [--max-call-cost <usd>] [--allow-dirty]
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
    the changes and moves on to the next task; if --ITER rounds run out
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
    not called at all — the iteration is immediately unsuccessful. A diff
    over --max-diff-bytes is replaced with 'git diff --stat' plus a diff
    restricted to the files listed in "## Files" of the plan.

    FAILED TASKS: every task starts from a clean working tree. When a task
    becomes FAILED (iterations exhausted, an agent error, --max-call-cost),
    its changes since the diff base (outside .claude/diadoche/ and
    .claude/agents/) are committed to a new branch
    diadoche/failed/<NNN>-<slug> without switching branches, then reverted
    in the working tree. The task's bookkeeping (plan, reviews, STATUS.md,
    ...) is committed to the current branch as "diadoche: FAILED <spec>",
    and spec/<NNN>_FAILED.md names the branch and how to bring the changes
    back. A usage-limit error is not a failure: nothing is parked, the task
    stays IN_PROGRESS and diadoche exits, so diadoche-restarter can resume
    it later with its changes in place.

    Before a task starts from scratch, diadoche refuses to run if the
    working tree already has uncommitted changes outside .claude/diadoche/
    and .claude/agents/ (they would otherwise be parked with the task's
    own). --allow-dirty skips this check; such files are then never
    parked. A task resumed after a restart keeps its own changes and the
    diff base recorded in spec/<NNN>_BASE at its first start.

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
        --preprocess), .claude/skills/, and a copy of each built-in agent
        prompt under .claude/agents/*.md so they're easy to customize.
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
    which of the above set the model. An unrecognized model name is a
    startup error, before any claude process runs.

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

`

const helpFlagsHeader = `FLAGS

`

const helpFiles = `
FILES
    <project>/.claude/diadoche/spec/STATUS.md
        Source of truth for every task's status: QUEUED, IN_PROGRESS,
        DONE or FAILED.

    <project>/.claude/diadoche/spec/QUEUE.md
        This run's work queue, rebuilt from STATUS.md on every sync.

    <project>/.claude/diadoche/spec/NNN_<slug>.md
        A task spec. diadoche also writes NNN_IMPL_PLAN.md and
        NNN_REVIEW_<M>.md alongside it as the task progresses.

    <project>/.claude/agents/<name>.md
        Optional per-project override of a built-in agent's prompt/model
        (name is one of planner, worker, tester, preprocessor).

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
        diadoche --dir /path/to/project --worker-model claude-sonnet-5
        DIADOCHE_WORKER_MODEL=claude-sonnet-5 diadoche --dir /path/to/project

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

	// MaxDiffBytes bounds the raw diff handed to tester (subtask 4 of
	// 09_optimize_agents.md); see DefaultMaxDiffBytes.
	MaxDiffBytes int

	// PlannerModel, WorkerModel, TesterModel, PreprocessorModel are model
	// overrides resolved from --<agent>-model (if set) else
	// DIADOCHE_<AGENT>_MODEL (if set); empty means "no override — use
	// whatever LoadOrDefault picks up from .claude/agents/<name>.md or the
	// built-in default". Applied via agents.ApplyModelOverride, which is
	// also where the final model name is validated.
	PlannerModel      string
	WorkerModel       string
	TesterModel       string
	PreprocessorModel string

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

	// AllowDirty lets a task start even though the working tree already
	// has uncommitted changes outside diadoche's own directories. Those
	// files are then left alone when a FAILED task is parked.
	AllowDirty bool
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
	maxDiffBytes := fs.Int("max-diff-bytes", DefaultMaxDiffBytes, "above this many bytes of raw `git diff`, tester gets `git diff --stat` plus a diff restricted to the plan's file list instead")
	plannerModel := fs.String("planner-model", "", "override the planner agent's model (else $DIADOCHE_PLANNER_MODEL, else .claude/agents/planner.md, else built-in default)")
	workerModel := fs.String("worker-model", "", "override the worker agent's model (else $DIADOCHE_WORKER_MODEL, else .claude/agents/worker.md, else built-in default)")
	testerModel := fs.String("tester-model", "", "override the tester agent's model (else $DIADOCHE_TESTER_MODEL, else .claude/agents/tester.md, else built-in default)")
	preprocessorModel := fs.String("preprocessor-model", "", "override the preprocessor agent's model (else $DIADOCHE_PREPROCESSOR_MODEL, else .claude/agents/preprocessor.md, else built-in default)")
	maxCallMinutes := fs.Int("max-call-minutes", 0, "kill a single agent call if it runs longer than this many minutes (0 = no limit)")
	allowDirty := fs.Bool("allow-dirty", false, "start a task even if the working tree already has uncommitted changes; those files are never parked when the task FAILS")
	maxCallCost := fs.Float64("max-call-cost", 0, "fail the task if a single agent call's cost in USD exceeds this — checked after the call returns, so it cannot preempt the spend (0 = no limit)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(fs, os.Stdout)
			return nil, ErrHelp
		}
		fmt.Fprintln(os.Stderr, "Run 'diadoche --help' for usage.")
		return nil, err
	}

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

	if *maxDiffBytes < 0 {
		return nil, fmt.Errorf("--max-diff-bytes must be >= 0, got %d", *maxDiffBytes)
	}
	if *maxCallMinutes < 0 {
		return nil, fmt.Errorf("--max-call-minutes must be >= 0, got %d", *maxCallMinutes)
	}
	if *maxCallCost < 0 {
		return nil, fmt.Errorf("--max-call-cost must be >= 0, got %g", *maxCallCost)
	}

	return &Config{
		Dir:               *dir,
		Iter:              *iter,
		Langs:             splitCSV(*langs),
		Frameworks:        splitCSV(*frameworks),
		DryRun:            *dryRun,
		ClaudeBin:         *claudeBin,
		Preprocess:        *preprocess,
		Task:              *task,
		MaxDiffBytes:      *maxDiffBytes,
		PlannerModel:      resolveModelOverride(*plannerModel, "DIADOCHE_PLANNER_MODEL"),
		WorkerModel:       resolveModelOverride(*workerModel, "DIADOCHE_WORKER_MODEL"),
		TesterModel:       resolveModelOverride(*testerModel, "DIADOCHE_TESTER_MODEL"),
		PreprocessorModel: resolveModelOverride(*preprocessorModel, "DIADOCHE_PREPROCESSOR_MODEL"),
		MaxCallMinutes:    *maxCallMinutes,
		MaxCallCostUSD:    *maxCallCost,
		AllowDirty:        *allowDirty,
	}, nil
}

// resolveModelOverride implements the flag > environment variable priority
// for one agent's model override: a non-empty flag value always wins; only
// when the flag was left at its default ("") does the environment variable
// apply. Returns "" (no override) if neither is set.
func resolveModelOverride(flagVal, envName string) string {
	if strings.TrimSpace(flagVal) != "" {
		return flagVal
	}
	return strings.TrimSpace(os.Getenv(envName))
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
