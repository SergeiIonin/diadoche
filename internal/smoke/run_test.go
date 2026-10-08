package smoke

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"diadoche/internal/redgreen"
)

// fakeRunner records every script it is asked to run and answers from
// results (exit 0 with no output by default). It never starts a process.
type fakeRunner struct {
	calls    []string
	timeouts []time.Duration
	ctxErrs  []error
	results  map[string]redgreen.CommandResult
	panicOn  string
	// browser, when set, handles scripts starting with it: it gets the
	// unquoted args and returns the result (it may write the screenshot).
	browser   string
	onBrowser func(url, outdir, shot string) redgreen.CommandResult
}

func (f *fakeRunner) run(ctx context.Context, dir, script string, timeout time.Duration) redgreen.CommandResult {
	f.calls = append(f.calls, script)
	f.timeouts = append(f.timeouts, timeout)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.panicOn != "" && script == f.panicOn {
		panic("boom")
	}
	if f.browser != "" && strings.HasPrefix(script, f.browser+" ") {
		args := unquoteArgs(strings.TrimPrefix(script, f.browser+" "))
		return f.onBrowser(args[0], args[1], args[2])
	}
	if r, ok := f.results[script]; ok {
		return r
	}
	return redgreen.CommandResult{}
}

// unquoteArgs splits a line of shellQuote'd words.
func unquoteArgs(s string) []string {
	var out []string
	for _, w := range strings.Split(s, "' '") {
		w = strings.TrimSuffix(strings.TrimPrefix(w, "'"), "'")
		out = append(out, strings.ReplaceAll(w, `'\''`, "'"))
	}
	return out
}

// fakeClock advances only when slept on.
type fakeClock struct {
	now   time.Time
	slept time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	c.slept += d
	return ctx.Err()
}

func newClock() *fakeClock { return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func writeShot(path string) {
	_ = os.WriteFile(path, []byte("png"), 0o644)
}

func statuses(res Result) string {
	var parts []string
	for _, s := range res.Steps {
		parts = append(parts, s.Name+"="+string(s.Status))
	}
	return strings.Join(parts, " ")
}

func TestRunSuccessOrder(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "pong")
	}))
	defer srv.Close()

	cfg, err := Parse([]byte(`up: make up
wait: {cmd: probe}
checks:
  - cmd: ./smoke.sh
  - url: ` + srv.URL + `/ping
browser:
  - http://localhost:8080/
  - http://localhost:8080/login
down: make down
`))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	fr := &fakeRunner{browser: "shoot", onBrowser: func(url, outdir, shot string) redgreen.CommandResult {
		if outdir != out {
			t.Errorf("outdir = %q, want %q", outdir, out)
		}
		writeShot(shot)
		return redgreen.CommandResult{Output: "TypeError on " + url + "\n\n"}
	}}
	res := Run(context.Background(), Options{Config: cfg, Dir: t.TempDir(), Runner: fr.run, HTTP: srv.Client(),
		Clock: newClock(), BrowserCmd: "shoot", OutDir: out})

	want := []string{"make up", "probe", "./smoke.sh",
		"shoot 'http://localhost:8080/' '" + out + "' '" + filepath.Join(out, "01-localhost-8080.png") + "'",
		"shoot 'http://localhost:8080/login' '" + out + "' '" + filepath.Join(out, "02-localhost-8080-login.png") + "'",
		"make down"}
	if strings.Join(fr.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls =\n%s\nwant\n%s", strings.Join(fr.calls, "\n"), strings.Join(want, "\n"))
	}
	if hits.Load() != 1 {
		t.Errorf("url check hit the server %d times, want 1", hits.Load())
	}
	if got := statuses(res); got != "up=ok wait=ok checks[1]=ok checks[2]=ok browser=ok down=ok" {
		t.Errorf("steps = %s", got)
	}
	if !res.OK() || !res.DownRan || !res.DownOK || res.Failed != "" {
		t.Errorf("result = %+v", res)
	}
	if len(res.Pages) != 2 || res.Pages[1].ConsoleErrors[0] != "TypeError on http://localhost:8080/login" {
		t.Errorf("pages = %+v", res.Pages)
	}
	if fr.timeouts[0] != DefaultUpTimeout || fr.timeouts[len(fr.timeouts)-1] != DefaultDownTimeout {
		t.Errorf("timeouts = %v", fr.timeouts)
	}
}

