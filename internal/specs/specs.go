// Package specs manages the diadoche spec directory: STATUS.md (source of
// truth for task status) and QUEUE.md (work queue for the current run), plus
// discovery of new task spec files (spec/NNN_<slug>.md).
package specs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	StatusQueued     = "QUEUED"
	StatusInProgress = "IN_PROGRESS"
	StatusDone       = "DONE"
	StatusFailed     = "FAILED"
)

// Row is one line of the STATUS.md table.
type Row struct {
	Num    string
	Spec   string
	Status string
}

// Layout is the set of paths diadoche reads/writes for a given project root.
type Layout struct {
	Root       string // <project>/.claude/diadoche
	SpecDir    string // <root>/spec
	StatusPath string // <root>/spec/STATUS.md
	QueuePath  string // <root>/spec/QUEUE.md
	AgentsDir  string // <project>/.claude/agents
	SkillsDir  string // <project>/.claude/skills
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
// user-authored task specs: NNN_IMPL_PLAN.md and NNN_REVIEW_<M>.md.
var generatedRe = regexp.MustCompile(`(?i)^(IMPL_PLAN|REVIEW_\d+)$`)

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
		rows = append(rows, Row{Num: cells[0], Spec: cells[1], Status: cells[2]})
	}
	return rows, nil
}

func WriteStatus(path string, rows []Row) error {
	var b strings.Builder
	b.WriteString("| # | Spec | Status |\n")
	b.WriteString("|---|------|--------|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Num, r.Spec, r.Status)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

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
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// Sync discovers new task spec files that aren't in STATUS.md yet, appends
// them as QUEUED rows, and (re)builds QUEUE.md: new tasks plus any task left
// QUEUED or IN_PROGRESS by a previous, interrupted run.
func Sync(l Layout) (rows []Row, queue []string, err error) {
	if err = l.Ensure(); err != nil {
		return
	}
	rows, err = ReadStatus(l.StatusPath)
	if err != nil {
		return
	}
	known := map[string]bool{}
	for _, r := range rows {
		known[r.Spec] = true
	}

	files, err := DiscoverTaskFiles(l.SpecDir)
	if err != nil {
		return
	}

	queue, _ = ReadQueue(l.QueuePath)
	queued := map[string]bool{}
	for _, q := range queue {
		queued[q] = true
	}
	for _, r := range rows {
		if (r.Status == StatusQueued || r.Status == StatusInProgress) && !queued[r.Spec] {
			queue = append(queue, r.Spec)
			queued[r.Spec] = true
		}
	}

	for _, f := range files {
		if known[f] {
			continue
		}
		rows = append(rows, Row{Num: TaskNumber(f), Spec: f, Status: StatusQueued})
		known[f] = true
		if !queued[f] {
			queue = append(queue, f)
			queued[f] = true
		}
	}

	if err = WriteStatus(l.StatusPath, rows); err != nil {
		return
	}
	err = WriteQueue(l.QueuePath, queue)
	return
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

func SetStatus(l Layout, specFile, status string) error {
	rows, err := ReadStatus(l.StatusPath)
	if err != nil {
		return err
	}
	found := false
	for i := range rows {
		if rows[i].Spec == specFile {
			rows[i].Status = status
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
