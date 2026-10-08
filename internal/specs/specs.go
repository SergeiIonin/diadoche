// Package specs manages the diadoche spec directory: STATUS.md (source of
// truth for task status) and QUEUE.md (work queue for the current run), plus
// discovery of new task spec files (spec/NNN_<slug>.md).
//
// A task's status is QUEUED, IN_PROGRESS, PAUSED (waiting out a claude
// usage or rate limit, see Row.Reason for when it resumes), DONE, FAILED or
// BLOCKED (waits for a human: DONE by tester, but its worktree branch
// couldn't be merged; not DONE after the --on-fail chain ended in block; or
// it depends on a BLOCKED task, see ParseDependsOn and BlockDependents).
// Only QUEUED, IN_PROGRESS and PAUSED tasks are pending.
package specs

import (
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
// NNN_FAILED.md and NNN_BLOCKED.md.
var generatedRe = regexp.MustCompile(`(?i)^(IMPL_PLAN|REVIEW_\d+|FAILED|BLOCKED)$`)

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
	return WriteFileAtomic(path, []byte(b.String()))
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
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "- %s\n", it)
	}
	return WriteFileAtomic(path, []byte(b.String()))
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
// previous, interrupted run), ordered by ascending task number — so a restart always
// resumes with the lowest-numbered pending task first, regardless of what
// order rows happen to sit in STATUS.md or the previous QUEUE.md — except
// that a spec with a Split-from line sorts at its original's position (see
// pendingQueue). DONE, FAILED and BLOCKED rows are not pending and never go
// back to the queue.
func Sync(l Layout) (rows []Row, queue []string, err error) {
	if err = l.Ensure(); err != nil {
		return
	}
	rows, err = mergedRows(l)
	if err != nil {
		return
	}
	if err = WriteStatus(l.StatusPath, rows); err != nil {
		return
	}
	queue = pendingQueue(l, rows)
	err = WriteQueue(l.QueuePath, queue)
	return
}

// PendingQueue returns the queue Sync would build — every pending task in
// ascending task number, including task files not in STATUS.md yet —
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

// pendingQueue returns the specs of every QUEUED/IN_PROGRESS/PAUSED row in rows
// (DONE, FAILED and BLOCKED rows are not pending),
// sorted ascending by numeric task number (rows whose Num isn't a plain
// integer sort after all that are, in their original relative order). A
// spec whose file has a Split-from line sorts right where the spec it was
// split from would, ahead of every higher-numbered task, the parts in their
// own number order; this nests, so a split of a split lands in place too.
func pendingQueue(l Layout, rows []Row) []string {
	type pending struct {
		key  []int
		spec string
	}
	var items []pending
	for _, r := range rows {
		if r.Status != StatusQueued && r.Status != StatusInProgress && r.Status != StatusPaused {
			continue
		}
		items = append(items, pending{key: queueKey(l, r.Spec, rowNumber(r.Num), nil), spec: r.Spec})
	}
	sort.SliceStable(items, func(i, j int) bool { return slices.Compare(items[i].key, items[j].key) < 0 })

	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.spec
	}
	return out
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
// it depends on: "Depends-on: 003, 007". A task depending on a BLOCKED task
// is BLOCKED too (see BlockDependents); a spec without the line depends on
// nothing.
const DependsOnPrefix = "Depends-on:"

var dependsOnRe = regexp.MustCompile(`(?i)^depends-on:\s*(.*)$`)

// ParseDependsOn returns the task numbers named on specContent's
// Depends-on lines (the key is case-insensitive; several lines add up), in
// order and without duplicates. Numbers are comma- or space-separated; a
// spec file name such as 003_foo.md counts as its number, and anything else
// that isn't a plain number is ignored. nil means no dependencies.
func ParseDependsOn(specContent []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(specContent), "\n") {
		m := dependsOnRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		for tok := range strings.FieldsFuncSeq(m[1], func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			num := TaskNumber(strings.Trim(tok, "`"))
			if num == "" || strings.Trim(num, "0123456789") != "" {
				continue
			}
			dup := false
			for _, seen := range out {
				dup = dup || sameTaskNumber(seen, num)
			}
			if !dup {
				out = append(out, num)
			}
		}
	}
	return out
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
// blockedSpec — and, transitively, every pending task depending on one of
// those. Each newly blocked row's reason names its direct blocker. DONE,
// FAILED and BLOCKED rows are left alone, as is a row whose spec file can't
// be read. STATUS.md is written once, and only if something changed; the
// newly blocked rows are returned in STATUS.md order.
func BlockDependents(l Layout, blockedSpec string) ([]Row, error) {
	rows, err := ReadStatus(l.StatusPath)
	if err != nil {
		return nil, err
	}
	deps := make([][]string, len(rows))
	for i, r := range rows {
		if r.Status != StatusQueued && r.Status != StatusInProgress && r.Status != StatusPaused {
			continue
		}
		b, err := os.ReadFile(filepath.Join(l.SpecDir, r.Spec))
		if err != nil {
			continue
		}
		deps[i] = ParseDependsOn(b)
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