func TestRunFailingCheck(t *testing.T) {
	cfg, err := Parse([]byte("up: make up\nchecks:\n  - cmd: ./smoke.sh\n  - cmd: ./other.sh\nbrowser:\n  - http://x/\ndown: make down\n"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{
		results: map[string]redgreen.CommandResult{"./smoke.sh": {ExitCode: 1, Output: strings.Repeat("noise\n", 2000) + "assertion failed: /api 404"}},
		browser: "shoot", onBrowser: func(string, string, string) redgreen.CommandResult {
			t.Error("browser command ran after a failed check")
			return redgreen.CommandResult{}
		},
	}
	res := Run(context.Background(), Options{Config: cfg, Runner: fr.run, Clock: newClock(), BrowserCmd: "shoot", OutDir: t.TempDir()})

	if res.Failed != "checks[1]" || res.OK() {
		t.Fatalf("Failed = %q", res.Failed)
	}
	f := res.FailedStep()
	if f.Label != "cmd: ./smoke.sh" || f.ExitCode != 1 || f.Detail != "exit 1" ||
		!strings.HasSuffix(f.OutputTail, "assertion failed: /api 404") || len(f.OutputTail) > outputTail+len("…") {
		t.Errorf("failed step = %+v", f)
	}
	if got := statuses(res); got != "up=ok checks[1]=failed checks[2]=skipped browser=skipped down=ok" {
		t.Errorf("steps = %s", got)
	}
	if last := fr.calls[len(fr.calls)-1]; last != "make down" || len(fr.calls) != 3 {
		t.Errorf("calls = %q", fr.calls)
	}
	if !res.DownRan || !res.DownOK {
		t.Errorf("down: ran %v ok %v", res.DownRan, res.DownOK)
	}
}

func TestRunFailingURLCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, strings.Repeat("x", 100<<10)+"stack trace end")
	}))
	defer srv.Close()
	cfg, err := Parse([]byte("checks:\n  - url: " + srv.URL + "/\n"))
	if err != nil {
		t.Fatal(err)
	}
	res := Run(context.Background(), Options{Config: cfg, HTTP: srv.Client(), Clock: newClock(), Runner: (&fakeRunner{}).run})
	f := res.FailedStep()
	if f == nil || f.HTTPStatus != 500 || f.Detail != "HTTP 500, want 200" || len(f.BodyTail) > bodyTail+len("…") || f.BodyTail == "" {
		t.Fatalf("failed step = %+v", f)
	}
}

func TestRunWaitNeverReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		fmt.Fprint(w, "starting")
	}))
	defer srv.Close()
	for _, wait := range []string{"wait: {cmd: probe}", "wait: {url: " + srv.URL + "/healthz}"} {
		t.Run(wait, func(t *testing.T) {
			cfg, err := Parse([]byte(wait + "\nchecks:\n  - cmd: ./smoke.sh\nbrowser:\n  - http://x/\ndown: make down\n"))
			if err != nil {
				t.Fatal(err)
			}
			fr := &fakeRunner{results: map[string]redgreen.CommandResult{"probe": {ExitCode: 7, Output: "connection refused"}}}
			clock := newClock()
			start := time.Now()
			res := Run(context.Background(), Options{Config: cfg, Runner: fr.run, HTTP: srv.Client(), Clock: clock, BrowserCmd: "shoot", OutDir: t.TempDir()})
			if time.Since(start) > 5*time.Second {
				t.Errorf("Run took %v: it slept for real", time.Since(start))
			}
			f := res.FailedStep()
			if res.Failed != "wait" || f == nil || !strings.Contains(f.Detail, "not ready after 1m0s") {
				t.Fatalf("Failed = %q, step = %+v", res.Failed, f)
			}
			if strings.Contains(wait, "cmd") {
				if f.ExitCode != 7 || f.OutputTail != "connection refused" {
					t.Errorf("last probe = %+v", f)
				}
			} else if f.HTTPStatus != 503 || f.BodyTail != "starting" {
				t.Errorf("last probe = %+v", f)
			}
			if clock.slept < 60*time.Second {
				t.Errorf("fake clock slept %v, want >= 60s", clock.slept)
			}
			if got := statuses(res); got != "wait=failed checks[1]=skipped browser=skipped down=ok" {
				t.Errorf("steps = %s", got)
			}
			if last := fr.calls[len(fr.calls)-1]; last != "make down" {
				t.Errorf("last call = %q", last)
			}
		})
	}
}

