// Package gitutil provides the minimal git operations diadoche needs:
// making sure a repo exists, diffing a task's changes against its base,
// committing them once the task is DONE, and parking them on a side branch
// once it FAILED — or, in worktree mode, giving each task its own worktree
// and branch, merging that branch once the task is DONE and renaming it
// once it FAILED.
package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// emptyTreeHash is git's well-known empty-tree object hash — content
// addressed, so it's valid in any repository regardless of history. Used
// as the diff base for a task that starts before the repo's first commit,
// where `git rev-parse HEAD` has nothing to resolve.
const emptyTreeHash = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// BaseCommit returns the commit diadoche should diff a task's changes
// against: HEAD, or git's empty-tree hash if the repo has no commits yet
// (a fresh `git init` with nothing committed).
func BaseCommit(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return emptyTreeHash, nil
	}
	return strings.TrimSpace(out), nil
}

// DiffSince returns the unified diff of tracked-file changes in dir since
// base (typically a BaseCommit result).
func DiffSince(ctx context.Context, dir, base string) (string, error) {
	out, err := run(ctx, dir, "diff", base)
	if err != nil {
		return "", fmt.Errorf("git diff: %w: %s", err, out)
	}
	return out, nil
}

// DiffStatSince returns `git diff --stat` since base — a compact summary
// used in place of the full diff when it's over a size threshold.
func DiffStatSince(ctx context.Context, dir, base string) (string, error) {
	out, err := run(ctx, dir, "diff", "--stat", base)
	if err != nil {
		return "", fmt.Errorf("git diff --stat: %w: %s", err, out)
	}
	return out, nil
}

// DiffSinceForFiles returns the unified diff since base, restricted to the
// given paths — used to keep an oversized diff down to just the files the
// implementation plan says were meant to change.
func DiffSinceForFiles(ctx context.Context, dir, base string, files []string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	args := append([]string{"diff", base, "--"}, files...)
	out, err := run(ctx, dir, args...)
	if err != nil {
		return "", fmt.Errorf("git diff (restricted to plan files): %w: %s", err, out)
	}
	return out, nil
}

// excludePathspec builds the `git diff`/`git diff --stat` pathspec that
// means "everything except excludeDirs" (each a path relative to dir).
func excludePathspec(excludeDirs []string) []string {
	var nonEmpty []string
	for _, d := range excludeDirs {
		if d != "" {
			nonEmpty = append(nonEmpty, d)
		}
	}
	if len(nonEmpty) == 0 {
		return nil
	}
	spec := []string{"--", "."}
	for _, d := range nonEmpty {
		spec = append(spec, ":(exclude)"+d)
	}
	return spec
}

// DiffSinceExcluding is DiffSince, but excludes everything under each of
// excludeDirs (paths relative to dir, e.g. ".claude/diadoche",
// ".claude/agents") — used to keep diadoche's own bookkeeping and agent
// config files from ever appearing as if they were part of a worker's
// change.
func DiffSinceExcluding(ctx context.Context, dir, base string, excludeDirs ...string) (string, error) {
	args := append([]string{"diff", base}, excludePathspec(excludeDirs)...)
	out, err := run(ctx, dir, args...)
	if err != nil {
		return "", fmt.Errorf("git diff: %w: %s", err, out)
	}
	return out, nil
}

// DiffStatSinceExcluding is DiffStatSince with the same exclusion as
// DiffSinceExcluding.
func DiffStatSinceExcluding(ctx context.Context, dir, base string, excludeDirs ...string) (string, error) {
	args := append([]string{"diff", "--stat", base}, excludePathspec(excludeDirs)...)
	out, err := run(ctx, dir, args...)
	if err != nil {
		return "", fmt.Errorf("git diff --stat: %w: %s", err, out)
	}
	return out, nil
}

