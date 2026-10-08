package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"diadoche/internal/gitutil"
	"diadoche/internal/implplan"
)

// normalDiffGuidance and oversizedDiffGuidance tell tester how far it may
// go beyond the diff it was handed. They are the per-call counterpart of
// the tester system prompt: that prompt says tester reviews from a diff,
// these say what this particular call's diff actually is.
// oversizedDiffGuidance takes the base commit, so tester can diff
// individual files itself.
const (
	normalDiffGuidance = "Judge the change from this diff and from running the test/lint command above; " +
		"open files outside the diff only where the diff itself is ambiguous."

	oversizedDiffGuidance = "The full diff was over the size limit and is not included. " +
		"Decide from the `git diff --stat` above and the plan's ## Files list which files matter, " +
		"then open those files yourself (e.g. `git diff %[1]s -- <path>`, with %[1]s the base commit " +
		"this task started from) — only the ones you need for the review, not the whole repository. " +
		"Still run the test/lint command."
)

// testerDiff is the review input handed to tester in place of the
// repository itself: what the worker changed since the task's base commit.
type testerDiff struct {
	// Body is the diff text — either the full diff plus the contents of
	// new untracked files, or, once that is over --max-diff-bytes,
	// `git diff --stat` plus the untracked file list (and the
	// plan-restricted diff when that one fits).
	Body string

	// Note is empty for a diff within the limit; otherwise it explains
	// what was left out and names the diff base, and is spliced into the
	// prompt's diff heading.
	Note string

	// Oversized reports whether the full diff was over the limit, i.e.
	// whether tester has to open the files it needs itself.
	Oversized bool

	// Empty means the worker changed nothing at all: no tracked diff and
	// no untracked files. Tester is then not called.
	Empty bool
}

// buildTesterDiff computes the worker's changes since the task's base
// commit, in the shape tester should see them. plan is the task's
// IMPL_PLAN.md, used for its "## Files" list when the diff has to be cut
// down.
func (o *Orchestrator) buildTesterDiff(ctx context.Context, ws *workspace, plan string) (testerDiff, error) {
	diff, err := gitutil.DiffSinceExcluding(ctx, o.Cfg.Dir, ws.base, ws.exclude...)
	if err != nil {
		return testerDiff{}, fmt.Errorf("git diff: %w", err)
	}
	untrackedAll, err := gitutil.UntrackedFiles(ctx, o.Cfg.Dir)
	if err != nil {
		return testerDiff{}, fmt.Errorf("git ls-files: %w", err)
	}
	untracked := excludeUnder(untrackedAll, ws.exclude)

	if strings.TrimSpace(diff) == "" && len(untracked) == 0 {
		return testerDiff{Empty: true}, nil
	}

	files, err := gitutil.ReadUntracked(o.Cfg.Dir, untracked)
	if err != nil {
		return testerDiff{}, err
	}
	withContent := renderUntracked(files, true)

	// New files only ever show up as untracked, so their contents count
	// toward the limit too — otherwise a worker that adds one huge new
	// file sends the whole thing regardless of --max-diff-bytes.
	if len(diff)+len(withContent) <= o.Cfg.MaxDiffBytes {
		return testerDiff{Body: join(diff, withContent)}, nil
	}

	stat, err := gitutil.DiffStatSinceExcluding(ctx, o.Cfg.Dir, ws.base, ws.exclude...)
	if err != nil {
		return testerDiff{}, fmt.Errorf("git diff --stat: %w", err)
	}
	body := join(stat, renderUntracked(files, false))

	// The plan's own files may well be where the huge change is, in which
	// case including their diff would defeat the whole point of the limit.
	restricted, err := gitutil.DiffSinceForFiles(ctx, o.Cfg.Dir, ws.base, implplan.Files(plan))
	if err != nil {
		return testerDiff{}, fmt.Errorf("git diff for plan files: %w", err)
	}
	switch {
	case strings.TrimSpace(restricted) == "":
		// Nothing changed in the plan's files, or the plan lists none.
	case len(restricted) <= o.Cfg.MaxDiffBytes:
		body = join(body, fmt.Sprintf("Diff restricted to the plan's ## %s list:\n%s", implplan.FilesHeading, restricted))
	default:
		body = join(body, fmt.Sprintf("The diff restricted to the plan's ## %s list is over the size limit too, so it was omitted as well.", implplan.FilesHeading))
	}

	note := fmt.Sprintf(
		" (exceeded --max-diff-bytes=%d (alias --tester-diff-limit): the full diff was not included; "+
			"showing `git diff --stat` and the list of untracked files; diff base is %s)",
		o.Cfg.MaxDiffBytes, ws.base)
	return testerDiff{Body: body, Note: note, Oversized: true}, nil
}

// renderUntracked describes new, untracked files — which a plain
// `git diff` never shows. withContent renders each file's body (binary
// files by name and size only); without it, only the name/size lines, for
// the oversized case where tester is told to open what it needs itself.
func renderUntracked(files []gitutil.UntrackedFile, withContent bool) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Untracked files (new, not in the diff above):\n")
	for _, f := range files {
		if !withContent {
			fmt.Fprintf(&b, "%s (%d bytes)\n", f.Path, f.Size)
			continue
		}
		fmt.Fprintf(&b, "--- %s (%d bytes) ---\n", f.Path, f.Size)
		if f.Binary {
			b.WriteString("[binary, not shown]\n")
			continue
		}
		b.WriteString(f.Content)
		if !strings.HasSuffix(f.Content, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// join concatenates the non-empty parts of a diff body, one blank line
// apart.
func join(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			nonEmpty = append(nonEmpty, strings.TrimRight(p, "\n"))
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}
