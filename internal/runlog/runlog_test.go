package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 123_000_000, time.UTC)

func TestOpenPicksAUniqueRunDir(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	a, err := Open(runs, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(runs, testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.ID != "20261008-120000" || b.ID != "20261008-120000-2" {
		t.Errorf("ids = %q, %q; want 20261008-120000 and 20261008-120000-2", a.ID, b.ID)
	}
	for _, r := range []*Run{a, b} {
		if _, err := os.Stat(filepath.Join(runs, r.ID, LogName)); err != nil {
			t.Errorf("run.log missing: %v", err)
		}
	}
}

func TestFormat(t *testing.T) {
	got := Format(testNow, Event{Name: "run_start", Fields: []Field{F("run", "x")}})
	want := "2026-10-08T12:00:00.123Z task=- iter=- agent=- event=run_start run=x"
	if got != want {
		t.Errorf("Format = %q, want %q", got, want)
	}
	got = Format(testNow, Event{Task: "001_a.md", Iter: 2, Agent: "worker", Name: "warning",
		Fields: []Field{F("error", "no such\nfile"), F("n", 3), F("empty", "")}})
	want = `2026-10-08T12:00:00.123Z task=001_a.md iter=2 agent=worker event=warning error="no such\nfile" n=3 empty=-`
	if got != want {
		t.Errorf("Format = %q, want %q", got, want)
	}
}

func TestWriteIsInTheFileImmediately(t *testing.T) {
	r, err := Open(t.TempDir(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, line := range []string{"first", "second"} {
		if err := r.Write(line); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(r.LogPath())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(string(b), line+"\n") {
			t.Errorf("run.log = %q, want it to end with %q", b, line)
		}
	}
}

func TestCallNameNumbersRepeatedCalls(t *testing.T) {
	r, err := Open(t.TempDir(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got := []string{
		r.CallName("001-a", 1, "planner"),
		r.CallName("001-a", 1, "planner"),
		r.CallName("001-a", 1, "worker"),
		r.CallName("001-a", 2, "planner"),
		r.CallName("002-b", 1, "planner"),
	}
	want := []string{"iter-1-planner", "iter-1-planner-2", "iter-1-worker", "iter-2-planner", "iter-1-planner"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("CallName = %v, want %v", got, want)
	}
}

func TestArtifacts(t *testing.T) {
	r, err := Open(t.TempDir(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if err := r.CopyArtifact("001-a", "iter-1-REVIEW.md", filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("CopyArtifact of a missing source: %v", err)
	}
	if _, err := os.Stat(r.TaskDir("001-a")); !os.IsNotExist(err) {
		t.Errorf("task dir created for a missing source: %v", err)
	}

	src := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(src, []byte("plan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.CopyArtifact("001-a", "iter-1-IMPL_PLAN.md", src); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(r.TaskDir("001-a"), "iter-1-IMPL_PLAN.md")); string(b) != "plan" {
		t.Errorf("copied plan = %q", b)
	}

	if err := os.WriteFile(r.TaskDir("002-b"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteArtifact("002-b", "final.diff", []byte("x")); err == nil {
		t.Error("WriteArtifact into a path blocked by a file: want an error")
	}
}

func TestNilRunIsANoOp(t *testing.T) {
	var r *Run
	if err := r.Write("x"); err != nil {
		t.Error(err)
	}
	if err := r.WriteArtifact("a", "b", nil); err != nil {
		t.Error(err)
	}
	if err := r.CopyArtifact("a", "b", "c"); err != nil {
		t.Error(err)
	}
	if got := r.CallName("a", 1, "worker"); got != "iter-1-worker" {
		t.Errorf("CallName = %q", got)
	}
	if r.TaskDir("a") != "" || r.LogPath() != "" {
		t.Error("paths of a nil Run should be empty")
	}
	if err := r.Close(); err != nil {
		t.Error(err)
	}
}
