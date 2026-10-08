package specs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDiscoverTaskFilesExcludesGenerated(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"001_segment-package.md",
		"01-legacy-style.md",
		"001_IMPL_PLAN.md",
		"001_REVIEW_1.md",
		"001_FAILED.md",
		"001_BASE",
		"STATUS.md",
		"QUEUE.md",
		"notes.txt",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := DiscoverTaskFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"001_segment-package.md", "01-legacy-style.md"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}

func TestStatusRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "STATUS.md")
	rows := []Row{
		{Num: "01", Spec: "01-segment-package.md", Status: StatusDone},
		{Num: "002", Spec: "002_other.md", Status: StatusQueued},
	}
	if err := WriteStatus(path, rows); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != rows[0] || got[1] != rows[1] {
		t.Fatalf("round trip mismatch: got %+v, want %+v", got, rows)
	}
}

func TestSyncQueuesNewTasksAndRecoversInProgress(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}

	// Pre-existing state: one task already IN_PROGRESS (simulating a crash
	// mid-run) that must be recovered into the queue.
	if err := WriteStatus(l.StatusPath, []Row{
		{Num: "001", Spec: "001_old.md", Status: StatusInProgress},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.SpecDir, "001_old.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A brand new task spec not yet in STATUS.md.
	if err := os.WriteFile(filepath.Join(l.SpecDir, "002_new.md"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	rows, queue, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %+v", rows)
	}
	foundNew := false
	for _, r := range rows {
		if r.Spec == "002_new.md" {
			foundNew = true
			if r.Status != StatusQueued {
				t.Errorf("new task status = %s, want QUEUED", r.Status)
			}
		}
	}
	if !foundNew {
		t.Fatalf("new task not added to STATUS.md: %+v", rows)
	}

	if len(queue) != 2 {
		t.Fatalf("expected 2 queued items, got %v", queue)
	}
}

func TestPopQueueAndSetStatus(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := WriteStatus(l.StatusPath, []Row{{Num: "001", Spec: "001_a.md", Status: StatusQueued}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteQueue(l.QueuePath, []string{"001_a.md"}); err != nil {
		t.Fatal(err)
	}

	next, err := PopQueue(l)
	if err != nil {
		t.Fatal(err)
	}
	if next != "001_a.md" {
		t.Fatalf("got %q, want 001_a.md", next)
	}

	remaining, err := ReadQueue(l.QueuePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected empty queue, got %v", remaining)
	}

	if err := SetStatus(l, "001_a.md", StatusDone); err != nil {
		t.Fatal(err)
	}
	status, err := GetStatus(l, "001_a.md")
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusDone {
		t.Fatalf("status = %s, want DONE", status)
	}
}

// TestSyncOrdersQueueAscendingByNumber guards the restart contract: after a
// restart, work must resume with the lowest-numbered pending task first,
// regardless of what order rows happen to sit in STATUS.md (e.g. because
// tasks were discovered or recovered out of order across several runs).
func TestSyncOrdersQueueAscendingByNumber(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}

	// Deliberately out of numeric order, and with a DONE row interleaved
	// that must be excluded from the queue entirely.
	if err := WriteStatus(l.StatusPath, []Row{
		{Num: "003", Spec: "003_c.md", Status: StatusInProgress},
		{Num: "001", Spec: "001_a.md", Status: StatusQueued},
		{Num: "002", Spec: "002_b.md", Status: StatusDone},
		{Num: "010", Spec: "010_j.md", Status: StatusQueued},
	}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"003_c.md", "001_a.md", "002_b.md", "010_j.md"} {
		if err := os.WriteFile(filepath.Join(l.SpecDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, queue, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"001_a.md", "003_c.md", "010_j.md"}
	if len(queue) != len(want) {
		t.Fatalf("queue = %v, want %v", queue, want)
	}
	for i := range want {
		if queue[i] != want[i] {
			t.Fatalf("queue = %v, want %v", queue, want)
		}
	}
}

func TestNewLayoutPrefixPath(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, ".claude", "diadoche", "prefix.md")
	if got := NewLayout(dir).PrefixPath; got != want {
		t.Errorf("PrefixPath = %q, want %q", got, want)
	}
}

func TestStatusReasonRoundTripsAndIsSanitized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "STATUS.md")
	rows := []Row{
		{Num: "001", Spec: "001_a.md", Status: StatusBlocked, Reason: "merge failed: conflict in a|b\nsecond  line"},
		{Num: "002", Spec: "002_b.md", Status: StatusDone},
	}
	if err := WriteStatus(path, rows); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "| BLOCKED: merge failed: conflict in a b second line |") {
		t.Errorf("STATUS.md should carry the sanitized reason in the Status cell, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "DONE:") {
		t.Errorf("a row without a reason must not get a ':', got:\n%s", raw)
	}
	got, err := ReadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []Row{
		{Num: "001", Spec: "001_a.md", Status: StatusBlocked, Reason: "merge failed: conflict in a b second line"},
		{Num: "002", Spec: "002_b.md", Status: StatusDone},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLongReasonIsCapped(t *testing.T) {
	r := sanitizeReason(strings.Repeat("é", 400))
	if len(r) > maxReasonLen+len("…") || !strings.HasSuffix(r, "…") || !utf8.ValidString(r) {
		t.Errorf("reason not capped on a rune boundary: %d bytes, %q", len(r), r[len(r)-10:])
	}
}

func TestSetStatusClearsReason(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := WriteStatus(l.StatusPath, []Row{{Num: "001", Spec: "001_a.md", Status: StatusQueued}}); err != nil {
		t.Fatal(err)
	}
	if err := SetStatusReason(l, "001_a.md", StatusBlocked, "conflict"); err != nil {
		t.Fatal(err)
	}
	rows, _ := ReadStatus(l.StatusPath)
	if rows[0].Status != StatusBlocked || rows[0].Reason != "conflict" {
		t.Fatalf("got %+v", rows[0])
	}
	if err := SetStatus(l, "001_a.md", StatusQueued); err != nil {
		t.Fatal(err)
	}
	rows, _ = ReadStatus(l.StatusPath)
	if rows[0].Status != StatusQueued || rows[0].Reason != "" {
		t.Fatalf("SetStatus should clear the reason, got %+v", rows[0])
	}
}

func TestSyncDoesNotRequeueBlocked(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"001_blocked.md", "002_failed.md", "003_queued.md"} {
		if err := os.WriteFile(filepath.Join(l.SpecDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteStatus(l.StatusPath, []Row{
		{Num: "001", Spec: "001_blocked.md", Status: StatusBlocked, Reason: "merge conflict"},
		{Num: "002", Spec: "002_failed.md", Status: StatusFailed},
	}); err != nil {
		t.Fatal(err)
	}
	rows, queue, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || queue[0] != "003_queued.md" {
		t.Errorf("queue = %v, want only 003_queued.md", queue)
	}
	onDisk, _ := ReadQueue(l.QueuePath)
	if len(onDisk) != 1 || onDisk[0] != "003_queued.md" {
		t.Errorf("QUEUE.md = %v, want only 003_queued.md", onDisk)
	}
	if rows[0].Status != StatusBlocked || rows[0].Reason != "merge conflict" {
		t.Errorf("BLOCKED row changed by Sync: %+v", rows[0])
	}
	persisted, _ := ReadStatus(l.StatusPath)
	if persisted[0].Status != StatusBlocked || persisted[0].Reason != "merge conflict" {
		t.Errorf("BLOCKED row not persisted: %+v", persisted[0])
	}
}

func TestReadQueueDropsTrailingNoTestsMarker(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.QueuePath, []byte("- 002_x.md [no-tests]\n- 003_y.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadQueue(l.QueuePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "002_x.md" || got[1] != "003_y.md" {
		t.Fatalf("ReadQueue = %q, want [002_x.md 003_y.md]", got)
	}
	next, err := PopQueue(l)
	if err != nil {
		t.Fatal(err)
	}
	if next != "002_x.md" {
		t.Fatalf("PopQueue = %q, want 002_x.md", next)
	}
}

func TestHasNoTestsMarker(t *testing.T) {
	cases := []struct {
		spec string
		want bool
	}{
		{"# 001. Task\n\n" + NoTestsMarker + "\n\nbody\n", true},
		{"# 001. Task\n  <!--   diadoche:  no-tests   -->  \n", true},
		{"# 001. Task\n\nno tests needed\n", false},
		{"text <!-- diadoche: no-tests --> inline\n", false},
	}
	for _, c := range cases {
		if got := HasNoTestsMarker([]byte(c.spec)); got != c.want {
			t.Errorf("HasNoTestsMarker(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}
