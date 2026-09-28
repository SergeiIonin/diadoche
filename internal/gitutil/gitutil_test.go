package gitutil

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func setTestGitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "diadoche-test")
	t.Setenv("GIT_AUTHOR_EMAIL", "diadoche-test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "diadoche-test")
	t.Setenv("GIT_COMMITTER_EMAIL", "diadoche-test@example.com")
}

func TestBaseCommitFallsBackToEmptyTreeBeforeFirstCommit(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}

	base, err := BaseCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if base != emptyTreeHash {
		t.Fatalf("base = %q, want empty-tree hash %q", base, emptyTreeHash)
	}

	// The empty-tree hash must actually work as a diff base pre-first-commit.
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	diff, err := DiffSince(ctx, dir, base)
	if err != nil {
		t.Fatal(err)
	}
	_ = diff // untracked files never show in `git diff`; just confirming no error
}

func TestBaseCommitReturnsHeadAfterFirstCommit(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	setTestGitEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAll(ctx, dir, "first commit"); err != nil {
		t.Fatal(err)
	}

	base, err := BaseCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	head, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	if base != strings.TrimSpace(string(head)) {
		t.Fatalf("base = %q, want HEAD %q", base, head)
	}
}

func TestDiffSinceAndUntrackedFilesReflectWorkerChanges(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	setTestGitEnv(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAll(ctx, dir, "first commit"); err != nil {
		t.Fatal(err)
	}

	base, err := BaseCommit(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}

	// No changes yet: diff and untracked list are both empty.
	diff, err := DiffSince(ctx, dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if diff != "" {
		t.Fatalf("expected empty diff before any change, got %q", diff)
	}
	untracked, err := UntrackedFiles(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(untracked) != 0 {
		t.Fatalf("expected no untracked files, got %v", untracked)
	}

	// Modify a tracked file and add a new (untracked) one.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diff, err = DiffSince(ctx, dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if diff == "" || !strings.Contains(diff, "a.txt") {
		t.Fatalf("expected diff to mention a.txt, got %q", diff)
	}

	untracked, err = UntrackedFiles(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(untracked) != 1 || untracked[0] != "b.txt" {
		t.Fatalf("untracked = %v, want [b.txt]", untracked)
	}

	stat, err := DiffStatSince(ctx, dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if stat == "" || !strings.Contains(stat, "a.txt") {
		t.Fatalf("expected diff --stat to mention a.txt, got %q", stat)
	}

	restricted, err := DiffSinceForFiles(ctx, dir, base, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if restricted == "" || !strings.Contains(restricted, "a.txt") {
		t.Fatalf("expected restricted diff to mention a.txt, got %q", restricted)
	}
}

func TestParkAndRestoreBeforeFirstCommit(t *testing.T) {
	requireGit(t)
	setTestGitEnv(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "a[1].txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, _ := BaseCommit(ctx, dir)

	paths, err := ChangedPathsExcluding(ctx, dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "sub/a[1].txt" {
		t.Fatalf("changed paths = %v", paths)
	}
	commit, err := ParkPaths(ctx, dir, base, "parked", "park", paths)
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := run(ctx, dir, "rev-list", "--count", commit); strings.TrimSpace(out) != "1" {
		t.Errorf("parked commit should be parentless, rev-list --count = %q", out)
	}
	if err := RestorePaths(ctx, dir, base, paths); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub")); !os.IsNotExist(err) {
		t.Errorf("sub/ should be gone after restoring to the empty tree, stat err = %v", err)
	}
	if out, _ := run(ctx, dir, "symbolic-ref", "-q", "HEAD"); strings.TrimSpace(out) == "refs/heads/parked" {
		t.Error("parking must not switch branches")
	}
	if _, err := ParkPaths(ctx, dir, base, "parked", "again", paths); err == nil {
		t.Error("ParkPaths must never overwrite an existing branch")
	}
}
