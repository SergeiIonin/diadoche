// Package redgreen holds the stdlib-only helpers of diadoche's red-green
// check: deciding which changed files are tests, extracting test names
// from them, and running a test command in a directory.
//
// The check itself (which files, which worktree, what counts as a failed
// iteration) lives in the orchestrator; this package has no internal
// dependencies so config can use it to validate --test-globs.
package redgreen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultGoGlob is the Go test-file pattern; it always counts, whatever
// other globs are configured.
const DefaultGoGlob = "*_test.go"

// MatchGlob reports whether the slash-separated relative path p matches
// pattern (path.Match syntax):
//
//   - a pattern without "/" is matched against the base name of p, i.e. at
//     any depth ("test_*.py" matches "pkg/test_x.py");
//   - a pattern with "/" is matched against the whole of p, except that a
//     leading "**/" means "at any depth" — the rest of the pattern is tried
//     against p and against every suffix of p that starts after a "/".
//
// Only a leading "**/" is special ("a/**/b" is plain path.Match), and brace
// alternatives ("{a,b}") are not supported: pass several globs instead.
func MatchGlob(pattern, p string) bool {
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(p))
		return ok
	}
	rest, anyDepth := strings.CutPrefix(pattern, "**/")
	if !anyDepth {
		ok, _ := path.Match(pattern, p)
		return ok
	}
	for s := p; ; {
		if ok, _ := path.Match(rest, s); ok {
			return true
		}
		i := strings.Index(s, "/")
		if i < 0 {
			return false
		}
		s = s[i+1:]
	}
}

// IsTestFile reports whether p is a test file: it matches DefaultGoGlob or
// any of globs.
func IsTestFile(p string, globs []string) bool {
	if MatchGlob(DefaultGoGlob, p) {
		return true
	}
	for _, g := range globs {
		if MatchGlob(g, p) {
			return true
		}
	}
	return false
}

// ValidateGlobs returns an error naming the first malformed glob.
func ValidateGlobs(globs []string) error {
	for _, g := range globs {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("bad glob %q: %w", g, err)
		}
	}
	return nil
}

var (
	goTestRe = regexp.MustCompile(`(?m)^func\s+((?:Test|Fuzz|Example)\w*)\s*\(`)
	pyTestRe = regexp.MustCompile(`(?m)^\s*(?:async\s+)?def\s+(test\w*)\s*\(`)
	jsTestRe = regexp.MustCompile("\\b(?:it|test)\\s*\\(\\s*(?:'([^']+)'|\"([^\"]+)\"|`([^`]+)`)")
	rbTestRe = regexp.MustCompile(`\bit\s+(?:'([^']+)'|"([^"]+)")`)
)

// TestNames returns the names of the tests defined in content, the text of
// file p, unique and in file order. The syntax is chosen by p's extension
// (Go, Python, JavaScript/TypeScript, Ruby); any other extension yields nil.
func TestNames(p, content string) []string {
	var re *regexp.Regexp
	switch path.Ext(p) {
	case ".go":
		re = goTestRe
	case ".py":
		re = pyTestRe
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs":
		re = jsTestRe
	case ".rb":
		re = rbTestRe
	default:
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(content, -1) {
		name := ""
		for _, g := range m[1:] {
			if g != "" {
				name = g
				break
			}
		}
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// NewNames returns the test names in newContent that baseContent, the same
// file's earlier version, doesn't define.
func NewNames(p, baseContent, newContent string) []string {
	old := map[string]bool{}
	for _, n := range TestNames(p, baseContent) {
		old[n] = true
	}
	var out []string
	for _, n := range TestNames(p, newContent) {
		if !old[n] {
			out = append(out, n)
		}
	}
	return out
}

// CommandResult is the outcome of RunCommand.
type CommandResult struct {
	ExitCode int    // -1 when the command couldn't be started or was killed
	Output   string // stdout and stderr combined
	TimedOut bool
	Err      error // a start failure, or the wait error of a killed command
}

// Passed reports whether the command ran to completion with exit code 0.
func (r CommandResult) Passed() bool {
	return r.ExitCode == 0 && r.Err == nil && !r.TimedOut
}

// waitDelay bounds how long RunCommand waits for output after the shell is
// killed, so children that hold the pipes open can't hang it.
const waitDelay = 5 * time.Second

// RunCommand runs script with `sh -c` in dir, capturing stdout and stderr
// together. A timeout > 0 bounds the run; when it expires the shell is
// killed and TimedOut is set.
func RunCommand(ctx context.Context, dir, script string, timeout time.Duration) CommandResult {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = dir
	cmd.WaitDelay = waitDelay
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	res := CommandResult{Output: out.String()}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		res.ExitCode = exitErr.ExitCode()
	default:
		res.ExitCode = -1
		res.Err = err
	}
	return res
}

// Tail returns the last n bytes of s, cut on a rune boundary and prefixed
// with "…" when anything was cut.
func Tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return "…" + s[cut:]
}
