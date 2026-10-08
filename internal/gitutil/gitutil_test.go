package gitutil

import (
	"context"
	"errors"
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

func TestReadUntrackedReturnsTextContentAndFlagsBinary(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}

	const textContent = "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte(textContent), 0o644); err != nil {
		t.Fatal(err)
	}
	binContent := []byte{0x7f, 'E', 'L', 'F', 0x00, 0x01, 0x02}
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), binContent, 0o644); err != nil {
		t.Fatal(err)
	}

	// "gone.txt" stands for a file git listed as untracked that vanished
	// before it could be read — a skip, not an error.
	files, err := ReadUntracked(dir, []string{"new.go", "blob.bin", "gone.txt"})
	if err != nil {
		t.Fatalf("ReadUntracked: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2 (the missing one skipped): %+v", len(files), files)
	}
	if files[0].Path != "new.go" || files[1].Path != "blob.bin" {
		t.Errorf("input order not preserved: got %q, %q", files[0].Path, files[1].Path)
	}
	if files[0].Content != textContent {
		t.Errorf("new.go content = %q, want %q", files[0].Content, textContent)
	}
	if files[0].Binary {
		t.Error("new.go reported as binary")
	}
	if files[0].Size != int64(len(textContent)) {
		t.Errorf("new.go size = %d, want %d", files[0].Size, len(textContent))
	}
	if !files[1].Binary {
		t.Error("blob.bin not reported as binary")
	}
	if files[1].Content != "" {
		t.Errorf("blob.bin content = %q, want empty for a binary file", files[1].Content)
	}
	if files[1].Size != int64(len(binContent)) {
		t.Errorf("blob.bin size = %d, want %d", files[1].Size, len(binContent))
	}
}

