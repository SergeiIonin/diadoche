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
prompt under `.claude/agents/*.md` so they're easy to customize. It never
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
- `--dry-run` — don't invoke `claude`, just print what would be sent to
  each agent.
- `--claude-bin` — path to the `claude` binary (default `claude` on `$PATH`).
- `--preprocess` + `--task <path>` — preprocessor mode, see below.
- `--initialize <project-path>` — scaffold `.claude/` in a project and
  exit; ignores every other flag. See above.
- `--planner-model`, `--worker-model`, `--tester-model`,
  `--preprocessor-model` — override one agent's model. Priority:
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
- `--allow-dirty` — start a task even if the working tree already has
  uncommitted changes; those files are never parked if the task fails. See
  "FAILED tasks" below.
- `-h` / `-help` / `--help` — print the full man-page-style help and exit.

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
   and pushed onto `spec/QUEUE.md` (this run's work queue).
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
   - **worker** (model `claude-opus-5-5` by default) — implements the plan,
     working only from its `## Files` list instead of re-exploring the repo;
     if another file turns out to be needed, worker first appends it to
     `## Files` with a one-line justification and only then edits it;
   - **tester** (model `claude-sonnet-5-5` by default) — reviews a `git diff`
     of the worker's changes (see "Diff-scoped tester" below) plus the plan's
     test command, writes `spec/NNN_REVIEW_M.md`, and, if the task is
     genuinely done, sets `spec/STATUS.md` to `DONE` itself.
3. Once tester marks a task `DONE`, diadoche moves on to the next task in
   the queue. If iterations run out without `DONE`, diadoche itself sets
   `FAILED` and parks the task's changes on a side branch (see "FAILED
   tasks" below).
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
tester `claude-sonnet-5-5`, preprocessor `claude-opus-5`.

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

## FAILED tasks

Every task starts from a clean working tree, and the work of a failed task
isn't lost. When a task becomes `FAILED` (iterations ran out, an agent call
errored, `--max-call-cost` was exceeded, ...), diadoche parks its changes:

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

A claude usage-limit error is not a failure. Nothing is parked: the task
stays `IN_PROGRESS` with its changes in the working tree, and diadoche
exits, so `diadoche-restarter` (or the next manual run) resumes it.

Before a task starts from scratch, diadoche checks the working tree. If
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

Flags:

- `--diadoche-bin`: the diadoche binary (default: the `diadoche` next to
  `diadoche-restarter`, else `diadoche` on `$PATH`).
- `--reset-buffer`: extra wait after the reported reset time (default `2m`).
- `--fallback-wait`: wait when the reset time can't be parsed (default `30m`).
- `--max-restarts`: give up after this many restarts (default `0` =
  unlimited).
