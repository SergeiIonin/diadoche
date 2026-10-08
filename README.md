# Diadoche

Loop-engineering CLI: drives an autonomous `planner -> worker -> tester`
cycle on top of independent `claude --dangerously-skip-permissions`
processes, until a task becomes `DONE` (or iterations run out).

## Build

```
go build -o diadoche ./cmd/diadoche
go build -o diadoche-restarter ./cmd/diadoche-restarter
```

(or just `make build`, which builds both).

## Usage

To scaffold a new project, run:

```
diadoche --initialize /path/to/project
```

This creates `.claude` in that project if it doesn't exist yet (or reuses
it if it does) and writes everything diadoche needs inside it: an empty
`.claude/diadoche/spec/STATUS.md`, `.claude/diadoche/spec/raw/` (for
`--preprocess` input), `.claude/skills/`, and a copy of each built-in agent
prompt under `.claude/agents/*.md`, and the built-in shared prompt prefix as
`.claude/diadoche/prefix.md`, so they're easy to customize. It never
overwrites a file that's already there, so re-running it later (e.g. after
upgrading diadoche) is safe. `--initialize` does only this, then exits —
no queue sync, no agent invocation.

The target project needs a `.claude/diadoche/spec/` folder with task files
`NNN_<slug>.md` (e.g. `001_add-login.md`). Agent prompts for
planner/worker/tester/preprocessor are built into the `diadoche` binary, so
it works in any project without extra setup. To customize a prompt or model
for one project, drop a `.claude/agents/<name>.md` file there (same
frontmatter format as the built-in ones) — it overrides the built-in default
for that agent only; every other agent keeps using its built-in prompt.

```
diadoche --dir /path/to/project --ITER 3 --langs go,typescript --frameworks gin,next
```

Flags:

- `--dir` — project root (default `.`).
- `--ITER` — max planner/worker/tester iterations per task, 1..8 (default 3).
- `--langs`, `--frameworks` — comma-separated; used to hint agents which
  `.claude/skills` subfolders are relevant. Without these flags, agents are
  told about every skill that's present.
- `--dry-run` — walk the queue read-only: for every pending task, in queue
  order, print the prompts planner, worker and tester would get in
  iteration 1 (the plan, the diff and tester's relevant-ADRs block, which
  don't exist in a dry run, are shown as labelled `[dry-run placeholder]`s). `claude` is not invoked,
  nothing is written (`STATUS.md`, `QUEUE.md`, spec files, `usage.jsonl`,
  run state, run artifacts) and git is not touched; the run ends with
  `dry-run: no state was changed`.
- `--claude-bin` — path to the `claude` binary (default `claude` on `$PATH`).
- `--preprocess` + `--task <path>` — preprocessor mode, see below.
- `--initialize <project-path>` — scaffold `.claude/` in a project and
  exit; ignores every other flag. See above.
- `--planner-model`, `--worker-model`, `--tester-model`,
  `--preprocessor-model`, `--adr-model` — override one agent's model. Priority:
  flag > `DIADOCHE_<AGENT>_MODEL` env var > `model:` in the project's
  `.claude/agents/<name>.md` > built-in default. Only the model is
  affected — a `.claude/agents/<name>.md` prompt override still applies
  regardless of which of the above picked the model. An unrecognized model
  name is a startup error, before any `claude` process runs. The resolved
  models are logged once at the start of the run.
- `--max-diff-bytes` (alias `--tester-diff-limit`; pass only one of the
  two, passing both is a startup error) — above this many bytes of diff
  (`git diff` plus the contents of new untracked files), tester gets
  `git diff --stat`, the untracked file list and an instruction to open
  what it needs itself (default 204800, i.e. 200 KB). See "Diff-scoped
  tester" below.
- `--max-call-minutes` — kill a single agent call if it runs longer than
  this many minutes (default 0 = no limit). See "Per-call budget" below.
- `--max-call-cost` — fail the task if a single agent call's cost (USD)
  exceeds this (default 0 = no limit). See "Per-call budget" below.
- `--max-cost-per-task` — fail the task once its total recorded cost (USD,
  previous runs included) exceeds this (default 0 = no limit). See "Task
  and run budgets" below.
- `--max-cost-per-run` — stop the run after the current agent call once
  this run's recorded cost (USD) exceeds this; the task stays pending and
  diadoche exits non-zero (default 0 = no limit). See "Task and run
  budgets" below.
- `--worktree` — run each task in its own git worktree
  `.claude/diadoche/worktrees/<NNN>-<slug>` on branch
  `diadoche/<NNN>-<slug>`, merging it into the current branch once `DONE`
  (default `true`). `--worktree=false` works in the main working tree and
  parks a `FAILED` task's changes instead. See "Worktrees" below.
- `--parallel N` — run up to N tasks that are ready by the dependency graph
  at the same time, each in its own worktree; merges into the current
  branch stay one at a time, in the order the tasks finish (default `1`,
  one task at a time). Requires `--worktree`. See "Parallel tasks" below.
- `--allow-dirty` — skip the dirty-working-tree check (at run start with
  `--worktree`, before each task with `--worktree=false`); with
  `--worktree=false` those files are never parked if the task fails. See
  "Worktrees" and "FAILED tasks (`--worktree=false`)" below.
- `--stable-system-prompt` — pass `claude`
  `--exclude-dynamic-system-prompt-sections`, so per-machine sections (cwd,
  env info, memory paths, git status) stay out of the system prompt and the
  shared prompt prefix is identical between calls (default `true`). Set
  `--stable-system-prompt=false` for a `claude` CLI without that flag. See
  "Prompt caching" below.
- `--red-green` — after worker, run the task's new/changed test files on
  the task's base commit in a temporary worktree; tests that pass there
  (i.e. check nothing) fail the iteration (default `true`).
  `--red-green=false` disables the step. See "Red-green check" below.
- `--test-globs` — comma-separated test-file globs for non-Go projects,
  e.g. `'test_*.py,*.spec.ts'` (default: inferred from the plan's
  `## Test Command`; `*_test.go` always counts).
- `--fresh` — ignore the saved `.claude/diadoche/run-state.json` and start
  from the head of the queue; the file is rewritten from scratch. See
  "Restart state and graceful shutdown" below.
- `--shutdown-timeout` — on SIGINT/SIGTERM, wait this long for the running
  agent call to finish before killing it (default `2m`); a second signal
  exits immediately.
- `--on-task-done <command>` — run a shell command after a task is DONE and
  its changes are merged into (or, with `--worktree=false`, committed on)
  the branch the run started on; repeatable, the commands run in the order
  given. See "On-task-done hooks" below.
- `--hook-timeout` — kill an `--on-task-done` command, together with its
  process group, once it runs longer than this (default `5m`).
- `--on-fail <list>` — comma-separated, ordered policies applied once a
  task's `--ITER` iterations run out without DONE (default
  `escalate,block`; policies: `escalate`, `split`, `block`);
  `--on-fail ""` marks the task FAILED and parks its changes, as before.
  See "Failure policy" below.
