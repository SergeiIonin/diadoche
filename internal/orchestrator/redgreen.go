package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"diadoche/internal/gitutil"
	"diadoche/internal/implplan"
	"diadoche/internal/redgreen"
	"diadoche/internal/specs"
)

// redGreenTimeout bounds one run of the plan's test command on the base
// commit.
const redGreenTimeout = 30 * time.Minute

// redGreenOutputTail is how much of the test command's output (from the
// end) the failure review quotes.
const redGreenOutputTail = 4000

// testFile is one test file in the task's diff: existed reports whether it
// was already there at the base commit, names the tests it adds (as far as
// they could be extracted).
type testFile struct {
	path    string
	existed bool
	names   []string
}

// redGreenOutcome is what the red-green step decided: fail makes the
// iteration unsuccessful with review as the review written for the next
// planner; testerNote, when non-empty, is an extra item for tester.
type redGreenOutcome struct {
	fail       bool
	review     string
	testerNote string
}

// redGreenCheck runs between worker and tester: it collects the test files
// worker added or changed since ws.base and runs the plan's test command
// on them in a temporary worktree at ws.base, where the task's new code
// doesn't exist. Tests that pass there check nothing, and a diff without
// tests (unless the spec is marked no-tests) fails the iteration too.
//
// Only a git failure while listing the task's files is returned as an
// error. Problems with the temporary worktree skip the check, and anything
// the test command does on the base commit other than passing counts as
// the expected red result.
func (o *Orchestrator) redGreenCheck(ctx context.Context, specFile string, specContent []byte, ws *workspace, plan string) (redGreenOutcome, error) {
	globs := o.Cfg.TestGlobs
	if len(globs) == 0 {
		globs = implplan.InferTestGlobs(implplan.TestCommand(plan))
	}

	files, err := o.collectTestFiles(ctx, ws, globs)
	if err != nil {
		return redGreenOutcome{}, err
	}

	var out redGreenOutcome
	out.testerNote = weakenedTestsNote(files, implplan.Files(plan))

	if specs.HasNoTestsMarker(specContent) {
		o.Log.Printf("%s: spec is marked no-tests, red-green check skipped", specFile)
		return out, nil
	}
	if len(files) == 0 {
		patterns := append([]string{redgreen.DefaultGoGlob}, globs...)
		out.fail = true
		out.review = fmt.Sprintf("Red-green check failed: worker wrote no tests.\n\n"+
			"The diff since the base commit %s contains no test files (patterns: %s). "+
			"Plan tests that cover the new behaviour and fail without it. "+
			"If this task genuinely needs no tests, add the line %s to the task spec.\n",
			ws.base, strings.Join(uniqueStrings(patterns), ", "), specs.NoTestsMarker)
		return out, nil
	}
	if gitutil.IsEmptyTree(ws.base) {
		o.Log.Printf("%s: no base commit to check against, red-green check skipped", specFile)
		return out, nil
	}
	script := implplan.TestCommandScript(plan)
	if script == "" {
		o.Log.Printf("%s: the plan has no runnable test command, red-green check skipped", specFile)
		return out, nil
	}

	res, ran := o.runTestsOnBase(ctx, specFile, ws, files, script)
	if !ran {
		return out, nil
	}
	if !res.Passed() {
		o.Log.Printf("%s: red-green: tests fail on the base commit as expected (exit %d, timed out: %v)", specFile, res.ExitCode, res.TimedOut)
		return out, nil
	}

	o.Log.Printf("%s: red-green: the task's tests pass on the base commit %s — they check nothing", specFile, ws.base)
	var b strings.Builder
	fmt.Fprintf(&b, "Red-green check failed: the tests check nothing.\n\n"+
		"The new/changed test files pass on the base commit %s, i.e. without the task's new code, "+
		"so they do not verify it.\n\nTest files:\n", ws.base)
	for _, f := range files {
		fmt.Fprintf(&b, "- %s", f.path)
		if len(f.names) > 0 {
			fmt.Fprintf(&b, " (tests: %s)", strings.Join(f.names, ", "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nTest command run on the base commit:\n%s\n\nOutput (tail):\n%s\n\n"+
		"Plan tests that fail without the implementation (assert on the new behaviour, "+
		"not just that the code compiles or runs).\n",
		script, redgreen.Tail(res.Output, redGreenOutputTail))
	out.fail = true
	out.review = b.String()
	return out, nil
}

// collectTestFiles returns the test files (by globs) added or modified in
// ws.dir since ws.base, excluding deletions and files that were dirty
// before the task started.
func (o *Orchestrator) collectTestFiles(ctx context.Context, ws *workspace, globs []string) ([]testFile, error) {
	changed, err := gitutil.ChangedPathsExcluding(ctx, ws.dir, ws.base, ws.exclude...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range changed {
		if ws.preexisting[p] || !redgreen.IsTestFile(p, globs) {
			continue
		}
		if _, err := os.Stat(filepath.Join(ws.dir, filepath.FromSlash(p))); err != nil {
			continue // deleted by worker
		}
		paths = append(paths, p)
	}

	existed := map[string]bool{}
	if !gitutil.IsEmptyTree(ws.base) {
		if existed, err = gitutil.PathsInTree(ctx, ws.dir, ws.base, paths); err != nil {
			return nil, err
		}
	}

	files := make([]testFile, 0, len(paths))
	for _, p := range paths {
		f := testFile{path: p, existed: existed[p]}
		content, err := os.ReadFile(filepath.Join(ws.dir, filepath.FromSlash(p)))
		if err != nil {
			return nil, fmt.Errorf("read test file %s: %w", p, err)
		}
		f.names = redgreen.TestNames(p, string(content))
		if f.existed {
			if old, err := gitutil.ShowFile(ctx, ws.dir, ws.base, p); err == nil {
				f.names = redgreen.NewNames(p, old, string(content))
			}
		}
		files = append(files, f)
	}
	return files, nil
}

// runTestsOnBase creates a temporary detached worktree at ws.base outside
// the task's working tree, copies files into it and runs script there.
// The worktree is removed before returning, whatever happened. ran is
// false when the worktree couldn't be set up — the check is then skipped.
func (o *Orchestrator) runTestsOnBase(ctx context.Context, specFile string, ws *workspace, files []testFile, script string) (res redgreen.CommandResult, ran bool) {
	tmp, err := os.MkdirTemp("", "diadoche-redgreen-")
	if err != nil {
		o.Log.Printf("%s: red-green check skipped: %v", specFile, err)
		return res, false
	}
	wt := filepath.Join(tmp, "wt")
	defer func() {
		// Cleanup must happen even when ctx was cancelled mid-run.
		if err := gitutil.WorktreeRemove(context.WithoutCancel(ctx), o.Cfg.Dir, wt); err != nil {
			o.Log.Printf("%s: red-green: removing temporary worktree %s: %v", specFile, wt, err)
		}
		if err := os.RemoveAll(tmp); err != nil {
			o.Log.Printf("%s: red-green: removing %s: %v", specFile, tmp, err)
		}
	}()

	if err := gitutil.WorktreeAddDetached(ctx, o.Cfg.Dir, wt, ws.base); err != nil {
		o.Log.Printf("%s: red-green check skipped: %v", specFile, err)
		return res, false
	}
	for _, f := range files {
		if err := copyFile(filepath.Join(ws.dir, filepath.FromSlash(f.path)), filepath.Join(wt, filepath.FromSlash(f.path))); err != nil {
			o.Log.Printf("%s: red-green check skipped: %v", specFile, err)
			return res, false
		}
	}

	o.Log.Printf("%s: red-green: running the plan's test command on the base commit %s with %d test file(s)", specFile, ws.base, len(files))
	return redgreen.RunCommand(ctx, wt, script, redGreenTimeout), true
}

// copyFile copies src to dst, creating dst's parent directories and
// keeping src's permission bits.
func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return err
	}
	// WriteFile keeps the mode of a file that already existed at base.
	return os.Chmod(dst, info.Mode().Perm())
}

// weakenedTestsNote returns the extra tester item for test files that
// existed before the task, were changed by worker, and are not listed in
// the plan's "## Files" — or "" when there are none.
func weakenedTestsNote(files []testFile, planFiles []string) string {
	listed := map[string]bool{}
	for _, p := range planFiles {
		listed[normalizePlanPath(p)] = true
	}
	var unlisted []string
	for _, f := range files {
		if f.existed && !listed[normalizePlanPath(f.path)] {
			unlisted = append(unlisted, f.path)
		}
	}
	if len(unlisted) == 0 {
		return ""
	}
	return fmt.Sprintf("Additional check — existing tests modified: worker changed test files that existed "+
		"before this task and are not listed in the plan's ## Files: %s. Check that these existing tests "+
		"were not weakened (assertions removed or loosened, cases deleted, tests skipped) and treat any "+
		"weakening as a significant defect.", strings.Join(unlisted, ", "))
}

func normalizePlanPath(p string) string {
	return strings.TrimPrefix(filepath.ToSlash(p), "./")
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
