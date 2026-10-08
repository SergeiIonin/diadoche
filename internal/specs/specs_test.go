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

func TestSyncRequeuesPaused(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"001_done.md", "002_paused.md", "003_queued.md"} {
		if err := os.WriteFile(filepath.Join(l.SpecDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reason := "claude usage window exhausted, resumes at 2026-10-08T19:00:00+03:00"
	if err := WriteStatus(l.StatusPath, []Row{
		{Num: "003", Spec: "003_queued.md", Status: StatusQueued},
		{Num: "001", Spec: "001_done.md", Status: StatusDone},
		{Num: "002", Spec: "002_paused.md", Status: StatusPaused, Reason: reason},
	}); err != nil {
		t.Fatal(err)
	}
	_, queue, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 || queue[0] != "002_paused.md" || queue[1] != "003_queued.md" {
		t.Errorf("queue = %v, want [002_paused.md 003_queued.md]", queue)
	}
	persisted, _ := ReadStatus(l.StatusPath)
	if persisted[2].Status != StatusPaused || persisted[2].Reason != reason {
		t.Errorf("PAUSED row not round-tripped: %+v", persisted[2])
	}
}

func TestWriteFileAtomicReplacesViaRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	oldInfo, err := old.Stat()
	if err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(path, []byte("new")); err != nil {
		t.Fatal(err)
	}

	newInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(oldInfo, newInfo) {
		t.Error("WriteFileAtomic rewrote the file in place; want a new file renamed over it")
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Errorf("contents = %q, want %q", b, "new")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".state.json.tmp*")); len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestPendingQueueWritesNothing(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	if err := os.MkdirAll(l.SpecDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteStatus(l.StatusPath, []Row{
		{Num: "003", Spec: "003_c.md", Status: StatusInProgress},
		{Num: "001", Spec: "001_a.md", Status: StatusQueued},
		{Num: "002", Spec: "002_b.md", Status: StatusDone},
		{Num: "004", Spec: "004_d.md", Status: StatusFailed},
		{Num: "005", Spec: "005_e.md", Status: StatusPaused},
	}); err != nil {
		t.Fatal(err)
	}
	// 006 is a task file STATUS.md doesn't list yet.
	for _, f := range []string{"001_a.md", "002_b.md", "003_c.md", "004_d.md", "005_e.md", "006_f.md"} {
		if err := os.WriteFile(filepath.Join(l.SpecDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	statusBefore, err := os.ReadFile(l.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	entriesBefore, err := os.ReadDir(l.SpecDir)
	if err != nil {
		t.Fatal(err)
	}

	queue, err := PendingQueue(l)
	if err != nil {
		t.Fatal(err)
	}

	statusAfter, err := os.ReadFile(l.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(statusAfter) != string(statusBefore) {
		t.Fatalf("STATUS.md changed:\n%s\nwant:\n%s", statusAfter, statusBefore)
	}
	if _, err := os.Stat(l.QueuePath); !os.IsNotExist(err) {
		t.Fatalf("QUEUE.md was created (stat err = %v)", err)
	}
	entriesAfter, err := os.ReadDir(l.SpecDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("spec dir has %d entries after PendingQueue, want %d", len(entriesAfter), len(entriesBefore))
	}

	_, synced, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"001_a.md", "003_c.md", "005_e.md", "006_f.md"}
	if strings.Join(queue, ",") != strings.Join(want, ",") {
		t.Fatalf("PendingQueue = %v, want %v", queue, want)
	}
	if strings.Join(queue, ",") != strings.Join(synced, ",") {
		t.Fatalf("PendingQueue = %v, Sync queue = %v", queue, synced)
	}
}

func TestPendingQueueOnMissingSpecDir(t *testing.T) {
	dir := t.TempDir()
	l := NewLayout(dir)
	queue, err := PendingQueue(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 0 {
		t.Fatalf("queue = %v, want empty", queue)
	}
	if _, err := os.Stat(l.Root); !os.IsNotExist(err) {
		t.Fatalf("%s was created (stat err = %v)", l.Root, err)
	}
}

func TestDiscoverTaskFilesIgnoresBlockedReport(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"001_example.md", "001_BLOCKED.md", "001_FAILED.md"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DiscoverTaskFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "001_example.md" {
		t.Errorf("DiscoverTaskFiles = %v, want only 001_example.md", got)
	}
}

func TestParseDependsOn(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"# T\n\nDepends-on: 001\n", []string{"001"}},
		{"Depends-on: 001, 3\n", []string{"001", "3"}},
		{"  depends-ON:   007 008\n", []string{"007", "008"}},
		{"Depends-on: 016_foo.md, `017`\n", []string{"016", "017"}},
		{"Depends-on: none, 002, x7\n", []string{"002"}},
		{"Depends-on: 1\nDepends-on: 001, 2\n", []string{"1", "2"}},
		{"# T\nno such line\nThis depends-on: 001 inline\n", nil},
		{"", nil},
	}
	for _, c := range cases {
		got := ParseDependsOn([]byte(c.in))
		if strings.Join(got, ",") != strings.Join(c.want, ",") || (got == nil) != (c.want == nil) {
			t.Errorf("ParseDependsOn(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestBlockDependentsCascades(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	specs := map[string]string{
		"001_a.md": "# A\n",
		"002_b.md": "# B\nDepends-on: 001\n",
		"003_c.md": "# C\nDepends-on: 2\n",
		"004_d.md": "# D\n",
		"005_e.md": "# E\nDepends-on: 001\n",
		"006_f.md": "# F\nDepends-on: 004\n",
	}
	for f, body := range specs {
		if err := os.WriteFile(filepath.Join(l.SpecDir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rows := []Row{
		{Num: "001", Spec: "001_a.md", Status: StatusBlocked, Reason: "on-fail"},
		{Num: "002", Spec: "002_b.md", Status: StatusQueued},
		{Num: "003", Spec: "003_c.md", Status: StatusPaused},
		{Num: "004", Spec: "004_d.md", Status: StatusQueued},
		{Num: "005", Spec: "005_e.md", Status: StatusDone},
		{Num: "006", Spec: "006_f.md", Status: StatusQueued},
	}
	if err := WriteStatus(l.StatusPath, rows); err != nil {
		t.Fatal(err)
	}

	blocked, err := BlockDependents(l, "001_a.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 2 || blocked[0].Spec != "002_b.md" || blocked[1].Spec != "003_c.md" {
		t.Fatalf("newly blocked = %+v, want 002 and 003", blocked)
	}

	got, err := ReadStatus(l.StatusPath)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Row{
		"001_a.md": {Status: StatusBlocked, Reason: "on-fail"},
		"002_b.md": {Status: StatusBlocked, Reason: "depends on 001_a.md, which is BLOCKED"},
		"003_c.md": {Status: StatusBlocked, Reason: "depends on 002_b.md, which is BLOCKED"},
		"004_d.md": {Status: StatusQueued},
		"005_e.md": {Status: StatusDone},
		"006_f.md": {Status: StatusQueued},
	}
	for _, r := range got {
		w := want[r.Spec]
		if r.Status != w.Status || r.Reason != w.Reason {
			t.Errorf("%s = %s: %q, want %s: %q", r.Spec, r.Status, r.Reason, w.Status, w.Reason)
		}
	}

	// The blocked rows are never pending again.
	_, queue, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(queue, ",") != "004_d.md,006_f.md" {
		t.Errorf("queue after cascade = %v, want only 004 and 006", queue)
	}
}

func TestBlockDependentsWithoutDependantsWritesNothing(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.SpecDir, "002_b.md"), []byte("# B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteStatus(l.StatusPath, []Row{{Num: "001", Spec: "001_a.md", Status: StatusBlocked}, {Num: "002", Spec: "002_b.md", Status: StatusQueued}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(l.StatusPath)
	blocked, err := BlockDependents(l, "001_a.md")
	if err != nil || blocked != nil {
		t.Fatalf("BlockDependents = %v, %v; want nothing", blocked, err)
	}
	after, _ := os.Stat(l.StatusPath)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("STATUS.md was rewritten although nothing changed")
	}
}

func TestParseSplitFrom(t *testing.T) {
	cases := map[string]string{
		"# T\n\nSplit-from: 001_x.md\n":                     "001_x.md",
		"# T\n  split-FROM:   `001_x.md`  \n":               "001_x.md",
		"# T\nSplit-from: 001_a.md\nSplit-from: 002_b.md\n": "001_a.md",
		"# T\nno line here\n":                               "",
		"":                                                  "",
	}
	for in, want := range cases {
		if got := ParseSplitFrom([]byte(in)); got != want {
			t.Errorf("ParseSplitFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAppendSplitFrom(t *testing.T) {
	p := filepath.Join(t.TempDir(), "004_a.md")
	writeSpec(t, p, "# A")
	if err := AppendSplitFrom(p, "001_x.md"); err != nil {
		t.Fatal(err)
	}
	if err := AppendSplitFrom(p, "002_y.md"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "# A\n\nSplit-from: 001_x.md\n" {
		t.Errorf("content = %q", b)
	}
}

func writeSpec(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// splitLayout has 001_x.md BLOCKED, 002/003 QUEUED, and 004–006 split
// from 001_x.md, not in STATUS.md yet.
func splitLayout(t *testing.T) Layout {
	t.Helper()
	l := NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"001_x.md", "002_two.md", "003_three.md"} {
		writeSpec(t, filepath.Join(l.SpecDir, f), "# "+f+"\n")
	}
	for _, f := range []string{"004_a.md", "005_b.md", "006_c.md"} {
		writeSpec(t, filepath.Join(l.SpecDir, f), "# "+f+"\n\nSplit-from: 001_x.md\n")
	}
	if err := WriteStatus(l.StatusPath, []Row{
		{Num: "001", Spec: "001_x.md", Status: StatusBlocked, Reason: "split"},
		{Num: "002", Spec: "002_two.md", Status: StatusQueued},
		{Num: "003", Spec: "003_three.md", Status: StatusQueued},
	}); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSyncPlacesSplitSpecsAtParentPosition(t *testing.T) {
	l := splitLayout(t)
	_, queue, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"004_a.md", "005_b.md", "006_c.md", "002_two.md", "003_three.md"}
	if strings.Join(queue, ",") != strings.Join(want, ",") {
		t.Errorf("queue = %v, want %v", queue, want)
	}
	if got, _ := PendingQueue(l); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("PendingQueue = %v, want %v", got, want)
	}

	// 005 split again, into 007 and 008: they take 005's place.
	writeSpec(t, filepath.Join(l.SpecDir, "007_b1.md"), "# b1\n\nSplit-from: 005_b.md\n")
	writeSpec(t, filepath.Join(l.SpecDir, "008_b2.md"), "# b2\n\nSplit-from: 005_b.md\n")
	if err := SetStatusReason(l, "005_b.md", StatusBlocked, "split"); err != nil {
		t.Fatal(err)
	}
	_, queue, err = Sync(l)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"004_a.md", "007_b1.md", "008_b2.md", "006_c.md", "002_two.md", "003_three.md"}
	if strings.Join(queue, ",") != strings.Join(want, ",") {
		t.Errorf("nested queue = %v, want %v", queue, want)
	}
}

func TestSyncSurvivesSplitFromCycle(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeSpec(t, filepath.Join(l.SpecDir, "001_a.md"), "Split-from: 002_b.md\n")
	writeSpec(t, filepath.Join(l.SpecDir, "002_b.md"), "Split-from: 001_a.md\n")
	writeSpec(t, filepath.Join(l.SpecDir, "003_c.md"), "Split-from: 003_c.md\n")
	_, queue, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 3 {
		t.Errorf("queue = %v, want all three specs", queue)
	}
}

func TestApplySplit(t *testing.T) {
	l := NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"001_x.md", "002_two.md", "003_three.md"} {
		writeSpec(t, filepath.Join(l.SpecDir, f), "# "+f+"\n")
	}
	if err := WriteStatus(l.StatusPath, []Row{
		{Num: "001", Spec: "001_x.md", Status: StatusInProgress},
		{Num: "002", Spec: "002_two.md", Status: StatusQueued},
		{Num: "003", Spec: "003_three.md", Status: StatusQueued},
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteQueue(l.QueuePath, []string{"002_two.md", "003_three.md"}); err != nil {
		t.Fatal(err)
	}
	parts := []string{"004_a.md", "005_b.md", "006_c.md"}
	for _, f := range parts {
		writeSpec(t, filepath.Join(l.SpecDir, f), "# "+f+"\n\nSplit-from: 001_x.md\n")
	}

	queue, err := ApplySplit(l, "001_x.md", "split into 004_a.md, 005_b.md, 006_c.md", parts)
	if err != nil {
		t.Fatal(err)
	}
	wantQueue := "004_a.md,005_b.md,006_c.md,002_two.md,003_three.md"
	if strings.Join(queue, ",") != wantQueue {
		t.Errorf("queue = %v", queue)
	}
	rows, _ := ReadStatus(l.StatusPath)
	var got []string
	for _, r := range rows {
		got = append(got, r.Spec+"="+r.Status)
	}
	wantRows := "001_x.md=BLOCKED,004_a.md=QUEUED,005_b.md=QUEUED,006_c.md=QUEUED,002_two.md=QUEUED,003_three.md=QUEUED"
	if strings.Join(got, ",") != wantRows {
		t.Errorf("STATUS.md rows = %v, want %s", got, wantRows)
	}
	if rows[0].Reason != "split into 004_a.md, 005_b.md, 006_c.md" {
		t.Errorf("reason = %q", rows[0].Reason)
	}
	q, _ := ReadQueue(l.QueuePath)
	if strings.Join(q, ",") != wantQueue {
		t.Errorf("QUEUE.md = %v", q)
	}

	status1, _ := os.ReadFile(l.StatusPath)
	queue1, _ := os.ReadFile(l.QueuePath)
	if _, _, err := Sync(l); err != nil {
		t.Fatal(err)
	}
	status2, _ := os.ReadFile(l.StatusPath)
	queue2, _ := os.ReadFile(l.QueuePath)
	if string(status1) != string(status2) || string(queue1) != string(queue2) {
		t.Errorf("Sync changed what ApplySplit wrote:\n%s\n%s\nvs\n%s\n%s", status1, queue1, status2, queue2)
	}

	if _, err := ApplySplit(l, "009_missing.md", "x", nil); err == nil {
		t.Error("ApplySplit of a spec missing from STATUS.md: no error")
	}
}

func TestNewLayoutQuestionsPath(t *testing.T) {
	dir := t.TempDir()
	if got, want := NewLayout(dir).QuestionsPath, filepath.Join(dir, "QUESTIONS.md"); got != want {
		t.Errorf("QuestionsPath = %q, want %q", got, want)
	}
}

func TestDiscoverTaskFilesIgnoresWaitingReport(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"001_example.md", "001_WAITING.md"} {
		writeSpec(t, filepath.Join(dir, f), "x")
	}
	got, err := DiscoverTaskFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "001_example.md" {
		t.Errorf("DiscoverTaskFiles = %v, want only 001_example.md", got)
	}
}

// waitingLayout has 001 WAITING, 002 depending on 001, 003 depending on
// 002, and 004 independent; 002–004 QUEUED.
func waitingLayout(t *testing.T) Layout {
	t.Helper()
	l := NewLayout(t.TempDir())
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	writeSpec(t, filepath.Join(l.SpecDir, "001_ask.md"), "# ask\n")
	writeSpec(t, filepath.Join(l.SpecDir, "002_dep.md"), "# dep\nDepends-on: 001\n")
	writeSpec(t, filepath.Join(l.SpecDir, "003_dep2.md"), "# dep2\nDepends-on: 2\n")
	writeSpec(t, filepath.Join(l.SpecDir, "004_free.md"), "# free\n")
	if err := WriteStatus(l.StatusPath, []Row{
		{Num: "001", Spec: "001_ask.md", Status: StatusWaiting, Reason: "planner needs a human decision: answer in QUESTIONS.md"},
		{Num: "002", Spec: "002_dep.md", Status: StatusQueued},
		{Num: "003", Spec: "003_dep2.md", Status: StatusQueued},
		{Num: "004", Spec: "004_free.md", Status: StatusQueued},
	}); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestWaitingIsNotPending(t *testing.T) {
	l := waitingLayout(t)
	pending, err := PendingQueue(l)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pending, ",") != "002_dep.md,003_dep2.md,004_free.md" {
		t.Errorf("PendingQueue = %v, want no 001_ask.md", pending)
	}
	rows, queue, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(queue, ",") != "002_dep.md,003_dep2.md,004_free.md" {
		t.Errorf("Sync queue = %v, want no 001_ask.md", queue)
	}
	if rows[0].Status != StatusWaiting || !strings.Contains(rows[0].Reason, "QUESTIONS.md") {
		t.Errorf("WAITING row changed by Sync: %+v", rows[0])
	}
	if got, _ := WaitingSpecs(l); len(got) != 1 || got[0] != "001_ask.md" {
		t.Errorf("WaitingSpecs = %v", got)
	}
	// A blocked task's dependents never touch a WAITING row.
	if _, err := BlockDependents(l, "004_free.md"); err != nil {
		t.Fatal(err)
	}
	if s, _ := GetStatus(l, "001_ask.md"); s != StatusWaiting {
		t.Errorf("001 = %s, want WAITING", s)
	}
}

func TestResumeWaiting(t *testing.T) {
	l := waitingLayout(t)
	got, err := ResumeWaiting(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "001_ask.md" {
		t.Fatalf("ResumeWaiting = %v", got)
	}
	rows, _ := ReadStatus(l.StatusPath)
	if rows[0].Status != StatusQueued || rows[0].Reason != "" {
		t.Errorf("row = %+v, want QUEUED without a reason", rows[0])
	}
	_, queue, err := Sync(l)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) == 0 || queue[0] != "001_ask.md" {
		t.Errorf("queue = %v, want 001_ask.md first", queue)
	}
	before, _ := os.ReadFile(l.StatusPath)
	if got, err := ResumeWaiting(l); err != nil || got != nil {
		t.Errorf("second ResumeWaiting = %v, %v; want nothing", got, err)
	}
	after, _ := os.ReadFile(l.StatusPath)
	if string(before) != string(after) {
		t.Error("ResumeWaiting without WAITING rows rewrote STATUS.md")
	}
}

func TestWaitingDependency(t *testing.T) {
	l := waitingLayout(t)
	for spec, want := range map[string]string{
		"002_dep.md":  "001_ask.md",
		"003_dep2.md": "001_ask.md",
		"004_free.md": "",
	} {
		got, err := WaitingDependency(l, spec)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("WaitingDependency(%s) = %q, want %q", spec, got, want)
		}
	}
	// A Depends-on cycle ends.
	writeSpec(t, filepath.Join(l.SpecDir, "004_free.md"), "# free\nDepends-on: 4, 5\n")
	writeSpec(t, filepath.Join(l.SpecDir, "005_loop.md"), "# loop\nDepends-on: 4\n")
	if err := SetStatus(l, "001_ask.md", StatusQueued); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Sync(l); err != nil {
		t.Fatal(err)
	}
	if got, err := WaitingDependency(l, "004_free.md"); err != nil || got != "" {
		t.Errorf("WaitingDependency over a cycle = %q, %v", got, err)
	}
}
