// Package gitutil provides the minimal git operations diadoche needs to
// commit a task's changes once it's DONE: making sure a repo exists, and
// staging + committing whatever the worker/tester agents changed.
package gitutil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
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