- `--escalation-model <name>` — model of planner and worker in the
  escalated iteration (else `DIADOCHE_ESCALATION_MODEL`, else
  `claude-fable-5-1`); validated at startup.
- `--adr` — after a task is DONE and merged, turn its `## Decisions` into
  architecture decision records under `docs/adr/` (off by default). See
  "ADR step" below.
- `--adr-model` — the `adr-writer` agent's model (also
  `DIADOCHE_ADR_MODEL`; see "Model selection").
- `-h` / `-help` / `--help` — print the full man-page-style help and exit.

## On-task-done hooks (`--on-task-done`)

`--on-task-done '<command>'` is the extension point for the moment a task
is genuinely finished: notifications, docs, external bookkeeping. It runs
only for a task that is DONE **and** merged — never for a FAILED, parked or
BLOCKED task, or one whose merge failed; `--dry-run` only lists the
commands it would run. Each command runs as `sh -c '<command>'` in the
repository root (so `--on-task-done 'make docs && git add -A'` works), with
these variables on top of diadoche's environment, always set:

- `DIADOCHE_TASK_ID` — `<NNN>-<slug>`;
- `DIADOCHE_TASK_TITLE` — the task's heading (its commit title);
- `DIADOCHE_REPO_ROOT` — absolute path of the repository root;
- `DIADOCHE_BASE_COMMIT` — the commit the task started from;
- `DIADOCHE_MERGE_COMMIT` — the commit its changes landed as;
- `DIADOCHE_DECISIONS_FILE` — absolute path of the task's `decisions.json`,
  or empty when there is none.

A non-zero exit or a timeout (`--hook-timeout`) is only a warning: it is
logged, written to `run.log`, and the task's `STATUS.md` row stays DONE with
a reason (`DONE: on-task-done hook 1 failed (exit 2)`); the run goes on.
Each command's output is kept as `on-task-done-<N>.stdout`/`.stderr` in the
task's run directory (at most 1 MiB each). Changes the hooks leave in the
repository (outside `.claude/diadoche`) are committed as
`diadoche: on-task-done for <NNN>-<slug>`; no changes, no commit.

## ADR step (`--adr`)

Agents remember nothing between tasks, so a decision only survives as a file.
With `--adr`, every task that is DONE and merged gets its decisions — the
`## Decisions` worker reported, i.e. its `decisions.json` — written down as
architecture decision records under `docs/adr/` in the repository root.

The step is a built-in task-done handler: same trigger as the
`--on-task-done` hooks, run first (the hooks then see the new ADRs), and,
like them, never a task failure — a failure rolls `docs/adr/` back and the
task's row says `DONE: adr step failed (...)`. Its changes are part of the
same `diadoche: on-task-done for <NNN>-<slug>` commit.

- A task without decisions (an empty or `none` section) skips the step
  entirely: no agent call, no file, no cost.