// newCommittedRepo returns a temp repo with a.txt committed on its
// current branch.
func newCommittedRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	setTestGitEnv(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAll(ctx, dir, "initial"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addTaskWorktree creates worktree wt on branch "task" and commits a
// change to file with content in it.
func addTaskWorktree(t *testing.T, dir, file, content string) string {
	t.Helper()
	ctx := context.Background()
	wt := filepath.Join(dir, "wts", "task")
	head := git(t, dir, "rev-parse", "HEAD")
	if err := WorktreeAdd(ctx, dir, wt, "task", head); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(wt, file), content)
	if _, err := CommitAllExcluding(ctx, wt, "task change"); err != nil {
		t.Fatal(err)
	}
	return wt
}

func TestWorktreeAddRemoveAndReattach(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	wt := filepath.Join(dir, "wts", "task")
	head := git(t, dir, "rev-parse", "HEAD")

	if err := WorktreeAdd(ctx, dir, wt, "task", head); err != nil {
		t.Fatal(err)
	}
	if !BranchExists(ctx, dir, "task") || !IsWorktree(ctx, dir, wt) {
		t.Fatal("branch and worktree should exist")
	}
	if got := git(t, wt, "symbolic-ref", "--short", "HEAD"); got != "task" {
		t.Errorf("worktree branch = %q", got)
	}
	writeTestFile(t, filepath.Join(wt, "b.txt"), "b\n")
	if _, err := CommitAllExcluding(ctx, wt, "b"); err != nil {
		t.Fatal(err)
	}
	tip := git(t, dir, "rev-parse", "task")

	if err := WorktreeRemove(ctx, dir, wt); err != nil {
		t.Fatal(err)
	}
	if IsWorktree(ctx, dir, wt) {
		t.Error("IsWorktree should be false after removal")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone, stat err = %v", err)
	}
	// Removing again is not an error.
	if err := WorktreeRemove(ctx, dir, wt); err != nil {
		t.Errorf("second WorktreeRemove: %v", err)
	}

	// The kept branch is attached again, not recreated at head.
	if err := WorktreeAdd(ctx, dir, wt, "task", head); err != nil {
		t.Fatal(err)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != tip {
		t.Errorf("reattached worktree HEAD = %s, want branch tip %s", got, tip)
	}
	if base, err := MergeBase(ctx, dir, "HEAD", "task"); err != nil || base != head {
		t.Errorf("MergeBase = %q, %v; want %s", base, err, head)
	}
}

func TestCommitAllExcludingSkipsExcludedDirs(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	writeTestFile(t, filepath.Join(dir, "code.txt"), "code\n")
	writeTestFile(t, filepath.Join(dir, ".claude", "diadoche", "spec", "x.md"), "bookkeeping\n")

	committed, err := CommitAllExcluding(ctx, dir, "task", ".claude/diadoche", ".claude/agents")
	if err != nil || !committed {
		t.Fatalf("committed=%v err=%v", committed, err)
	}
	files := git(t, dir, "show", "--name-only", "--format=", "HEAD")
	if files != "code.txt" {
		t.Errorf("commit should hold only code.txt, has %q", files)
	}
	committed, err = CommitAllExcluding(ctx, dir, "nothing", ".claude/diadoche")
	if err != nil || committed {
		t.Errorf("nothing to commit: committed=%v err=%v", committed, err)
	}
}

func TestMergeFastForward(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	addTaskWorktree(t, dir, "b.txt", "b\n")

	ff, err := Merge(ctx, dir, "task", "Merge task")
	if err != nil || !ff {
		t.Fatalf("ff=%v err=%v", ff, err)
	}
	if git(t, dir, "rev-parse", "HEAD") != git(t, dir, "rev-parse", "task") {
		t.Error("HEAD should equal the branch tip after a fast-forward")
	}
}

func TestMergeCreatesMergeCommitWhenDiverged(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	addTaskWorktree(t, dir, "b.txt", "b\n")
	writeTestFile(t, filepath.Join(dir, "c.txt"), "c\n")
	if err := CommitAll(ctx, dir, "main change"); err != nil {
		t.Fatal(err)
	}

	ff, err := Merge(ctx, dir, "task", "Merge task")
	if err != nil || ff {
		t.Fatalf("ff=%v err=%v", ff, err)
	}
	if parents := strings.Fields(git(t, dir, "log", "-1", "--format=%P")); len(parents) != 2 {
		t.Errorf("HEAD parents = %v, want a merge commit", parents)
	}
	if msg := git(t, dir, "log", "-1", "--format=%s"); msg != "Merge task" {
		t.Errorf("merge message = %q", msg)
	}
}

func TestMergeConflictIsAborted(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	addTaskWorktree(t, dir, "a.txt", "from task\n")
	writeTestFile(t, filepath.Join(dir, "a.txt"), "from main\n")
	if err := CommitAll(ctx, dir, "main change"); err != nil {
		t.Fatal(err)
	}
	head := git(t, dir, "rev-parse", "HEAD")

	_, err := Merge(ctx, dir, "task", "Merge task")
	var mErr *MergeError
	if !errors.As(err, &mErr) || !mErr.Conflict {
		t.Fatalf("err = %v, want a conflict *MergeError", err)
	}
	if _, err := exec.Command("git", "-C", dir, "rev-parse", "-q", "--verify", "MERGE_HEAD").Output(); err == nil {
		t.Error("MERGE_HEAD should be gone after the abort")
	}
	if st := git(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("tree should be clean after the abort, got:\n%s", st)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s", got)
	}
}

func TestRenameAndDeleteBranch(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	git(t, dir, "branch", "one")
	if err := RenameBranch(ctx, dir, "one", "x/two"); err != nil {
		t.Fatal(err)
	}
	if BranchExists(ctx, dir, "one") || !BranchExists(ctx, dir, "x/two") {
		t.Fatal("rename did not happen")
	}
	if err := DeleteBranch(ctx, dir, "x/two"); err != nil {
		t.Fatal(err)
	}
	if BranchExists(ctx, dir, "x/two") {
		t.Error("branch should be deleted")
	}
}

func TestChangedPathsBetweenSkipsDeletions(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	base := git(t, dir, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(dir, "b.txt"), "b\n")
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := CommitAll(ctx, dir, "change"); err != nil {
		t.Fatal(err)
	}
	got, err := ChangedPathsBetween(ctx, dir, base, "HEAD")
	if err != nil || len(got) != 1 || got[0] != "b.txt" {
		t.Errorf("got %v, %v; want [b.txt]", got, err)
	}
}

func TestEnsureInfoExcludeIsIdempotent(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	const pattern = "/.claude/diadoche/worktrees/"
	for range 2 {
		if err := EnsureInfoExclude(ctx, dir, pattern); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), pattern+"\n"); n != 1 {
		t.Errorf("pattern appears %d times, want 1:\n%s", n, data)
	}
	writeTestFile(t, filepath.Join(dir, ".claude", "diadoche", "worktrees", "x", "f.txt"), "x\n")
	if st := git(t, dir, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Errorf("excluded dir should be ignored, status:\n%s", st)
	}
}

func TestWorktreeAddDetachedAtBaseAndRemove(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	first := git(t, dir, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(dir, "b.txt"), "b\n")
	if err := CommitAll(ctx, dir, "second"); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(t.TempDir(), "check", "wt")
	if err := WorktreeAddDetached(ctx, dir, wt, first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "a.txt")); err != nil {
		t.Errorf("a.txt missing in detached worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("b.txt from the second commit present in worktree at the first commit (err=%v)", err)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != first {
		t.Errorf("worktree HEAD = %s, want %s", got, first)
	}
	if got := CurrentBranch(ctx, wt); got != "HEAD" {
		t.Errorf("worktree branch = %q, want detached HEAD", got)
	}
	if !IsWorktree(ctx, dir, wt) {
		t.Fatal("detached worktree not registered")
	}

	if err := WorktreeRemove(ctx, dir, wt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree dir still exists (err=%v)", err)
	}
	if IsWorktree(ctx, dir, wt) {
		t.Error("worktree still listed after WorktreeRemove")
	}
}

func TestPathsInTreeAndShowFile(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	base := git(t, dir, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(dir, "a.txt"), "changed\n")
	writeTestFile(t, filepath.Join(dir, "sub", "new.txt"), "new\n")

	in, err := PathsInTree(ctx, dir, base, []string{"a.txt", "sub/new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !in["a.txt"] || in["sub/new.txt"] {
		t.Errorf("PathsInTree = %v, want only a.txt", in)
	}
	if empty, err := PathsInTree(ctx, dir, base, nil); err != nil || len(empty) != 0 {
		t.Errorf("PathsInTree(nil) = %v, %v; want empty map", empty, err)
	}

	got, err := ShowFile(ctx, dir, base, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "a\n" {
		t.Errorf("ShowFile = %q, want base content %q", got, "a\n")
	}
	if _, err := ShowFile(ctx, dir, base, "sub/new.txt"); err == nil {
		t.Error("ShowFile of a path absent at base: want error")
	}
	if !IsEmptyTree(emptyTreeHash) || IsEmptyTree(base) {
		t.Error("IsEmptyTree misreports")
	}
}

func TestDiffUntrackedRendersNewFilePatches(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	ctx := context.Background()
	if err := EnsureRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := DiffUntracked(ctx, dir, []string{"sub/new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "new file mode") || !strings.Contains(out, "+brand new") || !strings.Contains(out, "sub/new.txt") {
		t.Errorf("DiffUntracked = %q, want a new-file patch of sub/new.txt", out)
	}
	if out, err := DiffUntracked(ctx, dir, nil); err != nil || out != "" {
		t.Errorf("DiffUntracked(nil) = %q, %v; want empty", out, err)
	}
	if _, err := DiffUntracked(ctx, dir, []string{"missing.txt"}); err == nil {
		t.Error("DiffUntracked of a missing file: want an error")
	}
}

func TestApplyCommitBringsBackParkedChangesUncommitted(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	head := git(t, dir, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(dir, "a.txt"), "a edited\n")
	writeTestFile(t, filepath.Join(dir, "new", "b.txt"), "b\n")
	commit, err := ParkPaths(ctx, dir, head, "parked", "park", []string{"a.txt", "new/b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := RestorePaths(ctx, dir, head, []string{"a.txt", "new/b.txt"}); err != nil {
		t.Fatal(err)
	}

	if err := ApplyCommit(ctx, dir, "parked"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "a edited\n" {
		t.Errorf("a.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "new", "b.txt")); string(b) != "b\n" {
		t.Errorf("new/b.txt = %q", b)
	}
	if got := git(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s", got)
	}
	if staged := git(t, dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("changes left staged: %s", staged)
	}
	if git(t, dir, "rev-parse", "parked") != commit {
		t.Error("parked branch changed")
	}
}

func TestApplyCommitAbortsOnConflict(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	head := git(t, dir, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(dir, "a.txt"), "parked version\n")
	if _, err := ParkPaths(ctx, dir, head, "parked", "park", []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "a.txt"), "something else\n")
	if err := CommitAll(ctx, dir, "diverge"); err != nil {
		t.Fatal(err)
	}

	if err := ApplyCommit(ctx, dir, "parked"); err == nil {
		t.Fatal("ApplyCommit succeeded on a conflict")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "something else\n" {
		t.Errorf("a.txt = %q, want the tree as it was", b)
	}
	if st := git(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("tree not clean after abort:\n%s", st)
	}
}

func TestDiffBetweenExcluding(t *testing.T) {
	dir := newCommittedRepo(t)
	ctx := context.Background()
	from := git(t, dir, "rev-parse", "HEAD")
	writeTestFile(t, filepath.Join(dir, "code.txt"), "new code\n")
	writeTestFile(t, filepath.Join(dir, ".claude", "diadoche", "x.md"), "bookkeeping\n")
	if err := CommitAll(ctx, dir, "change"); err != nil {
		t.Fatal(err)
	}
	to := git(t, dir, "rev-parse", "HEAD")
	// A later uncommitted change is not part of the diff between commits.
	writeTestFile(t, filepath.Join(dir, "later.txt"), "later\n")
	writeTestFile(t, filepath.Join(dir, "a.txt"), "changed later\n")

	diff, err := DiffBetweenExcluding(ctx, dir, from, to, ".claude/diadoche")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "+new code") {
		t.Errorf("diff misses the committed change:\n%s", diff)
	}
	for _, unwanted := range []string{"bookkeeping", "changed later", "later.txt"} {
		if strings.Contains(diff, unwanted) {
			t.Errorf("diff contains %q:\n%s", unwanted, diff)
		}
	}
	stat, err := DiffStatBetweenExcluding(ctx, dir, from, to, ".claude/diadoche")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stat, "code.txt") || strings.Contains(stat, ".claude") {
		t.Errorf("stat = %q, want code.txt only", stat)
	}
}
