# Diadoche

Loop-engineering CLI: drives an autonomous `planner -> worker -> tester`
cycle on top of independent `claude --dangerously-skip-permissions`
processes, until a task becomes `DONE` (or iterations run out).

## Build

```
go build -o diadoche ./cmd/diadoche
```

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
   - **planner** (model `claude-fable-5-1`) — writes `spec/NNN_IMPL_PLAN.md`;
   - **worker** (model `claude-opus-5`) — implements the plan in the repo;
   - **tester** (model `claude-opus-5`) — writes `spec/NNN_REVIEW_M.md` and,
     if the task is genuinely done, sets `spec/STATUS.md` to `DONE` itself.
3. Once tester marks a task `DONE`, diadoche moves on to the next task in
   the queue. If iterations run out without `DONE`, diadoche itself sets
   `FAILED`.
4. From the 2nd iteration on, planner and worker also get the previous
   `spec/NNN_REVIEW_M.md` in their context.

Agent prompts and models are built into the binary (frontmatter `name` /
`model` / `description` plus a body — the system prompt), with per-project
overrides via `.claude/agents/<name>.md` (same format) — overriding a prompt
never requires rebuilding diadoche, only changing the built-in default does.