- Otherwise the `adr-writer` agent (built-in default `claude-sonnet-5-5`)
  gets the decisions, the task's diff against its base commit and the current
  index, and writes one ADR per decision as `docs/adr/TMP<k>-<slug>.md`
  (`k` = the decision's position), titled `# TMP<k>. <Title>`.
- diadoche, not the agent, assigns the numbers: the next free ones after the
  existing ADRs, which are never renumbered. It renames the files to
  `docs/adr/NNNN-<slug>.md` and replaces the `TMP<k>` references.
- A decision that reverses an existing ADR changes only that ADR's status
  line, to `Status: Superseded by NNNN`; any other edit the agent makes to an
  existing ADR is undone (and logged).
- `docs/adr/README.md` — a table of number, title, status and summary — is
  rebuilt by diadoche from the ADR files, never by the agent.
- The project's `CLAUDE.md` (created if missing) gets, once, the line
  `Project architecture decisions live in docs/adr/README.md. Before planning, read the index and open the ADRs relevant to the task.`
  — no links to individual ADRs.
- The call is recorded in `usage.jsonl` (iteration 0) and `run.log`, its
  output kept as `iter-0-adr-writer.stdout`/`.stderr` in the task's run
  directory.
  `--dry-run` calls no agent and writes nothing.

The records are read back by the loop: planner lists the relevant ones under
`## Relevant ADRs` in its plan, and tester gets those ADRs (with worker's
`## Decisions`) and treats an unrecorded contradiction with an `Accepted` one
as a blocking remark; see "How it works".

Each ADR looks like this:

```
# 0004. Merge without squash

Status: Accepted
Date: 2026-10-08
Summary: Merge task branches with a merge commit instead of squashing.
Task: [014_merges.md](../../.claude/diadoche/spec/014_merges.md)

## Context
## Decision
## Alternatives considered
## Consequences
```

## Failure policy (`--on-fail`)

A task whose `--ITER` iterations run out without a DONE verdict gets the
`--on-fail` chain: its policies are applied in order until one ends the
task. A chain that runs out leaves the task FAILED; `--on-fail ""` is the
old behaviour (FAILED, changes parked, `NNN_FAILED.md`). Anything else that
cuts a task short (an agent error, `--max-call-cost`, `--max-cost-per-task`)
still fails it at once, and a usage limit, a signal or `--max-cost-per-run`
still leave it pending.

- `escalate` — one more complete planner → worker → tester iteration,
  numbered `--ITER`+1, with planner and worker on `--escalation-model`
  (tester keeps its model). It gets the last review like any iteration and
  goes through the same plan check, red-green check, limit pauses and
  budgets; its plan, review, artifacts and `usage.jsonl` lines are recorded
  as usual, and `run.log` marks it (`escalation_start`, `escalated=true`).
  DONE finishes the task normally (commit/merge, hooks) and ends the chain.
- `split` — the preprocessor agent (on `--preprocessor-model`) gets the
  task's spec and every review of it (`NNN_REVIEW_*.md`) and cuts it into
  2–4 smaller, self-contained tasks that together cover the whole spec. The
  new specs are numbered after the highest existing number, get a
  `Split-from: <original spec>` line and are QUEUED to run next, in place
  of the original and ahead of everything queued behind it. The original
  becomes `BLOCKED: split into <specs> …` — terminal, never re-queued, and
  its dependents are not blocked; its changes are parked on
  `diadoche/split/<NNN>-<slug>` (with `--worktree` the worktree is removed
  and its branch renamed to that), so the new tasks start from a clean
  base, and `spec/NNN_BLOCKED.md` says where they are. The call is logged
  and recorded in `usage.jsonl` like any agent call. If it fails or yields
  fewer than two new specs, `split` is a no-op (`outcome=no_op`, its files
  removed) and the next policy runs: `escalate,split,block` still ends in
  `block`.
- `block` — the task becomes `BLOCKED: <reason>` in `STATUS.md`, naming
  the policies applied. With `--worktree` its changes are committed on its
  branch and the worktree and branch are kept (like a merge conflict); with
  `--worktree=false` they are parked on `diadoche/blocked/<NNN>-<slug>`.
  `spec/NNN_BLOCKED.md` explains what was tried and where the changes are.
  BLOCKED tasks are never re-queued (also not by `diadoche-restarter`); set
  the row back to `QUEUED` to retry one. (`WAITING` tasks, which wait for a
  human's answer, are never re-queued by `diadoche-restarter` either; see
  [Open questions and WAITING](#open-questions-and-waiting---resume-waiting).)

A task spec may declare dependencies on a line of its own,
`Depends-on: 003, 007`. The line has three states:

- **a list of task numbers** — the task starts only once every task it
  names is `DONE`, and the queue places it after them. When one of them is
  `FAILED` or `BLOCKED`, the task becomes `BLOCKED` instead
  (`depends on <spec>, which is FAILED`), and so, transitively, does every
  pending task that names it; when a task is BLOCKED by `--on-fail`, its
  dependents are BLOCKED right away (`depends on <spec>, which is BLOCKED`).
  A dependency still pending leaves the task `QUEUED` for a later round of
  the loop; if nothing left is ready and nothing is in flight, the run
  stops with an error naming the stuck tasks;
- **`Depends-on: none`** (or an empty `Depends-on:`) — no dependencies, the
  task may run whenever;
- **absent** — the task implicitly runs after the task before it in queue
  order, exactly as before the line existed. This implicit dependency only
  orders the queue: a `FAILED` or `BLOCKED` task never blocks tasks that
  merely follow it.

The declared graph is validated at startup, over every task spec: a
`Depends-on` number that matches no task spec, and a dependency cycle, are
both errors (all offenders are listed, e.g.
`dependency cycle: 004_a.md -> 005_b.md -> 004_a.md`) that stop the run
before any agent is called; `--dry-run` reports them the same way. The
queue (`QUEUE.md`) is ordered topologically: every task after the pending
tasks it depends on, ascending task number between tasks that are both
ready. Every applied policy is a `run.log` event
`on_fail_policy policy=<name> step=<n> outcome=<...>`. `--dry-run` applies
no policy, calls no agent for it and writes no spec.

## Preprocessor (`--preprocess`)

If a user's task actually bundles several independent subtasks ("add CSRF
protection on both FE and BE"), you can drop it as-is into
`.claude/diadoche/spec/raw/<task_name>.md` and split it into atomic specs
with a separate call:

```
diadoche --dir /path/to/project --preprocess --task .claude/diadoche/spec/raw/csrf.md
```

This runs **only** a single preprocessor agent (`claude-opus-5`), which
reads the raw file and writes new `spec/NNN_<slug>.md` files alongside the
existing tasks. The preprocessor never implements anything and never
touches `STATUS.md`/`QUEUE.md` — the program exits once it's done. The new
tasks are picked up by the next regular `diadoche` run (without
`--preprocess`).

The regular loop (without `--preprocess`), once its queue drains, checks
`spec/` for new tasks again (including any that showed up via
`--preprocess` running in a separate process) — it either keeps working
through the new queue, or exits if there's nothing new.

Reads/writes of the `spec/` directory (by both the main loop and the
preprocessor) are guarded by a `spec/.lock` file lock, so a concurrent
`--preprocess` run's half-written task files are never read mid-write.

## How it works

1. On startup, diadoche compares `spec/NNN_<slug>.md` files against the
   `spec/STATUS.md` table; new tasks are added there with status `QUEUED`
   and pushed onto `spec/QUEUE.md` (this run's work queue, in dependency
   order — see `Depends-on` above; a dangling reference or a cycle stops
   the run here).
2. For each task pulled off the queue, status is set to `IN_PROGRESS`, and
   diadoche runs up to `--ITER` rounds of:
   - **planner** (model `claude-opus-5-5` by default) — writes
     `spec/NNN_IMPL_PLAN.md`, which must contain a `## Files` section and a
     `## Test Command` section (the command(s) that run this project's tests
     and linter). Each `## Files` entry is one line,
     `- <path> — create|modify: <one line on what the file is for>`, with the
     path first; diadoche parses only the path. If either section is missing,
     or `## Files` lists no file at all, the iteration is treated as failed
     and planner is asked again, still within the same `--ITER` budget.
     Planner also reads `docs/adr/README.md`, if it exists, and writes an
     optional `## Relevant ADRs` section: one `- <NNNN> — <title>` bullet per
     relevant ADR, or `none`. A missing section is only a warning in the log,
     never a failed iteration.
   - **worker** (model `claude-opus-5-5` by default) — implements the plan,
     working only from its `## Files` list instead of re-exploring the repo;
     if another file turns out to be needed, worker first appends it to
     `## Files` with a one-line justification and only then edits it.
     Worker ends its final answer with a `## Decisions` section: one
     `### <title>` entry per decision with `Context:`, `Decision:` and
     `Alternatives:` lines, or just `none`. Only a closed list counts as a
     decision — a new external dependency, a new or changed external
     contract, a choice between real architectural alternatives, a deviation
     from the spec, or the reversal of an earlier decision; renames,
     contract-preserving refactoring and naming do not. diadoche saves the
     DONE iteration's section as `decisions.json` (see "Run logs and
     artifacts"); a missing section is only a warning;
   - **tester** (model `claude-sonnet-5-5` by default) — reviews a `git diff`
     of the worker's changes (see "Diff-scoped tester" below) plus the plan's
     test command, writes `spec/NNN_REVIEW_M.md`, and, if the task is
     genuinely done, sets `spec/STATUS.md` to `DONE` itself. Its prompt also
     carries the ADRs listed under the plan's `## Relevant ADRs` — numbers,
     statuses and full contents, read from the task's `docs/adr/` — plus
     worker's `## Decisions`, or says explicitly that there are no relevant
     ADRs. A diff that contradicts an `Accepted` ADR without worker recording
     it as a decision is a blocking remark: tester does not mark the task
     `DONE`.
3. Once tester marks a task `DONE`, diadoche commits it (with `--worktree`,
   on the task's branch, then merges it) and moves on to the next task in
   the queue. If iterations run out without `DONE`, diadoche itself sets
   `FAILED` and keeps the task's changes on a `diadoche/failed/...` branch
   (see "Worktrees" and "FAILED tasks" below).
4. From the 2nd iteration on, planner and worker also get the previous
   `spec/NNN_REVIEW_M.md` in their context.

Agent prompts and models are built into the binary (frontmatter `name` /
`model` / `description` plus a body — the system prompt), with per-project
overrides via `.claude/agents/<name>.md` (same format) — overriding a prompt
never requires rebuilding diadoche, only changing the built-in default does.

## Model selection

Each agent's model is resolved in this order, first match wins:

1. `--<agent>-model` flag
2. `DIADOCHE_<AGENT>_MODEL` environment variable
3. `model:` field in the project's `.claude/agents/<name>.md`, if any
4. the agent's built-in default

This only ever picks the model — a `.claude/agents/<name>.md` prompt
override (its body, not its `model:` field) still applies regardless of
which of the above set the model. A project `.claude/agents/<name>.md` with
no `model:` field keeps its prompt and takes the built-in default model.

Every agent's resolved model and the source it came from (`flag`, `env`,
`frontmatter` or `built-in default`) is logged once at the start of the run,
e.g.:

```
models: planner=claude-opus-5-5 (built-in default) worker=claude-sonnet-5-5 (flag) tester=claude-sonnet-5-5 (env)
```

An unrecognized model name is a startup error, before any `claude` process
runs — and so is an explicitly empty one: `--planner-model ""`,
`DIADOCHE_PLANNER_MODEL=` (set to the empty string) or a blank `model:`
field all fail the run rather than quietly falling back to a default.

Built-in defaults: planner `claude-opus-5-5`, worker `claude-opus-5-5`,
tester `claude-sonnet-5-5`, preprocessor `claude-opus-5`, adr-writer
`claude-sonnet-5-5` (`--adr-model` / `DIADOCHE_ADR_MODEL`; resolved and
validated whenever it is set, even without `--adr`).

## Per-call budget

`--max-call-minutes` and `--max-call-cost` cap a single planner/worker/
tester call, so one runaway agent (stuck exploring, or retrying its own
nested `claude` invocations — e.g. a task whose worker step itself runs an
eval script that calls `claude`) can't quietly burn the whole account usage
window before diadoche notices. Both are `0` (no limit) by default, so
upgrading never silently kills a call that used to be allowed to run long.

- `--max-call-minutes` kills the `claude` process once a call runs longer
  than this; the iteration is then treated as failed, same as any other
  agent error. This is the only one of the two that can actually stop money
  being spent mid-call.
- `--max-call-cost` is checked only *after* a call returns — cost is only
  known once the `claude` CLI's JSON result comes back, so it cannot
  preempt an in-flight call. If the call's cost exceeds it, the task fails
  right away instead of continuing to the next agent/iteration — except a
  tester call that already reached `DONE` is never retroactively failed
  over cost, since there's nothing left to prevent and the work is real.

### Task and run budgets

`--max-cost-per-task` and `--max-cost-per-run` cap the sum of the costs
recorded in `usage.jsonl`, in USD. Both are `0` (no limit) by default, are
checked after every agent call, and are off in `--dry-run` (no cost data).

- `--max-cost-per-task` counts every call of the current task, including
  calls made by earlier runs (read from `usage.jsonl` at startup). Once
  exceeded, the task becomes `FAILED: budget exceeded: task cost $… over
  --max-cost-per-task=$…` and is parked like any other failed task; the run
  moves on to the next one.
- `--max-cost-per-run` counts this run's calls. Once exceeded, diadoche
  stops right after the current agent call: the current task stays pending
  (`IN_PROGRESS`, not `FAILED`) with its changes in place, the reason is
  logged (`run budget exhausted`), and diadoche exits with status 1. A task
  the tester marked `DONE` in that very call is still finished
  (committed/merged) first; the run then stops before the next task.

## Usage limits and pauses

An agent call refused by a claude usage limit (the subscription's usage
window is exhausted) or by rate limiting (HTTP 429, `rate_limit_error`) is
not a failure and does not spend an iteration. diadoche recognizes it by
the call's exit code (or the JSON result's `"is_error":true`) together with
the error text (`internal/usagelimit.Detect`); spend limits and team
budgets are excluded, since waiting doesn't lift them. Then:

1. the task becomes `PAUSED` in `STATUS.md`, with the expected resume time
   when the reply carries one (`PAUSED: claude usage window exhausted,
   resumes at 2026-10-08T19:00:00Z`), else without it;
2. diadoche waits until that reset time (`resets 7pm (Europe/Moscow)`, the
   legacy `usage limit reached|<unix time>`, ...), or, without one, for
   5 minutes, doubling on every further time-less pause up to 1 hour;
3. it retries the same call of the same agent in the same iteration. After
   a successful call the task is `IN_PROGRESS` again and the backoff starts
   over at 5 minutes.

SIGINT/SIGTERM interrupts the pause at once, already on the first signal
and without waiting for `--shutdown-timeout` (diadoche exits with status
130): the task stays `PAUSED`, and the next run re-queues it like an
`IN_PROGRESS` one and resumes it with its changes in place.

## Open questions and WAITING (`--resume-waiting`)

A run is unattended, so planner and worker make their own calls and record
them. The exception is a decision that does not follow from the spec, the
ADRs or the code and could reasonably go either way — a product choice, UX,
a cost trade-off. The agent appends such a question to `QUESTIONS.md` in
the repository root (append only, one entry per question):

```
## Question 018-1: Where should the cache live
Task: 018_cache.md
Asked-by: planner
Question: Keep the cache in memory or on disk?
Options:
- A: memory
- B: disk
Default: A
Why: simpler, and the data is small
Reversible: no
Answer:
```

Anything answerable from the spec, the code or an ADR, and anything the
agent can simply decide and record as a decision, does not belong there.
After writing the question the agent continues with its default if the
decision is reversible; if it is not, it ends its answer with `NEEDS_HUMAN`
**on a line of its own** (trimmed, outside any code block — an inline or
quoted mention does not count).

`NEEDS_HUMAN` sets the task aside without spending an iteration, like a
usage-limit pause:

- the task becomes `WAITING: <agent> needs a human decision in iteration
  <k>: answer in QUESTIONS.md, then rerun with --resume-waiting (…)` in
  `STATUS.md`, and `run.log` gets a `waiting` event;
- its `run-state.json` record is kept, exactly as before that call, under
  `waiting`;
- its changes are kept: with `--worktree` committed on its branch, with the
  worktree, branch and `spec/NNN_BASE` kept; with `--worktree=false` parked
  on `diadoche/waiting/<NNN>-<slug>`. `spec/NNN_WAITING.md` says where;
- the run moves on to the next task. Tasks that depend on it (`Depends-on`)
  are skipped and stay `QUEUED`.

`WAITING` is not pending: neither the same run, nor a restart, nor
`diadoche-restarter` picks the task up again. Answer after `Answer:` in
`QUESTIONS.md` (it need not be committed: it never counts as a dirty file),
then run `diadoche --resume-waiting`. Every `WAITING` task is `QUEUED` again
(`resume_waiting` event) and continues at the iteration and agent that
asked (parked changes re-applied); planner and worker get the task's
questions with their answers in their prompts, an entry without one
labelled as unanswered. `QUESTIONS.md` is committed with diadoche's
bookkeeping commits. `--dry-run` changes no status and writes no
`QUESTIONS.md`; with `--resume-waiting` it also prints the `WAITING` tasks'
prompts.

## Restart state and graceful shutdown

After every step diadoche writes `.claude/diadoche/run-state.json`
(through a temp file in the same directory + rename, so the file is never
half-written):

```json
{
  "run_id": "20261008T120000Z-1a2b3c4d",
  "tasks": [
    {
      "spec": "009_x.md",
      "num": "009",
      "iteration": 2,
      "agent": "worker",
      "agent_state": "done",
      "base_commit": "<sha>",
      "worktree": ".claude/diadoche/worktrees/009-x"
    }
  ],
  "task": { "spec": "009_x.md", "...": "the first record of tasks" },
  "updated_at": "2026-10-08T12:34:56Z"
}
```

`tasks` holds one record per active task — several with `--parallel` — in
the order they started; `task` mirrors the first of them for older
readers, and a file with only `task` (written before `tasks` existed) is
still resumed. `agent_state` is `pending`, `running` or `done`; `worktree`
is only there when the task runs in its own worktree; `attempt` is `2` for
a task re-executed after a merge conflict (see "Parallel tasks"). When a
task is DONE, FAILED or BLOCKED its record leaves the list, and at the end
of the run `tasks` is `[]` and `task` is `null`: the file stays, so the
next run sees that there is nothing to continue. The file is kept out of
git (`.git/info/exclude`), and nothing is read or written in `--dry-run`.

On start, every unfinished task in the file is moved to the head of the
queue and continued in the same iteration, with the same base commit and
worktree. A `done` agent is not called again: after worker comes tester,
and after tester the next iteration. A `running` or `pending` agent is
repeated, because it was cut off. So a run stopped between worker and
tester continues with tester; planner and worker are not called again.

The state adds to `STATUS.md`/`QUEUE.md`, it does not replace them. It is
ignored, with a `warning: ignoring run-state.json: <reason>` log line, when
the task is no longer pending in `STATUS.md` (e.g. DONE), is not in the
queue, or the record is malformed. It is also ignored when it is behind the
reviews on disk. In all these cases the `spec/` files (plan, reviews,
`NNN_BASE`) decide where to resume, as they do when there is no file.
`--fresh` ignores the saved state and starts from the head of the queue.

Signals:

- the first SIGINT/SIGTERM lets the agent call in flight finish (up to
  `--shutdown-timeout`, default 2 minutes), then diadoche saves the state
  and exits with status 130. The task stays pending and is never FAILED
  because of a signal;
- when the timeout runs out, the call is killed. `claude` runs in its own
  process group, which is killed as a whole, together with its tool
  subprocesses. The state still has that agent as `running`, so the next
  run repeats it;
- a second signal kills the call and exits at once.

Since `claude` has its own process group, a terminal Ctrl-C reaches only
diadoche, not the call it wants to let finish.

**diadoche-restarter.** The restarter stops diadoche by sending SIGTERM
to its whole process group every 200 ms (SIGKILL after 10 s). The repeat
counts as diadoche's second signal, so the stop stays immediate and the
restarter never waits for `--shutdown-timeout`. diadoche records the agent
as `running` before every call, so even after a SIGKILL `run-state.json`
lets the next run repeat the same agent.

## Diff-scoped tester

When a task first starts, diadoche records the current commit (or, before
the repo's first-ever commit, git's empty-tree hash) as that task's diff
base, in `spec/NNN_BASE`. A task resumed after a restart reads its base
back from there instead of taking the current HEAD. The file is removed
once the task is `DONE` or parked. Before each tester call it computes `git diff <base>` plus any
untracked files since that base — excluding `.claude/diadoche/` and
`.claude/agents/`, diadoche's own bookkeeping/config, so those never look
like part of the worker's change.

New files never show up in `git diff` at all, so the contents of untracked
text files are included alongside it (binary ones are listed by name and
size only, and the index is never touched to produce any of this).

- An empty diff (and no untracked files) means tester is **not called at
  all** — the iteration is immediately treated as unsuccessful.
- The diff and that untracked content count toward `--max-diff-bytes`
  (alias `--tester-diff-limit`) together, so a single huge new file can't
  slip past the limit. Over it, tester instead gets `git diff --stat`, the
  list of untracked files with their sizes, the diff restricted to the
  files listed in the plan's `## Files` section (only when that one fits
  the limit — otherwise it is omitted too and the prompt says so), the
  task's base commit, and an instruction to open just the files it needs
  itself, e.g. `git diff <base> -- <path>`.

## Red-green check

With `--red-green` (the default), diadoche checks after worker and before
tester, in the same iteration, that worker's tests actually test the new
code:

- The test files are the files added or changed since the task's base
  commit that match `*_test.go` or one of `--test-globs`. Without
  `--test-globs`, globs are inferred from the plan's `## Test Command`
  (`pytest` → `test_*.py`, `*_test.py`; `jest`/`npm test` →
  `*.test.ts`, `*.spec.ts`, ...; `cargo test` → `tests/*.rs`; ...).
- diadoche creates a temporary detached git worktree at the base commit in
  the OS temp directory, copies only those test files into it and runs the
  plan's Test Command there. The worktree is always removed afterwards,
  also when the command fails; the task's own working tree is never
  touched.
- Tests pass on the base commit → they check nothing: the iteration is
  unsuccessful, tester is not called, and the iteration's review (which
  the next planner gets) says so and lists the test files and test names.
