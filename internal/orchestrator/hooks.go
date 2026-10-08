package orchestrator

// The --on-task-done hooks are diadoche's extension point for the moment a
// task is genuinely finished: DONE and its changes merged into (or, without
// --worktree, committed on) the branch the run started on. Each configured
// command runs through the shell in the repository root, with the task's
// id, title, base and merge commits and decisions file in its environment.
// A hook is never allowed to change the task's outcome or stop the run: a
// non-zero exit or a timeout is a warning, recorded in run.log and as the
// reason of the task's (still DONE) STATUS.md row. Whatever the hooks leave
// changed in the repository is committed separately.
//
// The hooks are one kind of task-done handler; the other is the built-in
// ADR step (--adr, see adr.go), which runs first, so the hooks already see
// its docs/adr/. All handlers share the trigger, the warning-only failure
// rule and the one on-task-done commit.

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"diadoche/internal/agents"
	"diadoche/internal/config"
	"diadoche/internal/gitutil"
	"diadoche/internal/runlog"
	"diadoche/internal/specs"
	"diadoche/internal/usage"
)

// maxHookOutputBytes caps each of a hook's stdout and stderr as kept in the
// task's artifacts; the rest is dropped and a truncation note appended.
const maxHookOutputBytes = 1 << 20

// hookWaitDelay bounds how long a killed hook's output pipes, still held by
// a grandchild outside its process group, may delay Wait.
const hookWaitDelay = 5 * time.Second

// cappedBuffer keeps the first limit bytes written to it. Write always
// reports success, so a chatty hook never gets EPIPE for it.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

// Bytes is what was kept, plus a note when the output was cut.
func (b *cappedBuffer) Bytes() []byte {
	if !b.truncated {
		return b.buf.Bytes()
	}
	return append(bytes.Clone(b.buf.Bytes()), fmt.Sprintf("\n[diadoche: output truncated at %d bytes]\n", b.limit)...)
}

// hookResult is how one hook command ended.
type hookResult struct {
	n        int
	exitCode int
	timedOut bool
	timeout  time.Duration
	// err is set when the command failed for another reason than a
	// non-zero exit (it could not be started, ...).
	err      error
	duration time.Duration
}

func (r hookResult) ok() bool { return !r.timedOut && r.err == nil && r.exitCode == 0 }

// outcome is the run.log result field: ok, failed or timeout.
func (r hookResult) outcome() string {
	switch {
	case r.timedOut:
		return "timeout"
	case !r.ok():
		return "failed"
	}
	return "ok"
}

// failure is the STATUS.md reason fragment of a failed hook, in
// diadoche's own words — never the hook's output.
func (r hookResult) failure() string {
	switch {
	case r.timedOut:
		return fmt.Sprintf("on-task-done hook %d timed out after %s", r.n, r.timeout)
	case r.err != nil:
		return fmt.Sprintf("on-task-done hook %d failed (could not run it)", r.n)
	}
	return fmt.Sprintf("on-task-done hook %d failed (exit %d)", r.n, r.exitCode)
}

// hookEnv is the DIADOCHE_* environment of specFile's hooks. Every variable
// is always present, possibly empty. A spec that can't be read only costs
// the title its fallback (the error is returned for a warning).
func (o *Orchestrator) hookEnv(specFile, num string, ws *workspace) ([]string, error) {
	specContent, err := os.ReadFile(filepath.Join(o.Layout.SpecDir, specFile))
	root, absErr := filepath.Abs(o.Cfg.Dir)
	if absErr != nil {
		root = o.Cfg.Dir
	}
	decisionsFile := ""
	if p := o.DecisionsPath(specFile); p != "" {
		if _, serr := os.Stat(p); serr == nil {
			if abs, aerr := filepath.Abs(p); aerr == nil {
				p = abs
			}
			decisionsFile = p
		}
	}
	var base, merge string
	if ws != nil {
		base, merge = ws.base, ws.mergeCommit
	}
	return []string{
		"DIADOCHE_TASK_ID=" + taskID(specFile, num),
		"DIADOCHE_TASK_TITLE=" + taskTitle(specContent, specFile),
		"DIADOCHE_REPO_ROOT=" + root,
		"DIADOCHE_BASE_COMMIT=" + base,
		"DIADOCHE_MERGE_COMMIT=" + merge,
		"DIADOCHE_DECISIONS_FILE=" + decisionsFile,
	}, err
}

