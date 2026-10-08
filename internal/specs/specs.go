// Package specs manages the diadoche spec directory: STATUS.md (source of
// truth for task status) and QUEUE.md (work queue for the current run), plus
// discovery of new task spec files (spec/NNN_<slug>.md).
//
// A task's status is QUEUED, IN_PROGRESS, PAUSED (waiting out a claude
// usage or rate limit, see Row.Reason for when it resumes), DONE, FAILED or
// BLOCKED (waits for a human: DONE by tester, but its worktree branch
// couldn't be merged; not DONE after the --on-fail chain ended in block; or
// it depends on a BLOCKED task, see ParseDependsOn and BlockDependents) or
// WAITING (planner or worker asked a human in QUESTIONS.md and answered
// NEEDS_HUMAN; the reason points at QUESTIONS.md, and only --resume-waiting
// re-queues it, see ResumeWaiting). Only QUEUED, IN_PROGRESS and PAUSED
// tasks are pending: neither a run nor a restart picks up a WAITING task on
// its own.
//
// A task spec's Depends-on line (see DependsOn) has three states. Absent:
// the task implicitly depends on the pending task before it in queue order,
// so specs written without the line keep running one after another. This
// implicit dependency constrains scheduling only — the queue order — and
// never feeds the BLOCKED cascade: a FAILED or BLOCKED task does not block
// tasks that merely sit behind it. "Depends-on: none" (or an empty value):
// the task depends on nothing. A list of task numbers: exactly those tasks;
// the task starts only once all of them are DONE, and becomes BLOCKED when
// one of them is FAILED or BLOCKED (see CheckDependencies,
// BlockForDependency and BlockDependents). ValidateGraph rejects declared
// dependencies on missing tasks and dependency cycles; the pending queue is
// ordered topologically (see pendingQueue). A scheduler running several
// tasks at once starts a task only when CheckDependencies finds no blocker
// and nothing unfinished, and PendingPredecessors lists no pending task
// before it in that graph.
package specs

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	StatusQueued     = "QUEUED"
	StatusInProgress = "IN_PROGRESS"
	StatusPaused     = "PAUSED"
	StatusDone       = "DONE"
	StatusFailed     = "FAILED"
	StatusBlocked    = "BLOCKED"
	StatusWaiting    = "WAITING"
)

// Row is one line of the STATUS.md table.
type Row struct {
	Num    string
	Spec   string
	Status string

	// Reason explains Status, e.g. why a task is BLOCKED. It shares the
	// Status cell, written as "STATUS: reason", so STATUS.md stays a
	// three-column table; a cell without ":" has no reason.
	Reason string
}

// maxReasonLen caps a Reason written to STATUS.md, in bytes.
const maxReasonLen = 300

// sanitizeReason makes reason fit in a single table cell: no "|" or line
// breaks, whitespace collapsed, at most maxReasonLen bytes.
func sanitizeReason(reason string) string {
	reason = strings.NewReplacer("|", " ", "\r", " ", "\n", " ").Replace(reason)
	reason = strings.Join(strings.Fields(reason), " ")
	if len(reason) <= maxReasonLen {
		return reason
	}
	cut := maxReasonLen
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return strings.TrimSpace(reason[:cut]) + "…"
}

// Layout is the set of paths diadoche reads/writes for a given project root.
type Layout struct {
	Root       string // <project>/.claude/diadoche
	SpecDir    string // <root>/spec
	StatusPath string // <root>/spec/STATUS.md
	QueuePath  string // <root>/spec/QUEUE.md
	AgentsDir  string // <project>/.claude/agents
	SkillsDir  string // <project>/.claude/skills

	// PrefixPath is the optional per-project override of the shared
	// prompt prefix every agent of a run is given: <root>/prefix.md.
	PrefixPath string

	// QuestionsPath is the open questions file in the repository root:
	// <project>/QUESTIONS.md (see package questions).
	QuestionsPath string
}

func NewLayout(projectDir string) Layout {
	root := filepath.Join(projectDir, ".claude", "diadoche")
	specDir := filepath.Join(root, "spec")
	return Layout{
		Root:       root,
		SpecDir:    specDir,
		StatusPath: filepath.Join(specDir, "STATUS.md"),
		QueuePath:  filepath.Join(specDir, "QUEUE.md"),
		AgentsDir:  filepath.Join(projectDir, ".claude", "agents"),
		SkillsDir:  filepath.Join(projectDir, ".claude", "skills"),
		PrefixPath: filepath.Join(root, "prefix.md"),

		QuestionsPath: filepath.Join(projectDir, "QUESTIONS.md"),
	}
}