- Tests fail or don't compile on the base commit → the expected red
  result; the iteration continues to tester.
- No test files in the diff → the iteration is unsuccessful ("worker wrote
  no tests"), unless the task spec contains the line
  `<!-- diadoche: no-tests -->`, which skips the check. (`QUEUE.md` is
  rebuilt from `STATUS.md`, so the marker belongs in the spec; a trailing
  ` [no-tests]` on a queue line is tolerated and ignored.)
- Test files that existed before the task were changed but are not listed
  in the plan's `## Files` → tester's prompt gets an extra item asking it to
  check that those existing tests were not weakened.

The step is skipped in `--dry-run` and for a repository without commits.

## Worktrees

By default (`--worktree`) every task runs in its own git worktree, so an
unfinished or failed task never leaves anything behind in the main working
tree:

1. When a task starts, diadoche creates branch `diadoche/<NNN>-<slug>` from
   the current HEAD of the branch checked out in the main working tree, and
   a worktree for it in `.claude/diadoche/worktrees/<NNN>-<slug>`.
   `<NNN>-<slug>` comes from the spec file name, as in the
   `diadoche/failed/...` branch names.
2. planner, worker and tester all run with that worktree as their working
   directory, and tester's diff is computed inside it. diadoche's
   bookkeeping (`spec/`, `STATUS.md`, `QUEUE.md`, `usage.jsonl`, plans and
   reviews) stays in the main working tree, as before.
3. `DONE`: the task's changes (except `.claude/diadoche/` and
   `.claude/agents/`) are committed on its branch, with the task's heading
   (the spec's first line without `# ` and the number) as the message. The
   branch is then merged into the current branch: a fast-forward when
   possible, otherwise a merge commit. After that the worktree is removed
   (`git worktree remove`) and the branch is deleted. The bookkeeping is
   committed separately as `diadoche: complete <spec>`.
4. Merge conflict: the merge is aborted (`git merge --abort`), so the main
   branch keeps no trace of it. The task becomes `BLOCKED`, with the reason
   in the Status cell of `STATUS.md` (`BLOCKED: <reason>`); its worktree
   and branch are kept for a human to resolve, and the run moves on to the
   next task. `BLOCKED` tasks are never re-queued (like `FAILED` ones).
5. `FAILED`: the task's changes are committed on its branch, the worktree
   is removed and the branch is renamed to `diadoche/failed/<NNN>-<slug>`
   (with a `-2`, `-3`, ... suffix if that name is taken).
   `spec/NNN_FAILED.md` names the branch.

A usage-limit error pauses the task in place (see "Usage limits and
pauses"); a run stopped while a task is pending leaves its worktree in
place, and the next run resumes the task in it. `.claude/diadoche/worktrees/` is added to
`.git/info/exclude` automatically, so the main tree never sees the
worktrees as untracked files.

Before the run starts, diadoche checks the main working tree. If it has
uncommitted changes outside `.claude/diadoche/` and `.claude/agents/`, the
run exits with an error listing them and no agent is started. diadoche
never runs `git stash`: commit the changes first, or pass `--allow-dirty`
to skip the check (or `--worktree=false`). A repository without any commit
yet has nothing to branch from, so such a run falls back to
`--worktree=false`.

## Parallel tasks (`--parallel N`)

With `--parallel N` (default `1`, which is the one-task-at-a-time loop) up
to N tasks run at the same time, each in its own worktree and branch, each
with its own iterations. `--parallel` greater than 1 requires
`--worktree`.

- A task is ready when every task its `Depends-on` line names is `DONE`
  and merged. A spec without a `Depends-on` line still waits for the task
  before it, so it never runs alongside it; `Depends-on: none` is what lets
  tasks run in parallel. When fewer than N tasks are ready, only those run;
  each merge re-checks the graph for tasks it unblocked.
- Everything that touches the main working tree, the current branch or
  the spec directory as a whole is done by one owner: merges happen one at
  a time, in the order the tasks finished, and the bookkeeping commits, the
  `--on-task-done` hooks and the ADR step run after each merge, one task at
  a time.
- A merge conflict re-executes the task once from the new `HEAD`: its first
  attempt is kept on `diadoche/conflict/<NNN>-<slug>`, its plan and reviews
  are archived as `spec/<NNN>_ATTEMPT1_IMPL_PLAN.md` and
  `spec/<NNN>_ATTEMPT1_REVIEW_<M>.md` (planner and worker get them as
  history), and its iterations run again up to `--ITER`. A second conflict
  makes the task `BLOCKED`, with a reason naming the conflict and the kept
  branch, and the tasks that depend on it `BLOCKED` too. Both are `reexec`
  and `merge_blocked` events in `run.log`.
- One task failing does not stop the others. Three tasks failing with no
  task finishing in between still end the run, once the tasks in flight
  have finished and been merged or preserved.
- Every log line of a task starts with `[<NNN>-<slug>]`.
- `run-state.json` lists every active task; a restarted run resumes all of
  them, with whatever `--parallel` it is given.
- `SIGINT`/`SIGTERM` stops every task at its next step; `--shutdown-timeout`
  applies once to all in-flight calls together.
- `--dry-run` shows which tasks could run concurrently, in waves, and
  starts nothing; the merge order depends on which task finishes first.

## FAILED tasks (`--worktree=false`)

This section describes the in-place mode, `--worktree=false`. Every task starts from a clean working tree, and the work of a failed task
isn't lost. When a task becomes `FAILED` (iterations ran out, an agent call
errored, `--max-call-cost` or `--max-cost-per-task` was exceeded, ...),
diadoche parks its changes:

1. Every change since the task's diff base (modified, deleted and new
   non-ignored files), except `.claude/diadoche/` and `.claude/agents/`, is
   committed to a new branch `diadoche/failed/<NNN>-<slug>`, created from
   the diff base. The commit is built in a temporary index, so the current
   branch, HEAD and the index stay where they are. If that branch name is
   taken (the task failed before), a `-2`, `-3`, ... suffix is added.