// UntrackedFiles returns paths git considers untracked and not ignored
// (`git ls-files --others --exclude-standard`) — new files a plain `git
// diff` never reports, but that may still be part of a task's changes.
func UntrackedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := run(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w: %s", err, out)
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// UntrackedFile is one new, untracked file as tester needs to see it: its
// path, its size, and its text content (empty for a binary file).
type UntrackedFile struct {
	Path    string
	Size    int64
	Content string
	Binary  bool
}

// binarySniffLen is how far into a file a NUL byte still makes git treat it
// as binary; ReadUntracked uses the same heuristic so what it calls binary
// matches what `git diff` would.
const binarySniffLen = 8000

// ReadUntracked reads the contents of the given untracked paths (as
// reported by UntrackedFiles, i.e. slash-separated and relative to dir),
// preserving their order.
//
// It exists because a plain `git diff <base>` never shows a new file at
// all, yet a worker that only added files has still made a change tester
// must review. Reading the files directly keeps the index untouched — no
// `git add -N` — so nothing about the working tree is disturbed by
// building the review input. A file that disappeared between `ls-files`
// and the read is skipped rather than failing the iteration; a binary one
// is reported by path and size with no content.
func ReadUntracked(dir string, paths []string) ([]UntrackedFile, error) {
	out := make([]UntrackedFile, 0, len(paths))
	for _, p := range paths {
		full := filepath.Join(dir, filepath.FromSlash(p))
		b, err := os.ReadFile(full)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read untracked file %s: %w", p, err)
		}
		f := UntrackedFile{Path: p, Size: int64(len(b))}
		sniff := b
		if len(sniff) > binarySniffLen {
			sniff = sniff[:binarySniffLen]
		}
		if bytes.IndexByte(sniff, 0) >= 0 {
			f.Binary = true
		} else {
			f.Content = string(b)
		}
		out = append(out, f)
	}
	return out, nil
}

// EnsureRepo makes sure dir is inside a git working tree, running `git init`
// in dir if it isn't one yet.
func EnsureRepo(ctx context.Context, dir string) error {
	check := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	check.Dir = dir
	if err := check.Run(); err == nil {
		return nil
	}

	initCmd := exec.CommandContext(ctx, "git", "init")
	initCmd.Dir = dir
	var out bytes.Buffer
	initCmd.Stdout = &out
	initCmd.Stderr = &out
	if err := initCmd.Run(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, out.String())
	}
	return nil
}

// CommitAll stages every change under dir and commits it with message. It is
// not an error if there is nothing to commit — that's a no-op.
func CommitAll(ctx context.Context, dir, message string) error {
	add := exec.CommandContext(ctx, "git", "add", "-A", "--", ".")
	add.Dir = dir
	var addOut bytes.Buffer
	add.Stdout = &addOut
	add.Stderr = &addOut
	if err := add.Run(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, addOut.String())
	}

	diff := exec.CommandContext(ctx, "git", "diff", "--cached", "--quiet")
	diff.Dir = dir
	err := diff.Run()
	if err == nil {
		return nil // nothing staged, nothing to commit
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return fmt.Errorf("git diff --cached: %w", err)
	}

	commit := exec.CommandContext(ctx, "git", "commit", "-m", message)
	commit.Dir = dir
	var commitOut bytes.Buffer
	commit.Stdout = &commitOut
	commit.Stderr = &commitOut
	if err := commit.Run(); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, commitOut.String())
	}
	return nil
}

// runEnv is run with extra environment variables on top of the current
// process's environment.
func runEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// literalPathspecs makes git treat every path argument as a literal file
// name, so a path containing glob characters ("[", "*") never matches more
// than itself.
const literalPathspecs = "GIT_LITERAL_PATHSPECS=1"

// splitNUL splits `-z` output into its non-empty fields.
func splitNUL(out string) []string {
	var fields []string
	for f := range strings.SplitSeq(out, "\x00") {
		if f != "" {
			fields = append(fields, f)
		}
	}
	return fields
}