// Ensure creates the spec directory and an empty STATUS.md if missing.
func (l Layout) Ensure() error {
	if err := os.MkdirAll(l.SpecDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(l.StatusPath); errors.Is(err, os.ErrNotExist) {
		return WriteStatus(l.StatusPath, nil)
	} else if err != nil {
		return err
	}
	return nil
}

// taskFileRe matches spec/NNN_<slug>.md and spec/NNN-<slug>.md.
var taskFileRe = regexp.MustCompile(`^(\d+)[_-](.+)\.md$`)

// generatedRe matches slugs produced by diadoche itself, which are not
// user-authored task specs: NNN_IMPL_PLAN.md, NNN_REVIEW_<M>.md,
// NNN_FAILED.md, NNN_BLOCKED.md and NNN_WAITING.md, plus a re-executed
// task's archived first attempt, NNN_ATTEMPT<K>_IMPL_PLAN.md and
// NNN_ATTEMPT<K>_REVIEW_<M>.md.
var generatedRe = regexp.MustCompile(`(?i)^(IMPL_PLAN|REVIEW_\d+|FAILED|BLOCKED|WAITING|ATTEMPT\d+_(IMPL_PLAN|REVIEW_\d+))$`)

// DiscoverTaskFiles lists user-authored task spec filenames in specDir,
// excluding STATUS.md, QUEUE.md and diadoche-generated files.
func DiscoverTaskFiles(specDir string) ([]string, error) {
	entries, err := os.ReadDir(specDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "STATUS.md" || name == "QUEUE.md" {
			continue
		}
		m := taskFileRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		if generatedRe.MatchString(m[2]) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// TaskNumber extracts the leading numeric id from a task spec filename
// (e.g. "001" from "001_foo.md"), or the filename itself if it doesn't match.
func TaskNumber(specFile string) string {
	if m := taskFileRe.FindStringSubmatch(specFile); m != nil {
		return m[1]
	}
	return specFile
}

func ReadStatus(path string) ([]Row, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rows []Row
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			continue
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if strings.EqualFold(cells[0], "#") {
			continue // header row
		}
		if strings.Trim(cells[0], "- ") == "" {
			continue // separator row
		}
		status, reason, _ := strings.Cut(cells[2], ":")
		rows = append(rows, Row{Num: cells[0], Spec: cells[1], Status: strings.TrimSpace(status), Reason: strings.TrimSpace(reason)})
	}
	return rows, nil
}

func WriteStatus(path string, rows []Row) error {
	return WriteFileAtomic(path, renderStatus(rows))
}

// renderStatus is the STATUS.md table of rows.
func renderStatus(rows []Row) []byte {
	var b strings.Builder
	b.WriteString("| # | Spec | Status |\n")
	b.WriteString("|---|------|--------|\n")
	for _, r := range rows {
		status := r.Status
		if reason := sanitizeReason(r.Reason); reason != "" {
			status += ": " + reason
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Num, r.Spec, status)
	}
	return []byte(b.String())
}

// ReadQueue returns the items of QUEUE.md, one spec filename per line.
//
// A trailing " [no-tests]" on a line, if someone put it there by hand, is
// tolerated and dropped: QUEUE.md is rebuilt from STATUS.md by Sync, so a
// marker there would not survive anyway. The no-tests marker lives in the
// spec file itself — see NoTestsMarker.
func ReadQueue(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimSuffix(line, queueNoTestsSuffix))
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func WriteQueue(path string, items []string) error {
	return WriteFileAtomic(path, renderQueue(items))
}

// renderQueue is the QUEUE.md of items.
func renderQueue(items []string) []byte {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "- %s\n", it)
	}
	return []byte(b.String())
}

// writeIfChanged writes data to path unless the file already holds exactly
// data.
func writeIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return nil
	}
	return WriteFileAtomic(path, data)
}