2. Those files are reverted in the working tree: modified and deleted ones
   are restored, new ones are removed.
3. `spec/NNN_FAILED.md` is written: the reason, the branch name, the
   `git diff --stat` of what was parked, the last review, and how to bring
   the changes back (`git checkout diadoche/failed/... -- <paths>`, or
   `git merge diadoche/failed/...`).
4. The task's bookkeeping (plan, reviews, `_FAILED.md`, `STATUS.md`,
   `QUEUE.md`, `usage.jsonl`) is committed to the current branch as
   `diadoche: FAILED <spec>`, so history records the attempt without the
   failed code.

If the task changed no code, no branch is created, and `_FAILED.md` says
so. Parking logs one line with the branch name and the number of files.

A claude usage-limit error is not a failure: the task pauses in place (see
"Usage limits and pauses"). Nothing is parked, and if the run is stopped
meanwhile, the task stays pending with its changes in the working tree, so
the next run resumes it.

With `--worktree=false`, before a task starts from scratch, diadoche checks the working tree. If
there are uncommitted changes outside `.claude/diadoche/` and
`.claude/agents/`, it doesn't start the task and exits with an error that
lists the files: if the task failed, they would be parked along with the
task's own. Commit or stash them, or pass `--allow-dirty`. With
`--allow-dirty`, the files that were already changed before the task
started are never parked or reverted, so only the task's own changes are
parked. A task resumed after a restart (it has a `spec/NNN_BASE`, a plan or
reviews on disk) skips the check, because the changes in the tree are its
own.

