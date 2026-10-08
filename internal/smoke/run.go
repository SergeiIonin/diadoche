package smoke

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"diadoche/internal/redgreen"
)

// Runner runs a shell script in dir with a timeout, like
// redgreen.RunCommand. Tests inject a fake.
type Runner func(ctx context.Context, dir, script string, timeout time.Duration) redgreen.CommandResult

// HTTPDoer sends an HTTP request; *http.Client satisfies it.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Clock tells the time and sleeps; Sleep returns early with ctx's error
// when ctx is done.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func defaultRunner(ctx context.Context, dir, script string, timeout time.Duration) redgreen.CommandResult {
	return redgreen.RunCommandLimit(ctx, dir, script, timeout, maxCapturedOutput)
}

// Options configures Run. Nil Runner, HTTP and Clock get the real
// implementations.
type Options struct {
	Config *Config
	Dir    string // working directory of every command
	Runner Runner
	HTTP   HTTPDoer
	Clock  Clock

	// BrowserCmd opens one browser URL (see the package doc for the
	// contract). Empty skips the browser step with a warning.
	BrowserCmd string
	// BrowserTimeout bounds one BrowserCmd run; 0 means
	// DefaultBrowserTimeout.
	BrowserTimeout time.Duration
	// OutDir receives the screenshots; relative paths are taken against
	// Dir. Required when BrowserCmd is set.
	OutDir string
}

type plannedStep struct {
	name, label string
	run         func(ctx context.Context) StepResult
}

type executor struct {
	opts  Options
	res   *Result
	run   Runner
	http  HTTPDoer
	clock Clock
}

// Run executes the smoke config: up → wait → checks → browser, stopping at
// the first failed step, and then down, which runs whatever happened —
// success, a failed step, a panic in a step or a cancelled ctx. Run never
// panics and always returns a Result.
func Run(ctx context.Context, opts Options) (res Result) {
	e := &executor{opts: opts, res: &res, run: opts.Runner, http: opts.HTTP, clock: opts.Clock}
	if e.run == nil {
		e.run = defaultRunner
	}
	if e.http == nil {
		e.http = &http.Client{}
	}
	if e.clock == nil {
		e.clock = realClock{}
	}
	start := e.clock.Now()
	cfg := opts.Config
	if cfg == nil {
		res.Failed = "config"
		res.Steps = append(res.Steps, StepResult{Name: "config", Status: StatusFailed, Detail: "no smoke config"})
		return res
	}

	plan := e.plan(cfg)
	done := 0
	defer func() {
		if p := recover(); p != nil && done < len(plan) {
			s := plan[done]
			e.record(s, StepResult{Status: StatusFailed, Detail: fmt.Sprintf("panic: %v", p)})
			done++
		}
		for _, s := range plan[done:] {
			e.record(s, StepResult{Status: StatusSkipped})
		}
		e.down(ctx, cfg.Down)
		res.Duration = e.clock.Now().Sub(start)
	}()
	for _, s := range plan {
		var sr StepResult
		switch {
		case res.Failed != "":
			sr = StepResult{Status: StatusSkipped}
		case ctx.Err() != nil:
			sr = cancelled(ctx)
		default:
			t0 := e.clock.Now()
			sr = s.run(ctx)
			sr.Duration = e.clock.Now().Sub(t0)
		}
		e.record(s, sr)
		done++
	}
	return res
}

// record appends a step's result under its planned name and label.
func (e *executor) record(s plannedStep, sr StepResult) {
	sr.Name, sr.Label = s.name, s.label
	e.res.Steps = append(e.res.Steps, sr)
	if sr.Status == StatusFailed && e.res.Failed == "" {
		e.res.Failed = s.name
	}
}

func cancelled(ctx context.Context) StepResult {
	return StepResult{Status: StatusFailed, Detail: fmt.Sprintf("cancelled: %v", ctx.Err())}
}

