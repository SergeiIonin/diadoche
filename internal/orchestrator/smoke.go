package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"diadoche/internal/runlog"
	"diadoche/internal/smoke"
	"diadoche/internal/specs"
)

// smokeFailedHint closes the review of a failed smoke check.
const smokeFailedHint = "Fix the application (not the smoke file) so that the failed step passes: " +
	"run the failing command or URL locally with the up command running, " +
	"and plan a test that covers the failure.\n"

// smokeOutcome is what the smoke step decided: fail makes the iteration
// unsuccessful with review as the review written for the next planner;
// testerNote, when non-empty, is the smoke block for tester; interrupted
// means a stop was requested during the step (down has run by then).
type smokeOutcome struct {
	fail        bool
	review      string
	testerNote  string
	interrupted bool
	failedStep  string
}

// findSmokeFile returns the smoke file the task's run uses: the worktree's
// copy, else the main tree's; "" when neither exists. A nil ws looks only
// at the main tree.
func (o *Orchestrator) findSmokeFile(ws *workspace) (string, error) {
	var candidates []string
	if ws != nil && ws.dir != "" {
		candidates = append(candidates, smoke.PathIn(specs.NewLayout(ws.dir).Root))
	}
	if main := smoke.PathIn(o.Layout.Root); len(candidates) == 0 || candidates[0] != main {
		candidates = append(candidates, main)
	}
	for _, p := range candidates {
		_, err := os.Stat(p)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", nil
}

// smokeCheck runs between the red-green check and tester: it boots the
// application with the project's smoke.yaml in the task's work dir, checks
// it and tears it down, keeps the log, screenshots and console errors as
// artifacts and decides the outcome. Without a smoke.yaml it does nothing
// at all. An invalid smoke.yaml is returned as an error naming the file
// and line; nothing is started then.
func (o *Orchestrator) smokeCheck(ctx context.Context, specFile, num string, iter int, ws *workspace) (smokeOutcome, error) {
	path, err := o.findSmokeFile(ws)
	if err != nil || path == "" {
		return smokeOutcome{}, err
	}
	rel := o.relPath(path)
	cfg, err := smoke.Load(path)
	if err != nil {
		line := 0
		var se *smoke.Error
		if errors.As(err, &se) {
			cp := *se
			cp.Path = rel
			line, err = cp.Line, &cp
		}
		o.Log.Printf("%s iteration %d: smoke: %v", specFile, iter, err)
		o.event(specFile, iter, "", "smoke_invalid", runlog.F("file", rel), runlog.F("line", line))
		return smokeOutcome{}, err
	}

	wsDir := o.Cfg.Dir
	if ws != nil && ws.dir != "" {
		wsDir = ws.dir
	}
	if len(cfg.Browser) > 0 && o.Cfg.BrowserCmd == "" && o.smokeBrowserWarnOnce(specFile) {
		o.Log.Printf("warning: %s: smoke: browser step skipped: no --browser-cmd or DIADOCHE_BROWSER_CMD", specFile)
	}
	outDir := ""
	if len(cfg.Browser) > 0 && o.Cfg.BrowserCmd != "" {
		d, err := os.MkdirTemp("", "diadoche-smoke-")
		if err != nil {
			o.Log.Printf("warning: %s: smoke: create screenshot directory: %v", specFile, err)
		} else {
			outDir = d
			defer os.RemoveAll(d)
		}
	}

	o.Log.Printf("%s iteration %d: smoke: running %s in %s", specFile, iter, rel, wsDir)
	o.event(specFile, iter, "", "smoke_start", runlog.F("file", rel))
	sctx, cancel := o.gracefulCtx(ctx)
	defer cancel()
	res := smoke.Run(sctx, smoke.Options{
		Config: cfg, Dir: wsDir, BrowserCmd: o.Cfg.BrowserCmd, OutDir: outDir,
		Runner: o.smokeRunner, HTTP: o.smokeHTTP,
	})

	// The screenshots live in outDir, removed on return: keep them as
	// artifacts and name them by their artifact names from here on.
	artifactsDir := ""
	if o.run != nil {
		artifactsDir = o.relPath(o.run.TaskDir(taskID(specFile, num)))
		for i, p := range res.Pages {
			if p.Screenshot != "" {
				name := fmt.Sprintf("iter-%d-%s", iter, filepath.Base(p.Screenshot))
				o.copyArtifact(specFile, num, name, p.Screenshot)
				res.Pages[i].Screenshot = name
			}
			if len(p.ConsoleErrors) > 0 {
				o.saveArtifact(specFile, num, fmt.Sprintf("iter-%d-%s-console.txt", iter, p.Name),
					[]byte(strings.Join(p.ConsoleErrors, "\n")+"\n"))
			}
		}
		header := fmt.Sprintf("smoke file: %s\nwork dir: %s\n\n", rel, wsDir)
		o.saveArtifact(specFile, num, fmt.Sprintf("iter-%d-smoke.log", iter), []byte(header+res.Log()))
	} else {
		dropped := false
		for i := range res.Pages {
			if res.Pages[i].Screenshot != "" {
				res.Pages[i].Screenshot, dropped = "", true
			}
		}
		if dropped {
			res.Warnings = append(res.Warnings, "screenshots were taken but not kept: run artifacts are off")
		}
	}

	summary := res.Summary()
	o.Log.Printf("%s iteration %d: %s", specFile, iter, firstLine(summary))
	down := "not_run"
	if res.DownRan {
		down = map[bool]string{true: "ok", false: "failed"}[res.DownOK]
	}
	fields := []runlog.Field{runlog.F("result", "ok")}
	if !res.OK() {
		fields = []runlog.Field{runlog.F("result", "failed"), runlog.F("failed", res.Failed)}
	}
	fields = append(fields, runlog.F("duration", res.Duration.Round(time.Millisecond).String()), runlog.F("down", down))
	o.event(specFile, iter, "", "smoke_end", fields...)

	if o.stopRequested() {
		o.Log.Printf("%s iteration %d: smoke: stop requested; down ran before stopping: %t", specFile, iter, res.DownRan)
		return smokeOutcome{interrupted: true}, nil
	}
	if fatalSmokeStep(res.Failed) {
		return smokeOutcome{fail: true, failedStep: res.Failed, review: smokeFailureReview(rel, artifactsDir, iter, res)}, nil
	}
	return smokeOutcome{testerNote: smokeTesterNote(rel, artifactsDir, iter, res)}, nil
}

// fatalSmokeStep reports whether a failed step fails the iteration: up and
// wait (the application never came up) and any checks item. browser and
// down are only reported to tester.
func fatalSmokeStep(step string) bool {
	return step == "up" || step == "wait" || strings.HasPrefix(step, "checks[")
}

// smokeLogRef is the sentence pointing at the iteration's full smoke log,
// "" without run artifacts.
func smokeLogRef(artifactsDir string, iter int) string {
	if artifactsDir == "" {
		return ""
	}
	return fmt.Sprintf("Full log: %s/iter-%d-smoke.log", artifactsDir, iter)
}

// smokeFailureReview is the review written for the next planner when a
// fatal smoke step failed.
func smokeFailureReview(file, artifactsDir string, iter int, res smoke.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Smoke check failed at %s: the application did not pass %s; tester was not called.\n\n", res.Failed, file)
	b.WriteString(res.Summary())
	if ref := smokeLogRef(artifactsDir, iter); ref != "" {
		b.WriteString("\n" + ref + "\n")
	}
	b.WriteString("\n" + smokeFailedHint)
	return b.String()
}

// smokeTesterNote is the smoke block of tester's prompt. It always starts
// with "Smoke check", so tester can tell a passed check from none at all
// (no block).
func smokeTesterNote(file, artifactsDir string, iter int, res smoke.Result) string {
	clean := res.OK() && len(res.Warnings) == 0 && (!res.DownRan || res.DownOK)
	for _, p := range res.Pages {
		if !p.OK || len(p.ConsoleErrors) > 0 {
			clean = false
		}
	}
	ref := smokeLogRef(artifactsDir, iter)
	if clean {
		var steps []string
		for _, s := range res.Steps {
			steps = append(steps, s.Name)
		}
		note := fmt.Sprintf("Smoke check (%s): passed — the application came up and every configured step succeeded (%s).",
			file, strings.Join(steps, ", "))
		if ref != "" {
			note += " " + ref + "."
		}
		return note
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Smoke check (%s) — result of running the application after the tests passed:\n", file)
	b.WriteString(res.Summary())
	b.WriteString("Console errors from the browser step, failed pages and a failed `down` do not fail " +
		"the iteration by themselves; weigh them in your review.")
	if artifactsDir != "" {
		fmt.Fprintf(&b, " Screenshot and log paths are relative to the task's artifacts directory %s. %s.", artifactsDir, ref)
	}
	return b.String()
}

// smokeBrowserWarnOnce reports whether specFile has not got the "browser
// step skipped" warning yet in this run, and marks it as warned.
func (o *Orchestrator) smokeBrowserWarnOnce(specFile string) bool {
	sh := o.shared()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.smokeBrowserWarned[specFile] {
		return false
	}
	if sh.smokeBrowserWarned == nil {
		sh.smokeBrowserWarned = map[string]bool{}
	}
	sh.smokeBrowserWarned[specFile] = true
	return true
}