## Usage accounting

Agents are invoked with `--output-format json`, so each call returns a JSON
result object: the agent's text answer in its `result` field (that is what
the rest of diadoche works with; the raw stdout is kept as a fallback when
the JSON can't be read) plus the call's token usage and cost.

Every agent call's model, token usage (input/output/cache) and cost is
appended as one JSON line to `.claude/diadoche/usage.jsonl`, along with the
task, agent name, iteration, duration and the `claude` process's exit code
(`0` on success, the CLI's own status when it exits non-zero, `-1` when the
process never produced one — it couldn't start, or was killed by
`--max-call-minutes`). A line that couldn't be parsed is still recorded
(zeroed) and logs a warning; usage accounting never fails the run.

Once a run finishes, a summary is printed: the run total, a breakdown by
agent and by task, and the cache-read share of all input tokens — that is
`cache_read_input_tokens` over `input_tokens + cache_creation_input_tokens
+ cache_read_input_tokens` (output tokens excluded), the one number that
says whether prompt caching is working.

## Run logs and artifacts

Every run (except `--dry-run`) gets its own directory
`.claude/diadoche/runs/<run-id>/`, where `<run-id>` is the start time as
`YYYYMMDD-HHMMSS` (with a `-2`, `-3`... suffix on a collision):

```
.claude/diadoche/runs/<run-id>/
├── run.log
└── <NNN>-<slug>/
    ├── iter-<N>-IMPL_PLAN.md      copy of the plan of iteration N
    ├── iter-<N>-REVIEW.md         copy of the review of iteration N
    ├── iter-<N>-<agent>.stdout    raw stdout of the call (claude's JSON reply)
    ├── iter-<N>-<agent>.stderr    raw stderr of the call
    ├── iter-0-adr-writer.stdout   raw output (and .stderr) of the --adr step's agent call
    ├── final.diff                 the task's changes against its base commit
    └── decisions.json             decisions worker reported in the DONE iteration
                                   (JSON array of {title, context, decision, alternatives})
```