func (e *executor) plan(cfg *Config) []plannedStep {
	var plan []plannedStep
	if c := cfg.Up; c != nil {
		plan = append(plan, plannedStep{"up", c.Cmd, func(ctx context.Context) StepResult {
			return e.command(ctx, c.Cmd, c.Timeout)
		}})
	}
	if w := cfg.Wait; w != nil {
		label := "url: " + w.URL
		if w.Cmd != "" {
			label = "cmd: " + w.Cmd
		}
		plan = append(plan, plannedStep{"wait", label, func(ctx context.Context) StepResult { return e.wait(ctx, w) }})
	}
	for i, c := range cfg.Checks {
		plan = append(plan, plannedStep{fmt.Sprintf("checks[%d]", i+1), c.Label(), func(ctx context.Context) StepResult {
			return e.check(ctx, c)
		}})
	}
	if len(cfg.Browser) > 0 {
		label := fmt.Sprintf("%d URL(s)", len(cfg.Browser))
		plan = append(plan, plannedStep{"browser", label, func(ctx context.Context) StepResult {
			return e.browser(ctx, cfg.Browser)
		}})
	}
	return plan
}

// command runs a shell command step and turns its outcome into a result.
func (e *executor) command(ctx context.Context, script string, timeout time.Duration) StepResult {
	r := e.run(ctx, e.opts.Dir, script, timeout)
	if r.Passed() {
		return StepResult{Status: StatusOK}
	}
	return commandFailure(r, timeout)
}

func commandFailure(r redgreen.CommandResult, timeout time.Duration) StepResult {
	sr := StepResult{Status: StatusFailed, ExitCode: r.ExitCode, TimedOut: r.TimedOut,
		OutputTail: redgreen.Tail(r.Output, outputTail)}
	switch {
	case r.TimedOut:
		sr.Detail = fmt.Sprintf("timed out after %s", timeout)
	case r.Err != nil:
		sr.Detail = fmt.Sprintf("could not run: %v", r.Err)
	default:
		sr.Detail = fmt.Sprintf("exit %d", r.ExitCode)
	}
	return sr
}

// probeURL GETs u and returns its status and the tail of its body.
func (e *executor) probeURL(ctx context.Context, u string, timeout time.Duration) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	return resp.StatusCode, redgreen.Tail(string(body), bodyTail), nil
}

// wait polls the readiness probe until it succeeds or the timeout passes.
func (e *executor) wait(ctx context.Context, w *Wait) StepResult {
	deadline := e.clock.Now().Add(w.Timeout)
	for {
		if ctx.Err() != nil {
			return cancelled(ctx)
		}
		probeTimeout := max(deadline.Sub(e.clock.Now()), w.Interval)
		var last StepResult
		if w.Cmd != "" {
			r := e.run(ctx, e.opts.Dir, w.Cmd, probeTimeout)
			if r.Passed() {
				return StepResult{Status: StatusOK}
			}
			last = commandFailure(r, probeTimeout)
		} else {
			code, body, err := e.probeURL(ctx, w.URL, probeTimeout)
			if err == nil && code >= 200 && code < 300 {
				return StepResult{Status: StatusOK}
			}
			last = StepResult{Status: StatusFailed, HTTPStatus: code, BodyTail: body}
			if err != nil {
				last.Detail = fmt.Sprintf("request failed: %v", err)
			} else {
				last.Detail = fmt.Sprintf("HTTP %d", code)
			}
		}
		remaining := deadline.Sub(e.clock.Now())
		if remaining <= 0 {
			last.Detail = fmt.Sprintf("not ready after %s; last probe: %s", w.Timeout, last.Detail)
			return last
		}
		if err := e.clock.Sleep(ctx, min(w.Interval, remaining)); err != nil {
			return cancelled(ctx)
		}
	}
}

// check runs one checks item.
func (e *executor) check(ctx context.Context, c Check) StepResult {
	if c.Cmd != "" {
		return e.command(ctx, c.Cmd, c.Timeout)
	}
	code, body, err := e.probeURL(ctx, c.URL, c.Timeout)
	if err != nil {
		return StepResult{Status: StatusFailed, Detail: fmt.Sprintf("request failed: %v", err)}
	}
	if code != c.Status {
		return StepResult{Status: StatusFailed, HTTPStatus: code, BodyTail: body,
			Detail: fmt.Sprintf("HTTP %d, want %d", code, c.Status)}
	}
	return StepResult{Status: StatusOK}
}

