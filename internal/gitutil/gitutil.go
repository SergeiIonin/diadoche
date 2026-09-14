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
)

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
