package smoke

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"diadoche/internal/redgreen"
)

// Caps on what a Result keeps and how big its Summary gets.
const (
	maxCapturedOutput  = 1 << 20  // bytes of command output kept by the default runner (last bytes)
	outputTail         = 4000     // bytes of command output stored per step
	maxBodyRead        = 64 << 10 // bytes read from any HTTP response body
	bodyTail           = 2000     // bytes of an HTTP body stored per step
	maxConsoleErrors   = 20       // console errors kept per page
	maxConsoleErrorLen = 300      // bytes per console error
	maxSummaryBytes    = 8000     // total size of Summary()
	consoleBudget      = 2000     // bytes of Summary() spent on console errors
	maxDetailLen       = 200      // bytes of a step detail in the Summary() table
	summaryTruncated   = "\n[smoke: summary truncated]\n"
)

// Status is the outcome of one step.
type Status string

const (
	StatusOK      Status = "ok"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped"
)

// StepResult is the outcome of one step: up, wait, checks[N], browser or
// down.
type StepResult struct {
	Name       string // "up", "wait", "checks[2]", "browser", "down"
	Label      string // the command, URL or check being run
	Status     Status
	Duration   time.Duration
	Detail     string // why it failed or was skipped
	ExitCode   int
	TimedOut   bool
	OutputTail string // tail of the command output, on failure
	HTTPStatus int    // response status of a failed URL probe
	BodyTail   string // tail of the response body of a failed URL probe
}

// Page is the outcome of opening one browser URL.
type Page struct {
	URL           string
	Screenshot    string // path of the screenshot, empty when none was written
	OK            bool
	Detail        string // why the page failed
	ConsoleErrors []string
	OutputTail    string // tail of the browser command output, on failure
}

// Result is everything a smoke run found out.
type Result struct {
	Steps    []StepResult // in run order, down last
	Failed   string       // name of the first failed step, "" when none failed
	Pages    []Page       // one per browser URL visited
	Warnings []string
	DownRan  bool
	DownOK   bool
	Duration time.Duration
}

// OK reports whether no step failed (a failed down counts).
func (r Result) OK() bool { return r.Failed == "" }

// FailedStep returns the first failed step, or nil.
func (r Result) FailedStep() *StepResult {
	for i := range r.Steps {
		if r.Steps[i].Status == StatusFailed {
			return &r.Steps[i]
		}
	}
	return nil
}

// Screenshots returns the paths of every screenshot taken.
func (r Result) Screenshots() []string {
	var out []string
	for _, p := range r.Pages {
		if p.Screenshot != "" {
			out = append(out, p.Screenshot)
		}
	}
	return out
}

// String returns Summary().
func (r Result) String() string { return r.Summary() }

// Summary renders the result as a compact human-readable block of at most
// maxSummaryBytes bytes: the verdict, one line per step, the output of the
// failed step (and of a failed down and failed pages), screenshots, console
// errors and warnings. Output tails are shortened first so the rest fits.
func (r Result) Summary() string {
	var head strings.Builder
	if r.OK() {
		fmt.Fprintf(&head, "smoke: OK in %s\n", fmtDur(r.Duration))
	} else {
		at := r.Failed
		if f := r.FailedStep(); f != nil && f.Label != "" {
			at += " (" + oneLine(f.Label, maxDetailLen) + ")"
		}
		fmt.Fprintf(&head, "smoke: FAILED at %s in %s\n", at, fmtDur(r.Duration))
	}
	width := 0
	for _, s := range r.Steps {
		width = max(width, len(s.Name))
	}
	for _, s := range r.Steps {
		line := fmt.Sprintf("  %-*s  %-7s", width, s.Name, s.Status)
		if s.Status != StatusSkipped {
			line += "  " + fmtDur(s.Duration)
		}
		if s.Detail != "" {
			line += "  " + oneLine(s.Detail, maxDetailLen)
		}
		head.WriteString(strings.TrimRight(line, " ") + "\n")
	}

	type section struct{ title, text string }
	var tails []section
	for _, s := range r.Steps {
		if s.Status != StatusFailed {
			continue
		}
		if s.OutputTail != "" {
			tails = append(tails, section{fmt.Sprintf("%s output (tail):", s.Name), s.OutputTail})
		}
		if s.HTTPStatus != 0 || s.BodyTail != "" {
			tails = append(tails, section{fmt.Sprintf("%s HTTP %d body (tail):", s.Name, s.HTTPStatus), s.BodyTail})
		}
	}
	for _, p := range r.Pages {
		if !p.OK && p.OutputTail != "" {
			tails = append(tails, section{fmt.Sprintf("browser %s output (tail):", p.URL), p.OutputTail})
		}
	}

	var rest strings.Builder
	if shots := r.Screenshots(); len(shots) > 0 {
		rest.WriteString("screenshots:\n")
		for _, s := range shots {
			rest.WriteString("  " + s + "\n")
		}
	}
	failedPages := 0
	for _, p := range r.Pages {
		if p.OK {
			continue
		}
		if failedPages++; failedPages == 1 {
			rest.WriteString("pages failed:\n")
		}
		rest.WriteString("  " + p.URL + ": " + oneLine(p.Detail, maxDetailLen) + "\n")
	}
	if len(r.Warnings) > 0 {
		rest.WriteString("warnings:\n")
		for _, w := range r.Warnings {
			rest.WriteString("  " + oneLine(w, maxDetailLen) + "\n")
		}
	}
	switch {
	case !r.DownRan:
		rest.WriteString("down: not run\n")
	case r.DownOK:
		rest.WriteString("down: ok\n")
	default:
		rest.WriteString("down: FAILED\n")
	}

	var console strings.Builder
	for _, p := range r.Pages {
		for _, e := range p.ConsoleErrors {
			console.WriteString("  " + p.URL + ": " + oneLine(e, maxConsoleErrorLen) + "\n")
		}
	}
	consoleText := ""
	if console.Len() > 0 {
		consoleText = "console errors:\n" + cutHead(console.String(), consoleBudget)
	}

	var b strings.Builder
	b.WriteString(head.String())
	if len(tails) > 0 {
		budget := maxSummaryBytes - head.Len() - rest.Len() - len(consoleText) - len(summaryTruncated)
		for _, t := range tails {
			budget -= len(t.title) + 1
		}
		per := min(outputTail, max(budget/len(tails), 200))
		for _, t := range tails {
			b.WriteString(t.title + "\n")
			text := redgreen.Tail(indent(t.text), per)
			b.WriteString(text)
			if !strings.HasSuffix(text, "\n") {
				b.WriteString("\n")
			}
		}
	}
	b.WriteString(rest.String())
	b.WriteString(consoleText)
	return capSummary(b.String())
}

// capSummary cuts s to maxSummaryBytes on a rune boundary, with a note.
func capSummary(s string) string {
	if len(s) <= maxSummaryBytes {
		return s
	}
	return cutHead(s, maxSummaryBytes-len(summaryTruncated)) + summaryTruncated
}

// cutHead returns at most the first n bytes of s, cut on a rune boundary.
func cutHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	return "  " + strings.ReplaceAll(s, "\n", "\n  ") + "\n"
}

// oneLine flattens s to a single line of at most n bytes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return cutHead(s, n) + "…"
	}
	return s
}

func fmtDur(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}