`run.log` has one line per event, written without buffering, so
`tail -f .claude/diadoche/runs/*/run.log` follows the run live. Each line is
also echoed to stdout as `event: ...`:

```
2026-10-08T12:00:00.123+02:00 task=001_example.md iter=1 agent=planner event=agent_end duration=1m2.5s exit=0 cost=$0.4210 tokens=81234 result=ok
```

Events: `run_start`, `task_start`, `status` (every STATUS.md change),
`agent_start`, `agent_end` (duration, exit code, cost, tokens), `verdict`
(tester's DONE/NOT_DONE), `plan_rejected`, `empty_diff`, `redgreen_failed`,
`decisions` (count of decisions worker reported), `decisions_missing`
(worker gave no `## Decisions` section; not a failure), `pause`, `resume`, `commit`, `merge`, `merge_blocked`, `park`, `task_end`
(final status), `run_end`, and `warning` when an artifact couldn't be
written. A failed write never stops the run. A repeated call of the same
agent in one iteration (a retry after a limit pause) gets a `-2`, `-3`...
suffix. `final.diff` is written once the task ends, whatever the outcome;
`decisions.json` only for a DONE task (`[]` when worker reported none).

The `runs/` directory is added to `.git/info/exclude` of the project, so
diadoche's own commits never pick it up.

## Prompt caching

All agents of a run get the same shared prompt prefix — byte for byte — as
their system prompt (`claude --append-system-prompt`), with their role
instructions and the call's data following in the user turn, in that order.
The API's prompt cache only matches a request prefix exactly and only within
one model, so that identity is what lets planner → worker → tester read the
common context from the cache instead of paying for it again.

The prefix text is built into the binary; `--initialize` also writes it to
`.claude/diadoche/prefix.md`, where a project can override it. Keep anything
that varies between calls out of it (timestamps, iteration numbers, task
ids), or the cache misses every time.

With the default models, tester runs on `claude-sonnet-5-5` while
planner/worker run on `claude-opus-5-5`, and a cache entry is per model —
point all three at one model to have them share it. See
[docs/prompt-caching.md](docs/prompt-caching.md) for the full list of what
is unified, what cannot be, and how to check `cache_read` on a real run.

## Restarting after usage limits (`diadoche-restarter`)

`diadoche-restarter` runs diadoche and brings it back once the claude
account's usage window reopens:

```
diadoche-restarter -- --dir /path/to/project --ITER 5
```

Everything after `--` is passed to diadoche unchanged. The restarter
passes diadoche's output through and watches it for the claude CLI's
usage-limit error (`You've hit your session limit · resets 7pm
(Europe/Moscow)`, or the legacy `usage limit reached|<epoch>`). When it
sees one, it:

1. stops diadoche and any claude call still running right away, so
   diadoche doesn't go on to fail the rest of the queue against an
   exhausted account;
2. removes every task that isn't `DONE` from `spec/STATUS.md`, except
   `BLOCKED` tasks (they wait for a human to resolve their merge),
   `WAITING` tasks (they wait for a human's answer in `QUESTIONS.md`) and
   `FAILED` tasks that diadoche already parked (they have a
   `spec/NNN_FAILED.md`). Otherwise the next run could see the interrupted
   task as `FAILED` and never pick it up. The next sync re-adds those tasks as `QUEUED`, and each one
   resumes from the plan and reviews already on disk;
3. sleeps until the reset time from the error message plus
   `--reset-buffer`, or `--fallback-wait` if the message has no reset time.
   It re-checks the wall clock every 30s, so a Mac that slept through the
   reset time restarts within 30s of waking up;
4. starts diadoche again with the same flags.

Only a usage-limit error triggers a restart. For any other exit, the
restarter exits with diadoche's own exit status. Spend limits and team
budgets are not restarted, because waiting doesn't lift them.

**Working together with diadoche's own pause.** diadoche now waits out
usage and rate limits in-process (see "Usage limits and pauses"), and the
two schemes don't conflict without any extra flag: diadoche's pause log
lines are deliberately worded so they are never a claude limit message
(`001_x.md iter 1/3: [planner] paused: claude usage window exhausted
(until 2026-10-08T19:00:00Z), retrying the same call in 4h0m0s`), and the
claude error text of a refused call is never printed. So the restarter
does not kill a diadoche that is pausing. It stays a fallback for limit
errors that escape the pause and for crashes. When it does reset
`STATUS.md`, it treats `PAUSED` like `IN_PROGRESS`: the task is removed
and re-queued by the next run.

Flags:

- `--diadoche-bin`: the diadoche binary (default: the `diadoche` next to
  `diadoche-restarter`, else `diadoche` on `$PATH`).
- `--reset-buffer`: extra wait after the reported reset time (default `2m`).
- `--fallback-wait`: wait when the reset time can't be parsed (default `30m`).
- `--max-restarts`: give up after this many restarts (default `0` =
  unlimited).
