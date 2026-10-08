# Prompt caching in diadoche

What diadoche does to get the API's prompt cache to work across its agents,
what it deliberately cannot do, and how to check whether it worked.

## Why

A diadoche run is a chain of short-lived `claude -p` processes: planner,
worker, tester, over and over. Each one re-sends a large, nearly identical
context. On a subscription that is the dominant cost — the usage window used
to run out after about one and a half tasks.

Anthropic's prompt cache can remove most of that, but only on its own terms:

- it matches a **prefix** of the request, in the order `tools` → `system` →
  `messages`, and the match must be **byte for byte**;
- the **model must be the same** — cache entries are per model;
- the entry lives for **5 minutes** from the start of the request that wrote
  it, refreshed every time it is read;
- the cached prefix must be at least **512 tokens** for the 5.x models
  (Claude Code's own system prompt alone is far above that).

The consequence that drives the whole design: the entire system prompt
(Claude Code's preset plus whatever `--append-system-prompt` adds) goes to
the API as a single block. There is no partial hit inside a block. One
differing byte anywhere in it — a role name at the end, for instance — and
nothing of it is cached.

## What diadoche unifies

Each agent call now looks like this:

```
claude --dangerously-skip-permissions --model <model> \
       --exclude-dynamic-system-prompt-sections \
       --append-system-prompt <SHARED PREFIX> \
       --output-format json \
       -p "<role instructions>\n\n<this call's data>"
```

- **The shared prefix is the only thing in the system prompt**, and it is
  byte-identical for planner, worker and tester within a run (and for
  preprocessor in a `--preprocess` run). It describes diadoche, the
  `spec/` layout and the rules that hold for every role.
- **Role instructions moved into the user turn**, ahead of the task data.
  The text of `.claude/agents/<name>.md` (or the built-in default) is
  unchanged: it is just delivered in the first user message instead of the
  system prompt.
- **The skills note moved into the prefix.** "Relevant skills available
  under `.claude/skills`: …" is the same for every call of a run, so it
  belongs in the cached half; it is no longer appended to each prompt.
- **`--exclude-dynamic-system-prompt-sections` is passed by default**, which
  moves per-machine sections (cwd, env info, memory paths, git status) out of
  the system prompt and into the first user message. Without it those
  sections can differ between calls and spoil the prefix.
  `--stable-system-prompt=false` turns it off for a `claude` CLI too old to
  know the flag.
- **The prefix text can be overridden** per project in
  `<project>/.claude/diadoche/prefix.md` (written by `--initialize`). Keep
  anything that varies between calls out of it — no timestamps, no iteration
  numbers, no task ids or task paths — or the cache misses on every call. A
  prefix file that exists but is blank is a startup error rather than a
  silently empty prefix.

The prefix is resolved once per run and the byte length and its source are
logged at startup (`prompt prefix: N bytes (source: …)`).

## What cannot be unified, and why

- **The role instructions cannot live in the system prompt.** Appending them
  there would make the whole system-prompt block differ per role, which
  defeats the caching entirely — the block is all-or-nothing.
- **`__SYSTEM_PROMPT_DYNAMIC_BOUNDARY__` is not an option.** That marker,
  which splits the system prompt into a static and a dynamic part, works
  only with `--system-prompt` / `--system-prompt-file`, i.e. by *replacing*
  Claude Code's preset — losing its built-in tool-use and safety guidance.
  diadoche does not replace the preset.
- **Different models do not share a cache.** With the default models, tester
  (`claude-sonnet-5-5`) cannot read what planner and worker
  (`claude-opus-5-5`) cached. To have all three share one cached prefix,
  give them the same model — `--planner-model` / `--worker-model` /
  `--tester-model`, or the matching `DIADOCHE_*_MODEL` variables.
- **The 5-minute TTL still applies.** The next agent has to start within the
  window, which in a normal run it does: the calls follow one another
  immediately.
- **The first user message is not shared.** Role instructions plus task data
  differ per call by construction, so they are paid for every time. They are
  the smaller half.
- **What Claude Code puts into the conversation itself is out of diadoche's
  hands**: `CLAUDE.md`, system reminders, the skill and subagent listings.
  diadoche never passes `CLAUDE.md` explicitly — the nested `claude` picks it
  up from the working directory on its own.
- **Tools and settings must match too.** The cache prefix starts at `tools`,
  so a differing MCP/tool configuration between calls breaks it before the
  system prompt is even reached. diadoche passes every agent the same flags,
  and all of them run in the same project directory.

## How to check

Watch `cache_read` in the run's own accounting:

- each agent call logs `tokens: … (in=… out=… cache_write=… cache_read=…)`;
- every call is also a line in `.claude/diadoche/usage.jsonl`
  (`cache_read_input_tokens`);
- the summary printed at the end of a run totals the same fields.

A healthy run shows a large `cache_write` on the first call of a model and
large `cache_read` on the following ones. This has to be judged on a real
run: the tests here never invoke a real model (they use fake `claude`
executables and fake runners), so they can only assert that the prefix is
identical and free of per-call data — not that the cache was actually hit.