// browser opens every URL with the browser command; see the package doc.
func (e *executor) browser(ctx context.Context, urls []BrowserURL) StepResult {
	if e.opts.BrowserCmd == "" {
		e.res.Warnings = append(e.res.Warnings, "browser skipped: no browser command configured")
		return StepResult{Status: StatusSkipped, Detail: "no browser command configured"}
	}
	outDir := e.opts.OutDir
	if outDir == "" {
		return StepResult{Status: StatusFailed, Detail: "no output directory for screenshots"}
	}
	if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(e.opts.Dir, outDir)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return StepResult{Status: StatusFailed, Detail: fmt.Sprintf("create output directory: %v", err)}
	}
	timeout := cmpOrDur(e.opts.BrowserTimeout, DefaultBrowserTimeout)
	failed := 0
	for i, u := range urls {
		if ctx.Err() != nil {
			return cancelled(ctx)
		}
		shot := filepath.Join(outDir, fmt.Sprintf("%02d-%s.png", i+1, slug(u.URL)))
		script := strings.Join([]string{e.opts.BrowserCmd, shellQuote(u.URL), shellQuote(outDir), shellQuote(shot)}, " ")
		r := e.run(ctx, e.opts.Dir, script, timeout)
		page := Page{URL: u.URL}
		if _, err := os.Stat(shot); err == nil {
			page.Screenshot = shot
		}
		switch {
		case !r.Passed():
			page.Detail = commandFailure(r, timeout).Detail
			page.OutputTail = redgreen.Tail(r.Output, outputTail)
		case page.Screenshot == "":
			page.Detail = "no screenshot written to " + shot
		default:
			page.OK = true
			page.ConsoleErrors = consoleErrors(r.Output)
		}
		if !page.OK {
			failed++
		}
		e.res.Pages = append(e.res.Pages, page)
	}
	if failed > 0 {
		return StepResult{Status: StatusFailed, Detail: fmt.Sprintf("%d of %d pages failed", failed, len(urls))}
	}
	return StepResult{Status: StatusOK}
}

func cmpOrDur(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// consoleErrors turns the browser command's output into console errors:
// every non-empty line, capped in count and length.
func consoleErrors(out string) []string {
	var errs []string
	more := 0
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if len(errs) == maxConsoleErrors {
			more++
			continue
		}
		if len(l) > maxConsoleErrorLen {
			l = cutHead(l, maxConsoleErrorLen) + "…"
		}
		errs = append(errs, l)
	}
	if more > 0 {
		errs = append(errs, fmt.Sprintf("(+%d more)", more))
	}
	return errs
}

// down runs the down command with its own context, detached from ctx's
// cancellation, so it runs even when the run was cancelled.
func (e *executor) down(ctx context.Context, c *Command) {
	if c == nil {
		return
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.Timeout)
	defer cancel()
	t0 := e.clock.Now()
	var sr StepResult
	func() {
		defer func() {
			if p := recover(); p != nil {
				sr = StepResult{Status: StatusFailed, Detail: fmt.Sprintf("panic: %v", p)}
			}
		}()
		sr = e.command(dctx, c.Cmd, c.Timeout)
	}()
	sr.Duration = e.clock.Now().Sub(t0)
	e.res.DownRan = true
	e.res.DownOK = sr.Status == StatusOK
	e.record(plannedStep{name: "down", label: c.Cmd}, sr)
}

// slug turns a URL into a file-name fragment: host and path lowercased,
// runs of anything but [a-z0-9] replaced with "-", at most 40 bytes.
func slug(raw string) string {
	s := raw
	if u, err := url.Parse(raw); err == nil {
		s = u.Host + u.Path
	}
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(s) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.TrimRight(out[:40], "-")
	}
	if out == "" {
		return "page"
	}
	return out
}

// shellQuote quotes s as one sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