func TestRunCancelledCtx(t *testing.T) {
	cfg, err := Parse([]byte("up: make up\nchecks:\n  - cmd: ./smoke.sh\ndown: make down\n"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fr := &fakeRunner{}
	res := Run(ctx, Options{Config: cfg, Runner: fr.run, Clock: newClock()})
	if res.Failed != "up" || !strings.Contains(res.Steps[0].Detail, "cancelled") {
		t.Errorf("steps = %+v", res.Steps)
	}
	if len(fr.calls) != 1 || fr.calls[0] != "make down" || fr.ctxErrs[0] != nil {
		t.Errorf("calls = %q, ctx errs = %v; want only down with a live ctx", fr.calls, fr.ctxErrs)
	}
	if !res.DownRan || !res.DownOK {
		t.Errorf("down: ran %v ok %v", res.DownRan, res.DownOK)
	}

	fr = &fakeRunner{results: map[string]redgreen.CommandResult{"make down": {ExitCode: 2, Output: "network busy"}}}
	res = Run(ctx, Options{Config: cfg, Runner: fr.run, Clock: newClock()})
	if res.Failed != "up" || !res.DownRan || res.DownOK {
		t.Errorf("Failed = %q, down ran %v ok %v", res.Failed, res.DownRan, res.DownOK)
	}
	sum := res.Summary()
	for _, want := range []string{"FAILED at up", "cancelled", "down output (tail):", "network busy", "down: FAILED"} {
		if !strings.Contains(sum, want) {
			t.Errorf("Summary lacks %q:\n%s", want, sum)
		}
	}
}

func TestRunDownFailureAlone(t *testing.T) {
	cfg, err := Parse([]byte("up: make up\ndown: make down\n"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{results: map[string]redgreen.CommandResult{"make down": {ExitCode: 1}}}
	res := Run(context.Background(), Options{Config: cfg, Runner: fr.run, Clock: newClock()})
	if res.Failed != "down" || res.OK() || res.DownOK {
		t.Errorf("result = %+v", res)
	}
}

func TestRunPanicInStep(t *testing.T) {
	cfg, err := Parse([]byte("checks:\n  - cmd: ./smoke.sh\n  - cmd: ./b\ndown: make down\n"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{panicOn: "./smoke.sh"}
	res := Run(context.Background(), Options{Config: cfg, Runner: fr.run, Clock: newClock()})
	if res.Failed != "checks[1]" || !strings.Contains(res.Steps[0].Detail, "panic: boom") {
		t.Errorf("steps = %+v", res.Steps)
	}
	if got := statuses(res); got != "checks[1]=failed checks[2]=skipped down=ok" {
		t.Errorf("steps = %s", got)
	}
	if !res.DownRan || fr.calls[len(fr.calls)-1] != "make down" {
		t.Errorf("down did not run: %q", fr.calls)
	}
}

func TestRunNilConfig(t *testing.T) {
	if res := Run(context.Background(), Options{}); res.Failed != "config" {
		t.Errorf("Failed = %q", res.Failed)
	}
}

func TestRunBrowserWithoutCommand(t *testing.T) {
	cfg, err := Parse([]byte("browser:\n  - http://x/\n"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{}
	res := Run(context.Background(), Options{Config: cfg, Runner: fr.run, Clock: newClock()})
	if !res.OK() || res.Steps[0].Status != StatusSkipped || len(fr.calls) != 0 {
		t.Errorf("result = %+v, calls %q", res, fr.calls)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no browser command") {
		t.Errorf("warnings = %q", res.Warnings)
	}
}

func TestRunBrowserPages(t *testing.T) {
	cfg, err := Parse([]byte("browser:\n  - http://localhost:8080/\n  - http://localhost:8080/broken\n  - http://localhost:8080/noshot\ndown: make down\n"))
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "shots") // created by Run
	fr := &fakeRunner{browser: "shoot --headless", onBrowser: func(url, outdir, shot string) redgreen.CommandResult {
		switch {
		case strings.HasSuffix(url, "/broken"):
			return redgreen.CommandResult{ExitCode: 3, Output: "net::ERR_CONNECTION_REFUSED"}
		case strings.HasSuffix(url, "/noshot"):
			return redgreen.CommandResult{}
		}
		writeShot(shot)
		var b strings.Builder
		for i := range 30 {
			fmt.Fprintf(&b, "  console error %d %s\n", i, strings.Repeat("y", 500))
		}
		return redgreen.CommandResult{Output: b.String()}
	}}
	res := Run(context.Background(), Options{Config: cfg, Runner: fr.run, Clock: newClock(), BrowserCmd: "shoot --headless", OutDir: out})

	if res.Failed != "browser" || res.Steps[0].Detail != "2 of 3 pages failed" {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if len(res.Pages) != 3 {
		t.Fatalf("pages = %+v", res.Pages)
	}
	ok := res.Pages[0]
	if !ok.OK || ok.Screenshot != filepath.Join(out, "01-localhost-8080.png") {
		t.Errorf("page 1 = %+v", ok)
	}
	if len(ok.ConsoleErrors) != maxConsoleErrors+1 || ok.ConsoleErrors[maxConsoleErrors] != "(+10 more)" ||
		!strings.HasPrefix(ok.ConsoleErrors[0], "console error 0 ") || len(ok.ConsoleErrors[0]) > maxConsoleErrorLen+len("…") {
		t.Errorf("console errors = %d, first %q", len(ok.ConsoleErrors), ok.ConsoleErrors[0])
	}
	if p := res.Pages[1]; p.OK || p.Detail != "exit 3" || p.OutputTail != "net::ERR_CONNECTION_REFUSED" || p.Screenshot != "" {
		t.Errorf("page 2 = %+v", p)
	}
	if p := res.Pages[2]; p.OK || !strings.Contains(p.Detail, "no screenshot written to "+filepath.Join(out, "03-localhost-8080-noshot.png")) {
		t.Errorf("page 3 = %+v", p)
	}
	if got := res.Screenshots(); len(got) != 1 || got[0] != ok.Screenshot {
		t.Errorf("Screenshots = %q", got)
	}
	if !res.DownRan {
		t.Error("down did not run")
	}
}

func TestSlugAndShellQuote(t *testing.T) {
	cases := map[string]string{
		"http://localhost:8080/":              "localhost-8080",
		"https://Example.com/Login?x=1":       "example-com-login",
		"http://a/" + strings.Repeat("b", 60): "a-" + strings.Repeat("b", 38),
		"http://[::1]/":                       "1",
		"http://-/":                           "page",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := shellQuote("http://x/?q='; rm -rf /'"); got != `'http://x/?q='\''; rm -rf /'\'''` {
		t.Errorf("shellQuote = %s", got)
	}
}

func TestRunBrowserPageName(t *testing.T) {
	cfg, err := Parse([]byte("browser:\n  - http://localhost:8080/\n  - http://localhost:8080/noshot\n"))
	if err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{browser: "shoot", onBrowser: func(url, outdir, shot string) redgreen.CommandResult {
		if !strings.HasSuffix(url, "/noshot") {
			writeShot(shot)
		}
		return redgreen.CommandResult{}
	}}
	res := Run(context.Background(), Options{Config: cfg, Runner: fr.run, Clock: newClock(), BrowserCmd: "shoot", OutDir: t.TempDir()})
	if len(res.Pages) != 2 {
		t.Fatalf("pages = %+v", res.Pages)
	}
	if p := res.Pages[0]; p.Name != "01-localhost-8080" || filepath.Base(p.Screenshot) != p.Name+".png" {
		t.Errorf("page 1 = %+v", p)
	}
	if p := res.Pages[1]; p.Name != "02-localhost-8080-noshot" || p.Screenshot != "" {
		t.Errorf("page 2 = %+v", p)
	}
}
