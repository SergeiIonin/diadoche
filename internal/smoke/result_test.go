package smoke

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"diadoche/internal/redgreen"
)

func failedRunWithShots(t *testing.T, checkOutput string, consoleLines int) Result {
	t.Helper()
	cfg, err := Parse([]byte("up: make up\nbrowser:\n  - http://localhost:8080/\n  - http://localhost:8080/login\nchecks:\n  - cmd: ./smoke.sh\ndown: make down\n"))
	if err != nil {
		t.Fatal(err)
	}
	// The failing check skips browser, so the pages a reviewer would see
	// from an earlier run are filled in by hand below.
	fr := &fakeRunner{results: map[string]redgreen.CommandResult{"./smoke.sh": {ExitCode: 1, Output: checkOutput}}}
	res := Run(context.Background(), Options{Config: cfg, Runner: fr.run, Clock: newClock(), BrowserCmd: "shoot", OutDir: t.TempDir()})
	errs := make([]string, consoleLines)
	for i := range errs {
		errs[i] = strings.Repeat("e", maxConsoleErrorLen)
	}
	res.Pages = []Page{
		{URL: "http://localhost:8080/", Screenshot: filepath.Join("/out", "01-localhost-8080.png"), OK: true, ConsoleErrors: errs},
		{URL: "http://localhost:8080/login", Screenshot: filepath.Join("/out", "02-localhost-8080-login.png"), OK: true, ConsoleErrors: errs},
	}
	return res
}

func TestSummaryFailedRun(t *testing.T) {
	res := failedRunWithShots(t, "step 1\nassertion failed: GET /api 404\n", 2)
	sum := res.Summary()
	for _, want := range []string{
		"smoke: FAILED at checks[1] (cmd: ./smoke.sh)",
		"checks[1]  failed",
		"browser    skipped",
		"checks[1] output (tail):",
		"  assertion failed: GET /api 404",
		"/out/01-localhost-8080.png",
		"/out/02-localhost-8080-login.png",
		"console errors:",
		"down: ok",
	} {
		if !strings.Contains(sum, want) {
			t.Errorf("Summary lacks %q:\n%s", want, sum)
		}
	}
	if res.String() != sum {
		t.Error("String() != Summary()")
	}
}

func TestSummaryBounded(t *testing.T) {
	huge := strings.Repeat("container log line\n", 10<<20/19) + "the real error"
	res := failedRunWithShots(t, huge, maxConsoleErrors+1)
	if f := res.FailedStep(); len(f.OutputTail) > outputTail+len("…") {
		t.Errorf("stored tail is %d bytes", len(f.OutputTail))
	}
	sum := res.Summary()
	if len(sum) > maxSummaryBytes {
		t.Errorf("Summary is %d bytes, cap %d", len(sum), maxSummaryBytes)
	}
	for _, want := range []string{"FAILED at checks[1]", "the real error", "/out/01-localhost-8080.png", "/out/02-localhost-8080-login.png", "down: ok"} {
		if !strings.Contains(sum, want) {
			t.Errorf("bounded Summary lacks %q", want)
		}
	}

	// A step table too big for the cap is cut with the marker.
	var steps []StepResult
	for range 500 {
		steps = append(steps, StepResult{Name: "checks[1]", Status: StatusFailed, Detail: strings.Repeat("d", 300)})
	}
	big := Result{Failed: "checks[1]", Steps: steps}.Summary()
	if len(big) > maxSummaryBytes || !strings.HasSuffix(big, "[smoke: summary truncated]\n") {
		t.Errorf("huge table: %d bytes, suffix %q", len(big), big[max(0, len(big)-40):])
	}
}

func TestSummaryOK(t *testing.T) {
	cfg, err := Parse([]byte("up: make up\nchecks:\n  - cmd: ./smoke.sh\ndown: make down\n"))
	if err != nil {
		t.Fatal(err)
	}
	sum := Run(context.Background(), Options{Config: cfg, Runner: (&fakeRunner{}).run, Clock: newClock()}).Summary()
	for _, want := range []string{"smoke: OK", "up         ok", "checks[1]  ok", "down       ok", "down: ok"} {
		if !strings.Contains(sum, want) {
			t.Errorf("Summary lacks %q:\n%s", want, sum)
		}
	}
}
