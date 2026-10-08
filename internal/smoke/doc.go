// Package smoke runs a project's optional smoke test: it boots the
// application, waits until it answers, probes it and tears it down again,
// so "the tests pass" can be backed by "the app comes up and answers".
// The package knows nothing about the orchestrator; it only parses the
// smoke file and executes it.
//
// # The smoke file
//
// The file lives at <project>/.claude/diadoche/smoke.yaml (FileName,
// PathIn). Every key is optional, order is irrelevant, but at least one
// step must be present — a file with no step is an error, not a no-op:
//
//	up: docker compose up -d
//	wait:
//	  url: http://localhost:8080/healthz
//	  timeout: 60s
//	  interval: 1s
//	checks:
//	  - cmd: ./scripts/smoke.sh
//	    timeout: 30s
//	  - url: http://localhost:8080/api/ping
//	    status: 200
//	browser:
//	  - http://localhost:8080/
//	  - http://localhost:8080/login
//	down: docker compose down -v
//
// The keys:
//
//   - up, down: a shell command, run with `sh -c` in the project
//     directory. The canonical form is a plain string; the map form
//     `up: {cmd: ..., timeout: 10m}` (flow or block) is accepted too, with
//     only the keys cmd (required) and timeout. Default timeouts: up 5m,
//     down 2m.
//   - wait: exactly one of url or cmd, plus timeout (default 60s) and
//     interval (default 1s). A URL is ready on any 2xx response, a command
//     on exit code 0. The first probe runs at once, then one every
//     interval; each probe is bounded by the time left. Not being ready
//     by the timeout fails the step.
//   - checks: a list of items, each exactly one of cmd (optional timeout,
//     default 60s; passes on exit code 0) or url (optional status, default
//     200, which the response must match exactly; optional timeout,
//     default 30s). Checks fail fast: the first failing item fails the run
//     and the rest are skipped. Results name items checks[1], checks[2], …
//   - browser: a list of http/https URLs, opened by the browser command
//     (below).
//
// Durations are time.ParseDuration strings and must be positive.
//
// # A deliberate YAML subset
//
// The parser is hand-written (no YAML dependency) and accepts exactly:
// a top-level block map of keys; scalar values; one level of nested block
// map; one-line flow maps `{k: v, ...}` with scalar values; block lists of
// scalars; block lists of inline (`- {k: v}`) or block (`- k: v` plus
// continuation lines) maps whose values are scalars; `#` comments
// (full-line, or after a space outside quotes); plain, 'single-quoted'
// (a doubled quote is a quote) and "double-quoted" (escapes \" \\ \n \t) scalars. A list
// may start at the indentation of its key. A plain value runs to the end of
// the line, so `cmd: echo a: b` is the command "echo a: b".
//
// Everything else is rejected with an error naming the line and the key:
// tabs in indentation, document markers and directives (---, ..., %),
// anchors and aliases (&, *), tags (!), block scalars (|, >), flow
// sequences ([a, b]), multi-line values, nesting deeper than the above,
// duplicate keys, unknown keys, and a map or list where a scalar is
// expected (or the reverse).
//
// # Running
//
// Run executes up → wait → checks → browser, stopping at the first failed
// step and marking the rest skipped. down always runs when configured: on
// success, after a failed step, after a panic inside a step (recovered and
// reported as that step's failure) and when ctx is cancelled — it gets its
// own context, detached from ctx's cancellation and bounded by its timeout.
// A failed down is reported in Result.DownOK and only becomes
// Result.Failed when nothing failed before it. A cancelled ctx fails the
// step about to run (or the wait/browser step in progress) with
// "cancelled". The command runner, HTTP client and clock are injectable
// through Options.
//
// # The browser command
//
// Options.BrowserCmd is a shell command prefix. For the N-th browser URL
// (1-based) Run executes, in the project directory,
//
//	<BrowserCmd> '<url>' '<outdir>' '<outdir>/<NN>-<slug>.png'
//
// with each argument single-quoted, where NN is the two-digit index and
// slug is the URL's host and path lowercased, every run of characters other
// than [a-z0-9] replaced by "-", trimmed of "-", cut to 40 bytes ("page"
// when empty). outdir is Options.OutDir (relative to the project directory
// unless absolute), created before the first URL. The command must:
//
//   - load the URL and write a PNG screenshot to the third argument;
//   - print each browser console error as one line on stdout;
//   - exit 0 when the page loaded, non-zero when it did not;
//   - keep its own diagnostics out of stdout and stderr, which are captured
//     together: every non-empty output line of a successful run counts as
//     a console error.
//
// A page fails when the command exits non-zero or times out
// (Options.BrowserTimeout, default 60s) or no screenshot file exists
// afterwards. Console errors alone do not fail it. Every URL is visited
// even after a failure; the browser step fails if any page did. Without a
// BrowserCmd the browser step is skipped with a warning, not failed.
//
// # Caps and limits
//
// The default runner keeps the last 1 MiB of a command's output; a step
// stores the last 4000 bytes of it; at most 64 KiB of an HTTP body is read
// and its last 2000 bytes stored; a page keeps at most 20 console errors of
// 300 bytes each; Result.Summary is at most 8000 bytes, output tails being
// shortened first. Commands run through redgreen.RunCommandLimit, which
// kills only the shell on timeout, not its process group; children still
// holding the output pipes are cut off after a short wait delay.
package smoke