// WriteFileAtomic replaces path with data via a temp file + rename, so a
// diadoche process killed mid-write (e.g. by diadoche-restarter on a usage
// limit) never leaves STATUS.md/QUEUE.md truncated — an empty STATUS.md
// would silently forget which tasks are already DONE. The temp file lives
// in path's own directory, so the rename never crosses a filesystem. Also
// used for .claude/diadoche/run-state.json (see package runstate).
func WriteFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Sync discovers new task spec files that aren't in STATUS.md yet, appends
// them as QUEUED rows, and rebuilds QUEUE.md from scratch: every row still
// QUEUED, IN_PROGRESS or PAUSED (new tasks, and any left mid-flight by a
// previous, interrupted run), each after the pending tasks it depends on,
// otherwise by ascending task number — so a restart always resumes with the
// lowest-numbered ready task first, regardless of what order rows happen to
// sit in STATUS.md or the previous QUEUE.md — a spec with a Split-from line
// sorting at its original's position (see pendingQueue). DONE, FAILED, BLOCKED and WAITING rows are not pending and
// never go back to the queue (a WAITING one only by ResumeWaiting).
func Sync(l Layout) (rows []Row, queue []string, err error) {
	if err = l.Ensure(); err != nil {
		return
	}
	rows, err = mergedRows(l)
	if err != nil {
		return
	}
	// A file that wouldn't change is not rewritten: besides sparing the
	// write, that narrows the window in which this read-modify-write could
	// overwrite an agent's concurrent edit of STATUS.md (--parallel).
	if err = writeIfChanged(l.StatusPath, renderStatus(rows)); err != nil {
		return
	}
	queue = pendingQueue(l, rows)
	err = writeIfChanged(l.QueuePath, renderQueue(queue))
	return
}

// PendingQueue returns the queue Sync would build — every pending task in
// dependency order, including task files not in STATUS.md yet —
// without writing anything: no STATUS.md, no QUEUE.md, not even the spec
// directory. Used by --dry-run.
func PendingQueue(l Layout) ([]string, error) {
	rows, err := mergedRows(l)
	if err != nil {
		return nil, err
	}
	return pendingQueue(l, rows), nil
}

// mergedRows reads STATUS.md and appends every discovered task file it
// doesn't list yet as a QUEUED row. It writes nothing; a missing STATUS.md
// or spec directory is no rows/files.
func mergedRows(l Layout) ([]Row, error) {
	rows, err := ReadStatus(l.StatusPath)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, r := range rows {
		known[r.Spec] = true
	}

	files, err := DiscoverTaskFiles(l.SpecDir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if known[f] {
			continue
		}
		rows = append(rows, Row{Num: TaskNumber(f), Spec: f, Status: StatusQueued})
		known[f] = true
	}
	return rows, nil
}

// pendingQueue returns the specs of every QUEUED/IN_PROGRESS/PAUSED row in
// rows (DONE, FAILED, BLOCKED and WAITING rows are not pending), in
// topological order: every task after the pending tasks it depends on.
//
// The base order sorts ascending by numeric task number (rows whose Num
// isn't a plain integer sort after all that are, in their original relative
// order); a spec whose file has a Split-from line sorts right where the spec
// it was split from would, ahead of every higher-numbered task, the parts in
// their own number order; this nests, so a split of a split lands in place
// too. Among the tasks whose dependencies are all placed, the first in base
// order goes next.
//
// Edges: a declared Depends-on number points at every pending task with
// that number (a dependency that isn't pending — DONE, say — imposes
// nothing). A spec without a Depends-on line depends implicitly on the
// pending task right before it in base order, unless that edge would close
// a cycle with the edges added so far (declared ones first, then implicit
// ones in base order): implicit edges yield to declared ones. With no
// Depends-on lines at all the result is exactly the base order. A cycle
// among declared edges (ValidateGraph rejects those) never loses a task: the
// tasks left on it follow in base order.
func pendingQueue(l Layout, rows []Row) []string {
	items, deps := pendingGraph(l, rows)
	n := len(items)
	out := make([]string, 0, n)
	placed := make([]bool, n)
	for len(out) < n {
		next := -1
		for i := range items {
			if placed[i] {
				continue
			}
			ready := true
			for _, d := range deps[i] {
				ready = ready && placed[d]
			}
			if ready {
				next = i
				break
			}
		}
		if next < 0 {
			for i := range items {
				if !placed[i] {
					placed[i] = true
					out = append(out, items[i])
				}
			}
			break
		}
		placed[next] = true
		out = append(out, items[next])
	}
	return out
}

