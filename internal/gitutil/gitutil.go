// Package gitutil provides the minimal git operations diadoche needs:
// making sure a repo exists, diffing a task's changes against its base,
// committing them once the task is DONE, and parking them on a side branch
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
	lsArgs := append([]string{"ls-tree", "-r", "-z", "--name-only", base, "--"}, paths...)
	lsOut, err := runEnv(ctx, dir, env, lsArgs...)
	if err != nil {
		return fmt.Errorf("git ls-tree: %w: %s", err, lsOut)
	}
	inBase := map[string]bool{}
	for _, p := range splitNUL(lsOut) {
		inBase[p] = true
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
