package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
}

func TestEnsureRepoInitsWhenMissingAndIsIdempotent(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	ctx := context.Background()

	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf(".git not created: %v", err)
	}

	// Calling again on an already-initialized repo must not error.
	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatalf("second EnsureRepo call failed: %v", err)
	}
}

func TestCommitAllCommitsChangesAndIsNoopWhenClean(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	ctx := context.Background()

	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GIT_AUTHOR_NAME", "diadoche-test")
	t.Setenv("GIT_AUTHOR_EMAIL", "diadoche-test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "diadoche-test")
	t.Setenv("GIT_COMMITTER_EMAIL", "diadoche-test@example.com")

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAll(ctx, dir, "first commit"); err != nil {
		t.Fatal(err)
	}

	before, err := exec.CommandContext(ctx, "git", "-C", dir, "log", "--oneline").Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("expected at least one commit after CommitAll with a change")
	}

	// No changes since: must be a no-op, not an error, and no new commit.
	if err := CommitAll(ctx, dir, "should not happen"); err != nil {
		t.Fatalf("expected no-op commit to succeed, got: %v", err)
	}

	after, err := exec.CommandContext(ctx, "git", "-C", dir, "log", "--oneline").Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("expected no new commit; before=%q after=%q", before, after)
	}
}