// pendingGraph is the dependency graph of pendingQueue: items are the
// specs of the pending rows in base order, deps[i] the indices of the
// items items[i] must come after (declared and implicit edges, see
// pendingQueue).
func pendingGraph(l Layout, rows []Row) (items []string, deps [][]int) {
	type pending struct {
		key  []int
		spec string
	}
	var ps []pending
	for _, r := range rows {
		if !isPending(r.Status) {
			continue
		}
		ps = append(ps, pending{key: queueKey(l, r.Spec, rowNumber(r.Num), nil), spec: r.Spec})
	}
	sort.SliceStable(ps, func(i, j int) bool { return slices.Compare(ps[i].key, ps[j].key) < 0 })
	for _, p := range ps {
		items = append(items, p.spec)
	}

	n := len(items)
	deps = make([][]int, n) // deps[i]: indices i must come after
	declared := make([]bool, n)
	for i, spec := range items {
		var nums []string
		nums, declared[i] = readDependsOn(l, spec)
		for _, d := range nums {
			for j, other := range items {
				if j != i && sameTaskNumber(TaskNumber(other), d) && !slices.Contains(deps[i], j) {
					deps[i] = append(deps[i], j)
				}
			}
		}
	}
	// reaches reports whether to is reachable from from over deps.
	reaches := func(from, to int) bool {
		seen := make([]bool, n)
		stack := []int{from}
		for len(stack) > 0 {
			v := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if v == to {
				return true
			}
			if seen[v] {
				continue
			}
			seen[v] = true
			stack = append(stack, deps[v]...)
		}
		return false
	}
	for i := 1; i < n; i++ {
		if !declared[i] && !reaches(i-1, i) {
			deps[i] = append(deps[i], i-1)
		}
	}
	return items, deps
}

// PendingPredecessors is task 019's dependency graph as a scheduler sees
// it: for every pending task, the spec files of the pending tasks it must
// come after — the tasks its Depends-on line names, or, without the line,
// the pending task right before it (see pendingQueue). alsoPending are
// tasks that count as pending whatever STATUS.md says: tasks running now,
// e.g. DONE by tester but not merged yet. A task with an empty list may
// start once CheckDependencies finds no blocker and nothing unfinished.
// It writes nothing.
func PendingPredecessors(l Layout, alsoPending []string) (map[string][]string, error) {
	rows, err := mergedRows(l)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if slices.Contains(alsoPending, rows[i].Spec) {
			rows[i].Status = StatusInProgress
		}
	}
	items, deps := pendingGraph(l, rows)
	out := make(map[string][]string, len(items))
	for i, spec := range items {
		preds := []string{}
		for _, j := range deps[i] {
			preds = append(preds, items[j])
		}
		out[spec] = preds
	}
	return out, nil
}

// isPending reports whether status is QUEUED, IN_PROGRESS or PAUSED.
func isPending(status string) bool {
	return status == StatusQueued || status == StatusInProgress || status == StatusPaused
}

// rowNumber is num as an integer, math.MaxInt if it isn't one.
func rowNumber(num string) int {
	n, err := strconv.Atoi(num)
	if err != nil {
		return math.MaxInt
	}
	return n
}

// maxSplitDepth bounds how many Split-from links queueKey follows.
const maxSplitDepth = 32

// queueKey is spec's sort key in the pending queue: [num] for a plain
// spec, its original's key followed by num for one with a Split-from line.
// An unreadable file has no Split-from line; a missing original is keyed
// by its number alone; seen guards against Split-from cycles.
func queueKey(l Layout, spec string, num int, seen map[string]bool) []int {
	if seen[spec] || len(seen) >= maxSplitDepth {
		return []int{num}
	}
	b, err := os.ReadFile(filepath.Join(l.SpecDir, spec))
	if err != nil {
		return []int{num}
	}
	parent := ParseSplitFrom(b)
	if parent == "" || parent == spec {
		return []int{num}
	}
	if seen == nil {
		seen = map[string]bool{}
	}
	seen[spec] = true
	key := queueKey(l, parent, rowNumber(TaskNumber(parent)), seen)
	return append(slices.Clone(key), num)
}