// ChangedPathsExcluding returns every path whose working-tree state
// differs from base — modified, deleted, and new (untracked, not ignored)
// files — except those under excludeDirs, sorted and deduplicated.
func ChangedPathsExcluding(ctx context.Context, dir, base string, excludeDirs ...string) ([]string, error) {
	spec := excludePathspec(excludeDirs)
	diffArgs := append([]string{"diff", "--name-only", "--no-renames", "-z", base}, spec...)
	diffOut, err := run(ctx, dir, diffArgs...)
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only: %w: %s", err, diffOut)
	}
	lsArgs := append([]string{"ls-files", "--others", "--exclude-standard", "-z"}, spec...)
	lsOut, err := run(ctx, dir, lsArgs...)
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w: %s", err, lsOut)
	}
	seen := map[string]bool{}
	var paths []string
	for _, p := range append(splitNUL(diffOut), splitNUL(lsOut)...) {
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// BranchExists reports whether refs/heads/<branch> exists.
func BranchExists(ctx context.Context, dir, branch string) bool {
	_, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// ParkPaths commits the working-tree state of paths on top of base into a
// new branch, and returns the new commit. It never touches HEAD, the
// current branch, the index or the working tree: the commit is built in a
// temporary index and written with `git commit-tree`. A base of git's
// empty tree (no commits yet) yields a parentless commit.
func ParkPaths(ctx context.Context, dir, base, branch, message string, paths []string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "diadoche-park-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmpDir, "index"), literalPathspecs}

	if out, err := runEnv(ctx, dir, env, "read-tree", base); err != nil {
		return "", fmt.Errorf("git read-tree: %w: %s", err, out)
	}
	addArgs := append([]string{"add", "-A", "--"}, paths...)
	if out, err := runEnv(ctx, dir, env, addArgs...); err != nil {
		return "", fmt.Errorf("git add (temporary index): %w: %s", err, out)
	}
	treeOut, err := runEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("git write-tree: %w: %s", err, treeOut)
	}
	commitArgs := []string{"commit-tree", strings.TrimSpace(treeOut), "-m", message}
	if base != emptyTreeHash {
		commitArgs = append(commitArgs, "-p", base)
	}
	commitOut, err := run(ctx, dir, commitArgs...)
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w: %s", err, commitOut)
	}
	commit := strings.TrimSpace(commitOut)
	if out, err := run(ctx, dir, "update-ref", "refs/heads/"+branch, commit, ""); err != nil {
		return "", fmt.Errorf("git update-ref %s: %w: %s", branch, err, out)
	}
	return commit, nil
}

// RestorePaths brings paths back to their state in base (a commit, or git's
// empty tree), in both the index and the working tree: paths that exist in
// base are checked out from it,
// the rest (files new since base) are unstaged and deleted, along with any
// directories that deleting them left empty.
func RestorePaths(ctx context.Context, dir, base string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	env := []string{literalPathspecs}
	inBase, err := PathsInTree(ctx, dir, base, paths)
	if err != nil {
		return err
	}
	var existing, added []string
	for _, p := range paths {
		if inBase[p] {
			existing = append(existing, p)
		} else {
			added = append(added, p)
		}
	}

	if len(existing) > 0 {
		args := append([]string{"checkout", base, "--"}, existing...)
		if out, err := runEnv(ctx, dir, env, args...); err != nil {
			return fmt.Errorf("git checkout %s: %w: %s", base, err, out)
		}
	}
	if len(added) > 0 {
		args := append([]string{"rm", "-q", "--cached", "--ignore-unmatch", "--"}, added...)
		if out, err := runEnv(ctx, dir, env, args...); err != nil {
			return fmt.Errorf("git rm --cached: %w: %s", err, out)
		}
		for _, p := range added {
			full := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removeEmptyParents(dir, filepath.Dir(full))
		}
	}
	return nil
}

// PathsInTree reports which of paths exist in treeish (a commit or git's
// empty tree), as `git ls-tree -r --name-only` with literal pathspecs.
// Empty paths need no git call and yield an empty map.
func PathsInTree(ctx context.Context, dir, treeish string, paths []string) (map[string]bool, error) {
	in := map[string]bool{}
	if len(paths) == 0 {
		return in, nil
	}
	args := append([]string{"ls-tree", "-r", "-z", "--name-only", treeish, "--"}, paths...)
	out, err := runEnv(ctx, dir, []string{literalPathspecs}, args...)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree: %w: %s", err, out)
	}
	for _, p := range splitNUL(out) {
		in[p] = true
	}
	return in, nil
}

// ShowFile returns the content of path (slash-separated, relative to the
// repository root) as of commit, via `git show <commit>:<path>`.
func ShowFile(ctx context.Context, dir, commit, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "show", commit+":"+path)
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git show %s:%s: %w: %s", commit, path, err, errOut.String())
	}
	return out.String(), nil
}

// IsEmptyTree reports whether base is git's empty-tree hash, i.e. the base
// BaseCommit returns for a repository without commits.
func IsEmptyTree(base string) bool {
	return base == emptyTreeHash
}

// removeEmptyParents deletes d and its ancestors, up to but excluding
// root, for as long as they're empty.
func removeEmptyParents(root, d string) {
	root = filepath.Clean(root)
	for d = filepath.Clean(d); d != root && strings.HasPrefix(d, root+string(filepath.Separator)); d = filepath.Dir(d) {
		if os.Remove(d) != nil { // fails on a non-empty directory
			return
		}
	}
}