// runHook runs the n-th hook command through the shell in dir with env on
// top of diadoche's environment, killing it and its process group after
// --hook-timeout.
func (o *Orchestrator) runHook(ctx context.Context, n int, command, dir string, env []string) (res hookResult, stdout, stderr []byte) {
	timeout := cmp.Or(o.Cfg.HookTimeout, config.DefaultHookTimeout)
	res = hookResult{n: n, timeout: timeout}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(hctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(cmd.Environ(), env...)
	agents.ConfigureProcessGroup(cmd)
	cmd.WaitDelay = hookWaitDelay
	outBuf := &cappedBuffer{limit: maxHookOutputBytes}
	errBuf := &cappedBuffer{limit: maxHookOutputBytes}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf

	start := time.Now()
	err := cmd.Run()
	res.duration = time.Since(start)
	res.timedOut = errors.Is(hctx.Err(), context.DeadlineExceeded)

	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0:
	case errors.As(err, &exitErr):
		res.exitCode = exitErr.ExitCode()
	default:
		res.exitCode = -1
		if !res.timedOut {
			res.err = err
		}
	}
	return res, outBuf.Bytes(), errBuf.Bytes()
}

// taskDoneInput is what a task-done handler works on.
type taskDoneInput struct {
	specFile, num string
	ws            *workspace
	uw            *usage.Writer
	// root is the absolute repository root, where the merge landed.
	root string
	// env is the hooks' DIADOCHE_* environment; nil without hooks.
	env []string
}

// handlerOutcome is how a task-done handler ended: an empty fragment is
// success; otherwise fragment is the STATUS.md reason fragment, in
// diadoche's own words, and err an optional detail for the log only.
type handlerOutcome struct {
	fragment string
	err      error
}

// taskDoneHandler is one step run once a task is DONE and merged.
type taskDoneHandler struct {
	// name is the handler in log lines and fragments ("on-task-done hook 1").
	name string
	// what is its run.log warning id ("on-task-done-1").
	what string
	run  func(ctx context.Context, in taskDoneInput) handlerOutcome
}

// hasTaskDoneHandlers reports whether a DONE task has anything to run.
func (o *Orchestrator) hasTaskDoneHandlers() bool {
	return o.Cfg.ADR || len(o.Cfg.OnTaskDone) > 0
}

// taskDoneHandlers are the handlers in the order they run: the built-in
// ADR step (with --adr), then every --on-task-done command as given.
func (o *Orchestrator) taskDoneHandlers() []taskDoneHandler {
	var hs []taskDoneHandler
	if o.Cfg.ADR {
		hs = append(hs, taskDoneHandler{name: "adr step", what: "adr", run: func(ctx context.Context, in taskDoneInput) handlerOutcome {
			return o.runADRStep(ctx, in.specFile, in.num, in.ws, in.uw)
		}})
	}
	for i, command := range o.Cfg.OnTaskDone {
		n := i + 1
		hs = append(hs, taskDoneHandler{
			name: fmt.Sprintf("on-task-done hook %d", n),
			what: fmt.Sprintf("on-task-done-%d", n),
			run: func(ctx context.Context, in taskDoneInput) handlerOutcome {
				return o.runShellHook(ctx, in, n, command)
			},
		})
	}
	return hs
}