// PopQueue removes and returns the first item of QUEUE.md, persisting the
// remainder. Returns "" if the queue is empty.
func PopQueue(l Layout) (string, error) {
	queue, err := ReadQueue(l.QueuePath)
	if err != nil || len(queue) == 0 {
		return "", err
	}
	next := queue[0]
	rest := queue[1:]
	if err := WriteQueue(l.QueuePath, rest); err != nil {
		return "", err
	}
	return next, nil
}

// SetStatus sets specFile's status, clearing any reason it had.
func SetStatus(l Layout, specFile, status string) error {
	return SetStatusReason(l, specFile, status, "")
}

// SetStatusReason sets specFile's status together with the reason for it
// (see Row.Reason).
func SetStatusReason(l Layout, specFile, status, reason string) error {
	rows, err := ReadStatus(l.StatusPath)
	if err != nil {
		return err
	}
	found := false
	for i := range rows {
		if rows[i].Spec == specFile {
			rows[i].Status = status
			rows[i].Reason = reason
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("spec %q not found in %s", specFile, l.StatusPath)
	}
	return WriteStatus(l.StatusPath, rows)
}

func GetStatus(l Layout, specFile string) (string, error) {
	rows, err := ReadStatus(l.StatusPath)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.Spec == specFile {
			return r.Status, nil
		}
	}
	return "", fmt.Errorf("spec %q not found in %s", specFile, l.StatusPath)
}

// ApplySplit records that original was split into parts (spec files in
// the spec directory, in execution order) by --on-fail split: original
// becomes BLOCKED with reason, one QUEUED row per part is inserted right
// after it in STATUS.md (replacing any row a part already had), and
// QUEUE.md is rebuilt, the parts first in place of original (they carry a
// Split-from line, see pendingQueue). STATUS.md and QUEUE.md end up exactly
// as a following Sync would leave them. The caller holds the spec lock.
func ApplySplit(l Layout, original, reason string, parts []string) (queue []string, err error) {
	rows, err := mergedRows(l)
	if err != nil {
		return nil, err
	}
	isPart := map[string]bool{}
	for _, p := range parts {
		isPart[p] = true
	}
	var out []Row
	found := false
	for _, r := range rows {
		if isPart[r.Spec] {
			continue
		}
		if r.Spec != original {
			out = append(out, r)
			continue
		}
		found = true
		r.Status, r.Reason = StatusBlocked, reason
		out = append(out, r)
		for _, p := range parts {
			out = append(out, Row{Num: TaskNumber(p), Spec: p, Status: StatusQueued})
		}
	}
	if !found {
		return nil, fmt.Errorf("spec %q not found in %s", original, l.StatusPath)
	}
	if err := WriteStatus(l.StatusPath, out); err != nil {
		return nil, err
	}
	queue = pendingQueue(l, out)
	if err := WriteQueue(l.QueuePath, queue); err != nil {
		return nil, err
	}
	return queue, nil
}

// SplitFromPrefix starts the line diadoche appends to a spec that --on-fail
// split cut off another one: "Split-from: 001_foo.md". It places the spec
// at its original's position in the queue (see pendingQueue).
const SplitFromPrefix = "Split-from:"

var splitFromRe = regexp.MustCompile(`(?i)^split-from:\s*(.*)$`)

// ParseSplitFrom returns the spec file named on specContent's first
// Split-from line (the key is case-insensitive, backticks are dropped), or
// "" when there is none.
func ParseSplitFrom(specContent []byte) string {
	for line := range strings.SplitSeq(string(specContent), "\n") {
		if m := splitFromRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return strings.TrimSpace(strings.Trim(strings.TrimSpace(m[1]), "`"))
		}
	}
	return ""
}