// DiffStat returns `git diff --stat from to`.
func DiffStat(ctx context.Context, dir, from, to string) (string, error) {
	out, err := run(ctx, dir, "diff", "--stat", from, to)
	if err != nil {
		return "", fmt.Errorf("git diff --stat: %w: %s", err, out)
	}
	return out, nil
}

// CommitPaths stages every change under paths and commits only those paths
// with message, leaving anything else that happens to be staged out of the
// commit. It is not an error if there is nothing to commit — that's a
// no-op.
func CommitPaths(ctx context.Context, dir, message string, paths ...string) error {
	addArgs := append([]string{"add", "-A", "--"}, paths...)
	if out, err := run(ctx, dir, addArgs...); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	diffArgs := append([]string{"diff", "--cached", "--quiet", "--"}, paths...)
	_, err := run(ctx, dir, diffArgs...)
	if err == nil {
		return nil // nothing staged under paths
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return fmt.Errorf("git diff --cached: %w", err)
	}
	commitArgs := append([]string{"commit", "-q", "-m", message, "--"}, paths...)
	if out, err := run(ctx, dir, commitArgs...); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	return nil
}

// WorktreeAdd creates a worktree at path checked out on branch. If branch
// doesn't exist yet it is created at base; an existing branch is attached
// as is (a task resumed after its worktree went away). Stale worktree
// registrations are pruned first, so a directory deleted by hand never
// blocks the add.
func WorktreeAdd(ctx context.Context, repoDir, path, branch, base string) error {
	if out, err := run(ctx, repoDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("git worktree prune: %w: %s", err, out)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	args := []string{"worktree", "add", "-b", branch, path, base}
	if BranchExists(ctx, repoDir, branch) {
		args = []string{"worktree", "add", path, branch}
	}
	if out, err := run(ctx, repoDir, args...); err != nil {
		return fmt.Errorf("git worktree add: %w: %s", err, out)
	}
	return nil
}

// WorktreeAddDetached creates a worktree at path with a detached HEAD at
// commit — a throwaway checkout that creates no branch. Stale worktree
// registrations are pruned first, like WorktreeAdd.
func WorktreeAddDetached(ctx context.Context, repoDir, path, commit string) error {
	if out, err := run(ctx, repoDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("git worktree prune: %w: %s", err, out)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if out, err := run(ctx, repoDir, "worktree", "add", "--detach", path, commit); err != nil {
		return fmt.Errorf("git worktree add --detach: %w: %s", err, out)
	}
	return nil
}

// resolvePath returns p made absolute with symlinks resolved, or p
// cleaned if that isn't possible (e.g. it doesn't exist).
func resolvePath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// IsWorktree reports whether path is a worktree registered with the repo
// at repoDir.
func IsWorktree(ctx context.Context, repoDir, path string) bool {
	out, err := run(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return false
	}
	want := resolvePath(path)
	for line := range strings.SplitSeq(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && resolvePath(p) == want {
			return true
		}
	}
	return false
}

// WorktreeRemove removes the worktree at path, discarding anything left in
// it. It is not an error if path is no longer a worktree; whatever is left
// of the directory is deleted.
func WorktreeRemove(ctx context.Context, repoDir, path string) error {
	if IsWorktree(ctx, repoDir, path) {
		if out, err := run(ctx, repoDir, "worktree", "remove", "--force", path); err != nil {
			return fmt.Errorf("git worktree remove: %w: %s", err, out)
		}
	} else if err := os.RemoveAll(path); err != nil {
		return err
	}
	if out, err := run(ctx, repoDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("git worktree prune: %w: %s", err, out)
	}
	return nil
}

// CommitAllExcluding stages every change under dir except those under
// excludeDirs and commits it with message. committed is false (and it is
// not an error) when there was nothing to commit.
func CommitAllExcluding(ctx context.Context, dir, message string, excludeDirs ...string) (committed bool, err error) {
	spec := excludePathspec(excludeDirs)
	if spec == nil {
		spec = []string{"--", "."}
	}
	addArgs := append([]string{"add", "-A"}, spec...)
	if out, err := run(ctx, dir, addArgs...); err != nil {
		return false, fmt.Errorf("git add: %w: %s", err, out)
	}
	_, err = run(ctx, dir, "diff", "--cached", "--quiet")
	if err == nil {
		return false, nil // nothing staged
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false, fmt.Errorf("git diff --cached: %w", err)
	}
	if out, err := run(ctx, dir, "commit", "-q", "-m", message); err != nil {
		return false, fmt.Errorf("git commit: %w: %s", err, out)
	}
	return true, nil
}

// MergeError is a merge that didn't happen. Conflict reports a content
// conflict (the merge was started and then aborted); otherwise git refused
// to start it at all (e.g. local changes would be overwritten). Either
// way the current branch, index and working tree are as before the merge.
type MergeError struct {
	Output   string
	Conflict bool
}

func (e *MergeError) Error() string {
	kind := "merge failed"
	if e.Conflict {
		kind = "merge conflict"
	}
	if line := firstNonEmptyLine(e.Output); line != "" {
		return kind + ": " + line
	}
	return kind
}

func firstNonEmptyLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// Merge merges branch into the branch checked out in dir: a fast-forward
// when possible, otherwise a merge commit with message. A failed merge is
// aborted and returned as a *MergeError.
func Merge(ctx context.Context, dir, branch, message string) (fastForward bool, err error) {
	var out string
	if _, ancErr := run(ctx, dir, "merge-base", "--is-ancestor", "HEAD", branch); ancErr == nil {
		fastForward = true
		out, err = run(ctx, dir, "merge", "--ff-only", branch)
	} else {
		out, err = run(ctx, dir, "merge", "--no-ff", "--no-edit", "-m", message, branch)
	}
	if err == nil {
		return fastForward, nil
	}
	if _, inProgress := run(ctx, dir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); inProgress == nil {
		mErr := &MergeError{Output: out, Conflict: true}
		if abortOut, abortErr := run(ctx, dir, "merge", "--abort"); abortErr != nil {
			return false, fmt.Errorf("%w; git merge --abort also failed: %v: %s", mErr, abortErr, abortOut)
		}
		return false, mErr
	}
	return false, &MergeError{Output: out}
}

// MergeBase returns `git merge-base a b`.
func MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	out, err := run(ctx, dir, "merge-base", a, b)
	if err != nil {
		return "", fmt.Errorf("git merge-base: %w: %s", err, out)
	}
	return strings.TrimSpace(out), nil
}

// RenameBranch renames branch oldName to newName.
func RenameBranch(ctx context.Context, dir, oldName, newName string) error {
	if out, err := run(ctx, dir, "branch", "-m", oldName, newName); err != nil {
		return fmt.Errorf("git branch -m: %w: %s", err, out)
	}
	return nil
}

// DeleteBranch deletes branch, merged or not.
func DeleteBranch(ctx context.Context, dir, branch string) error {
	if out, err := run(ctx, dir, "branch", "-D", branch); err != nil {
		return fmt.Errorf("git branch -D: %w: %s", err, out)
	}
	return nil
}

// ChangedPathsBetween returns the paths that exist in to and differ from
// from (i.e. not deletions), as `git diff --name-only --diff-filter=d`.
func ChangedPathsBetween(ctx context.Context, dir, from, to string) ([]string, error) {
	out, err := run(ctx, dir, "diff", "--name-only", "--no-renames", "--diff-filter=d", "-z", from, to)
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only: %w: %s", err, out)
	}
	return splitNUL(out), nil
}

// EnsureInfoExclude adds pattern as a line of the repository's
// info/exclude file (shared by all its worktrees) unless it is already
// there, so git ignores matching paths without touching .gitignore.
func EnsureInfoExclude(ctx context.Context, dir, pattern string) error {
	out, err := run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("git rev-parse --git-common-dir: %w: %s", err, out)
	}
	common := strings.TrimSpace(out)
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	infoDir := filepath.Join(common, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(infoDir, "exclude")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	data = append(data, pattern+"\n"...)
	return os.WriteFile(path, data, 0o644)
}

// HasCommits reports whether HEAD resolves to a commit, i.e. the repo has
// at least one commit on its current branch.
func HasCommits(ctx context.Context, dir string) bool {
	_, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	return err == nil
}

// CurrentBranch returns the short name of the branch checked out in dir,
// or "HEAD" when HEAD is detached.
func CurrentBranch(ctx context.Context, dir string) string {
	out, err := run(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		return "HEAD"
	}
	return strings.TrimSpace(out)
}