// runShellHook runs the n-th --on-task-done command and keeps its output.
func (o *Orchestrator) runShellHook(ctx context.Context, in taskDoneInput, n int, command string) handlerOutcome {
	specFile, num := in.specFile, in.num
	o.Log.Printf("task %s: on-task-done hook %d: running %q", specFile, n, command)
	o.event(specFile, 0, "", "on_task_done_start", runlog.F("n", n))
	res, stdout, stderr := o.runHook(ctx, n, command, in.root, in.env)
	o.saveArtifact(specFile, num, fmt.Sprintf("on-task-done-%d.stdout", n), stdout)
	o.saveArtifact(specFile, num, fmt.Sprintf("on-task-done-%d.stderr", n), stderr)
	o.event(specFile, 0, "", "on_task_done", runlog.F("n", n), runlog.F("exit", res.exitCode),
		runlog.F("duration", res.duration.Round(time.Millisecond)), runlog.F("result", res.outcome()))
	if res.ok() {
		o.Log.Printf("task %s: on-task-done hook %d finished in %s", specFile, n, res.duration.Round(time.Millisecond))
		return handlerOutcome{}
	}
	return handlerOutcome{fragment: res.failure(), err: res.err}
}

// runTaskDoneHooks runs every task-done handler for specFile, a task that
// is DONE and merged — the ADR step, then the --on-task-done commands in
// the order given — then commits whatever they changed as
// "diadoche: on-task-done for <task-id>". Nothing here can fail the task or
// the run: a failed handler (or the commit) is logged, written to run.log
// and recorded as the reason of the task's DONE row in STATUS.md. failed
// reports whether that happened.
func (o *Orchestrator) runTaskDoneHooks(ctx context.Context, specFile, num string, ws *workspace, uw *usage.Writer) (failed bool) {
	id := taskID(specFile, num)
	root, err := filepath.Abs(o.Cfg.Dir)
	if err != nil {
		root = o.Cfg.Dir
	}
	in := taskDoneInput{specFile: specFile, num: num, ws: ws, uw: uw, root: root}
	if len(o.Cfg.OnTaskDone) > 0 {
		env, err := o.hookEnv(specFile, num, ws)
		if err != nil {
			o.Log.Printf("warning: task %s: couldn't read the spec for DIADOCHE_TASK_TITLE: %v", specFile, err)
			o.event(specFile, 0, "", "warning", runlog.F("what", "on-task-done env"), runlog.F("error", "spec unreadable"))
		}
		in.env = env
	}

	var fragments []string
	for _, h := range o.taskDoneHandlers() {
		if ctx.Err() != nil {
			o.Log.Printf("warning: task %s: interrupted; skipping %s and later ones", specFile, h.name)
			fragments = append(fragments, fmt.Sprintf("%s skipped (interrupted)", h.name))
			break
		}
		out := h.run(ctx, in)
		if out.fragment == "" {
			continue
		}
		if out.err != nil {
			o.Log.Printf("warning: task %s: %s: %v", specFile, out.fragment, out.err)
		} else {
			o.Log.Printf("warning: task %s: %s", specFile, out.fragment)
		}
		o.event(specFile, 0, "", "warning", runlog.F("what", h.what), runlog.F("error", out.fragment))
		fragments = append(fragments, out.fragment)
	}

	// Even after a cancelled ctx: what the handlers changed must not be
	// left lying around in the main tree.
	committed, err := gitutil.CommitAllExcluding(context.WithoutCancel(ctx), o.Cfg.Dir,
		"diadoche: on-task-done for "+id, relExcludeDirs(o.Cfg.Dir, o.Layout.Root)...)
	switch {
	case err != nil:
		o.Log.Printf("warning: task %s: couldn't commit the on-task-done changes: %v", specFile, err)
		o.event(specFile, 0, "", "warning", runlog.F("what", "on-task-done commit"), runlog.F("error", "commit failed"))
		fragments = append(fragments, "on-task-done commit failed")
	case committed:
		o.Log.Printf("task %s: committed the on-task-done changes", specFile)
		o.event(specFile, 0, "", "on_task_done_commit")
	}

	if len(fragments) == 0 {
		return false
	}
	reason := strings.Join(fragments, "; ")
	if err := o.setStatusReason(specFile, specs.StatusDone, reason); err != nil {
		o.Log.Printf("warning: task %s: couldn't record the on-task-done warning in STATUS.md: %v", specFile, err)
	} else {
		o.event(specFile, 0, "", "status", runlog.F("status", specs.StatusDone), runlog.F("reason", "on_task_done_failed"))
		o.commitBookkeeping(context.WithoutCancel(ctx), specFile, "diadoche: on-task-done warning for "+specFile)
	}
	return true
}