// AppendSplitFrom appends a "Split-from: <original>" line to the spec file
// at path, unless it already has a Split-from line.
func AppendSplitFrom(path, original string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if ParseSplitFrom(b) != "" {
		return nil
	}
	var add strings.Builder
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		add.WriteString("\n")
	}
	fmt.Fprintf(&add, "\n%s %s\n", SplitFromPrefix, original)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(add.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// NoTestsMarker is the line that marks a task spec as needing no tests:
// with it, the red-green check neither runs the task's tests on the base
// commit nor fails the iteration for a diff without test files. It lives
// in the spec file because QUEUE.md is rebuilt by Sync and would lose it.
const NoTestsMarker = "<!-- diadoche: no-tests -->"

// queueNoTestsSuffix is the hand-written queue-line marker ReadQueue drops.
const queueNoTestsSuffix = "[no-tests]"

var noTestsRe = regexp.MustCompile(`^<!--\s*diadoche:\s*no-tests\s*-->$`)

// HasNoTestsMarker reports whether specContent has a line that is, once
// trimmed, the NoTestsMarker (extra whitespace inside the comment allowed).
func HasNoTestsMarker(specContent []byte) bool {
	for line := range strings.SplitSeq(string(specContent), "\n") {
		if noTestsRe.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// DependsOnPrefix starts the optional line of a task spec naming the tasks
// it depends on: "Depends-on: 003, 007". It has three states (see
// DependsOn): absent — the task implicitly runs after the task before it in
// queue order, an ordering constraint only; "Depends-on: none" or an empty
// value — no dependencies; a list — exactly those tasks, which must all be
// DONE before it starts. A task whose declared dependency is FAILED or
// BLOCKED is BLOCKED too (see BlockForDependency and BlockDependents).
const DependsOnPrefix = "Depends-on:"

var dependsOnRe = regexp.MustCompile(`(?i)^depends-on:\s*(.*)$`)

// DependsOn returns the task numbers named on specContent's Depends-on
// lines (the key is case-insensitive; several lines add up), in order and
// without duplicates, and whether there is such a line at all. Numbers are
// comma- or space-separated; a spec file name such as 003_foo.md counts as
// its number, and anything else that isn't a plain number ("none"
// included) is ignored. declared false means the line is absent (the
// implicit previous-task dependency, ordering only); declared true with no
// deps means "Depends-on: none" or an empty value.
func DependsOn(specContent []byte) (deps []string, declared bool) {
	for line := range strings.SplitSeq(string(specContent), "\n") {
		m := dependsOnRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		declared = true
		for tok := range strings.FieldsFuncSeq(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			num := TaskNumber(strings.Trim(tok, "`"))
			if num == "" || strings.Trim(num, "0123456789") != "" {
				continue
			}
			dup := false
			for _, seen := range deps {
				dup = dup || sameTaskNumber(seen, num)
			}
			if !dup {
				deps = append(deps, num)
			}
		}
	}
	return deps, declared
}

// ParseDependsOn returns the explicitly declared dependencies of
// specContent, as DependsOn does, without telling an absent line from an
// empty one. nil means no declared dependencies.
func ParseDependsOn(specContent []byte) []string {
	deps, _ := DependsOn(specContent)
	return deps
}

// readDependsOn is DependsOn of spec's file in the spec directory; an
// unreadable file has no line.
func readDependsOn(l Layout, spec string) (deps []string, declared bool) {
	b, err := os.ReadFile(filepath.Join(l.SpecDir, spec))
	if err != nil {
		return nil, false
	}
	return DependsOn(b)
}

// sameTaskNumber compares task numbers numerically when both are integers
// (so "7" and "007" are the same task), else as strings.
func sameTaskNumber(a, b string) bool {
	x, errA := strconv.Atoi(a)
	y, errB := strconv.Atoi(b)
	if errA == nil && errB == nil {
		return x == y
	}
	return a == b
}

// BlockDependents marks BLOCKED every pending (QUEUED, IN_PROGRESS or
// PAUSED) task whose spec declares, via a Depends-on line, a dependency on
// blockedSpec (explicitly declared dependencies only: the implicit
// previous-task dependency of a spec without the line never cascades) — and, transitively, every pending task depending on one of
// those. Each newly blocked row's reason names its direct blocker. DONE,
// FAILED and BLOCKED rows are left alone, as is a row whose spec file can't
// be read; so are WAITING rows. STATUS.md is written once, and only if something changed; the
// newly blocked rows are returned in STATUS.md order.
func BlockDependents(l Layout, blockedSpec string) ([]Row, error) {
	rows, err := ReadStatus(l.StatusPath)
	if err != nil {
		return nil, err
	}
	deps := make([][]string, len(rows))
	for i, r := range rows {
		if !isPending(r.Status) {
			continue
		}
		deps[i], _ = readDependsOn(l, r.Spec)
	}

	type blocker struct{ num, spec string }
	frontier := []blocker{{TaskNumber(blockedSpec), blockedSpec}}
	changed := map[int]bool{}
	for len(frontier) > 0 {
		var next []blocker
		for _, b := range frontier {
			for i := range rows {
				if changed[i] || rows[i].Spec == blockedSpec || len(deps[i]) == 0 {
					continue
				}
				for _, d := range deps[i] {
					if sameTaskNumber(d, b.num) {
						rows[i].Status = StatusBlocked
						rows[i].Reason = fmt.Sprintf("depends on %s, which is BLOCKED", b.spec)
						changed[i] = true
						next = append(next, blocker{TaskNumber(rows[i].Spec), rows[i].Spec})
						break
					}
				}
			}
		}
		frontier = next
	}
	if len(changed) == 0 {
		return nil, nil
	}
	if err := WriteStatus(l.StatusPath, rows); err != nil {
		return nil, err
	}
	var out []Row
	for i, r := range rows {
		if changed[i] {
			out = append(out, r)
		}
	}
	return out, nil
}

// ResumeWaiting returns every WAITING task to the queue (--resume-waiting):
// its row becomes QUEUED with the reason cleared. STATUS.md is written
// once, and only if something changed; the resumed specs are returned in
// STATUS.md order. The caller holds the spec lock.
func ResumeWaiting(l Layout) ([]string, error) {
	rows, err := ReadStatus(l.StatusPath)
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range rows {
		if rows[i].Status == StatusWaiting {
			rows[i].Status, rows[i].Reason = StatusQueued, ""
			out = append(out, rows[i].Spec)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	if err := WriteStatus(l.StatusPath, rows); err != nil {
		return nil, err
	}
	return out, nil
}

// WaitingSpecs lists the WAITING tasks in STATUS.md order, writing nothing.
func WaitingSpecs(l Layout) ([]string, error) {
	rows, err := ReadStatus(l.StatusPath)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if r.Status == StatusWaiting {
			out = append(out, r.Spec)
		}
	}
	return out, nil
}

// WaitingDependency follows spec's Depends-on lines, transitively, and
// returns the first WAITING task it reaches ("" when none): such a task
// can't start before a human answered. An unreadable spec file has no
// dependencies.
func WaitingDependency(l Layout, spec string) (string, error) {
	rows, err := ReadStatus(l.StatusPath)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{spec: true}
	frontier := []string{spec}
	for len(frontier) > 0 {
		var next []string
		for _, s := range frontier {
			deps, _ := readDependsOn(l, s)
			for _, d := range deps {
				for _, r := range rows {
					if seen[r.Spec] || !sameTaskNumber(TaskNumber(r.Spec), d) {
						continue
					}
					if r.Status == StatusWaiting {
						return r.Spec, nil
					}
					seen[r.Spec] = true
					next = append(next, r.Spec)
				}
			}
		}
		frontier = next
	}
	return "", nil
}

// DanglingDep is a Depends-on number of Spec that matches no task spec.
type DanglingDep struct{ Spec, Num string }

// GraphError is what ValidateGraph found wrong with the declared task
// dependency graph: every dangling reference (sorted by spec) and every
// cycle (spec names in cycle order, starting at the smallest, the first not
// repeated at the end; sorted by that first spec).
type GraphError struct {
	Dangling []DanglingDep
	Cycles   [][]string
}

func (e *GraphError) Error() string {
	var lines []string
	for _, d := range e.Dangling {
		lines = append(lines, fmt.Sprintf("%s: Depends-on names %s, which matches no task spec", d.Spec, d.Num))
	}
	for _, c := range e.Cycles {
		lines = append(lines, "dependency cycle: "+strings.Join(append(slices.Clone(c), c[0]), " -> "))
	}
	return "task dependency graph is invalid:\n  " + strings.Join(lines, "\n  ")
}

// ValidateGraph checks the explicitly declared dependencies of every task
// spec — every STATUS.md row and every task file not in it yet, whatever
// its status — and returns a *GraphError listing every Depends-on number
// that matches no task spec and every dependency cycle (a task depending on
// itself is a cycle of one), or nil. A number matching several specs points
// at all of them; an unreadable spec file declares nothing. The implicit
// previous-task dependency is not part of this graph. It writes nothing.
func ValidateGraph(l Layout) error {
	rows, err := mergedRows(l)
	if err != nil {
		return err
	}
	specNames := make([]string, len(rows))
	for i, r := range rows {
		specNames[i] = r.Spec
	}
	slices.Sort(specNames)

	ge := &GraphError{}
	edges := map[string][]string{}
	for _, spec := range specNames {
		deps, _ := readDependsOn(l, spec)
		for _, d := range deps {
			found := false
			for _, other := range specNames {
				if sameTaskNumber(TaskNumber(other), d) {
					found = true
					if !slices.Contains(edges[spec], other) {
						edges[spec] = append(edges[spec], other)
					}
				}
			}
			if !found {
				ge.Dangling = append(ge.Dangling, DanglingDep{Spec: spec, Num: d})
			}
		}
	}

	const (
		white = iota
		grey
		black
	)
	colour := map[string]int{}
	seenCycle := map[string]bool{}
	var stack []string
	var visit func(spec string)
	visit = func(spec string) {
		colour[spec] = grey
		stack = append(stack, spec)
		for _, next := range edges[spec] {
			switch colour[next] {
			case white:
				visit(next)
			case grey:
				start := slices.Index(stack, next)
				cycle := slices.Clone(stack[start:])
				least := 0
				for i, s := range cycle {
					if s < cycle[least] {
						least = i
					}
				}
				cycle = append(cycle[least:], cycle[:least]...)
				if key := strings.Join(cycle, "\x00"); !seenCycle[key] {
					seenCycle[key] = true
					ge.Cycles = append(ge.Cycles, cycle)
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[spec] = black
	}
	for _, spec := range specNames {
		if colour[spec] == white {
			visit(spec)
		}
	}
	slices.SortStableFunc(ge.Cycles, func(a, b []string) int { return strings.Compare(a[0], b[0]) })

	if len(ge.Dangling) == 0 && len(ge.Cycles) == 0 {
		return nil
	}
	return ge
}

// DepCheck is whether a task's explicitly declared dependencies let it
// start (see CheckDependencies). Blocker is the first declared dependency
// that is FAILED or BLOCKED (zero Row if none); Unfinished describes every
// other dependency not DONE yet, e.g. "001 (QUEUED)" or "009 (no such
// task)". A task with neither may start.
type DepCheck struct {
	Blocker    Row
	Unfinished []string
}

// CheckDependencies looks up the status of every dependency spec declares
// on its Depends-on lines (a task file not in STATUS.md yet counts as
// QUEUED). The implicit previous-task dependency of a spec without the line
// is not checked: it only orders the queue, so a FAILED or BLOCKED task
// never holds up tasks that merely follow it.
func CheckDependencies(l Layout, spec string) (DepCheck, error) {
	var chk DepCheck
	rows, err := mergedRows(l)
	if err != nil {
		return chk, err
	}
	deps, _ := readDependsOn(l, spec)
	for _, d := range deps {
		unfinished := ""
		matched := false
		for _, r := range rows {
			if !sameTaskNumber(TaskNumber(r.Spec), d) {
				continue
			}
			matched = true
			switch r.Status {
			case StatusFailed, StatusBlocked:
				if chk.Blocker.Spec == "" {
					chk.Blocker = r
				}
			case StatusDone:
			default:
				if unfinished == "" {
					unfinished = fmt.Sprintf("%s (%s)", d, r.Status)
				}
			}
		}
		if !matched {
			unfinished = d + " (no such task)"
		}
		if unfinished != "" {
			chk.Unfinished = append(chk.Unfinished, unfinished)
		}
	}
	if chk.Blocker.Spec != "" {
		chk.Unfinished = nil
	}
	return chk, nil
}

// BlockForDependency marks spec BLOCKED because its declared dependency dep
// is FAILED or BLOCKED ("depends on <dep>, which is <status>"), then
// cascades along declared dependencies (see BlockDependents). It returns
// spec's own row followed by the rows blocked along with it.
func BlockForDependency(l Layout, spec string, dep Row) ([]Row, error) {
	reason := fmt.Sprintf("depends on %s, which is %s", dep.Spec, dep.Status)
	if err := SetStatusReason(l, spec, StatusBlocked, reason); err != nil {
		return nil, err
	}
	out := []Row{{Num: TaskNumber(spec), Spec: spec, Status: StatusBlocked, Reason: reason}}
	more, err := BlockDependents(l, spec)
	return append(out, more...), err
}
