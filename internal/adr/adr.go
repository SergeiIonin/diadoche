// Package adr is the file side of diadoche's ADR step (--adr): the
// architecture decision records under <project>/docs/adr/, their Go-assigned
// numbers, the generated index and the CLAUDE.md pointer. It knows nothing
// about agents or git; the orchestrator calls the adr-writer agent and uses
// this package before and after the call.
//
// An ADR is docs/adr/NNNN-<slug>.md:
//
//	# NNNN. <Title>
//
//	Status: Accepted            (or: Status: Superseded by NNNN)
//	Date: YYYY-MM-DD
//	Summary: <one line>
//	Task: [<spec file>](<link to the spec>)
//
//	## Context / ## Decision / ## Alternatives considered / ## Consequences
//
// The agent never picks a number. It writes each new ADR as
// docs/adr/TMP<k>-<slug>.md with the title "# TMP<k>. <Title>", and refers
// to it as TMP<k> — also in the one line it may change in an existing ADR,
// "Status: Superseded by TMP<k>". AssignNumbers then gives the TMP<k> files
// the next free numbers in k order, renames them and replaces the tokens.
// The built-in adr-writer prompt describes the same convention.
package adr

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	// Dir is where the ADRs live, relative to the project root.
	Dir = "docs/adr"

	// IndexFile is the generated index inside Dir.
	IndexFile = "README.md"

	// ClaudeFile is the project's CLAUDE.md, relative to the project root.
	ClaudeFile = "CLAUDE.md"

	// ClaudeLine is the one line EnsureClaudeLine adds to CLAUDE.md.
	ClaudeLine = "Project architecture decisions live in docs/adr/README.md. Before planning, read the index and open the ADRs relevant to the task."

	// PlaceholderPrefix starts the temporary number of a new ADR: TMP<k>.
	PlaceholderPrefix = "TMP"
)

// ErrNoPlaceholders is AssignNumbers' error when the agent wrote no
// TMP<k>-<slug>.md file.
var ErrNoPlaceholders = errors.New("no TMP<k>-<slug>.md file in " + Dir)

var (
	numberedRe    = regexp.MustCompile(`^(\d{4})-(.+)\.md$`)
	placeholderRe = regexp.MustCompile(`^TMP(\d+)-(.+)\.md$`)
	tokenRe       = regexp.MustCompile(`\bTMP(\d+)\b`)
	titleNumberRe = regexp.MustCompile(`^(\d{4}|TMP\d+)[.:]\s*`)
	supersededRe  = regexp.MustCompile(`^Status:\s*Superseded by (TMP\d+|\d{4})\s*$`)
	slugBadRe     = regexp.MustCompile(`[^a-z0-9-]+`)
)

// Record is what the index shows of one ADR.
type Record struct {
	Number  int
	File    string // base name, e.g. 0003-use-jsonl.md
	Title   string
	Status  string
	Summary string
}

// FormatNumber is n as an ADR number: four digits, zero-padded.
func FormatNumber(n int) string { return fmt.Sprintf("%04d", n) }

// NextNumber is the number after the highest one in records (1 for none).
func NextNumber(records []Record) int {
	n := 0
	for _, r := range records {
		n = max(n, r.Number)
	}
	return n + 1
}

// List returns the numbered ADRs in dir (NNNN-*.md), sorted by number. A
// missing dir is no ADRs.
func List(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		m := numberedRe.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, parseRecord(n, e.Name(), m[2], string(data)))
	}
	slices.SortStableFunc(out, func(a, b Record) int { return a.Number - b.Number })
	return out, nil
}

// lines splits content into lines without their "\n" or "\r\n".
func lines(content string) []string {
	ls := strings.Split(content, "\n")
	for i, l := range ls {
		ls[i] = strings.TrimSuffix(l, "\r")
	}
	return ls
}

// parseRecord reads an ADR's title, status and summary, with fallbacks:
// the slug for a missing title, Accepted for a missing status, the first
// line of the Decision section for a missing Summary line.
func parseRecord(n int, file, slug, content string) Record {
	r := Record{Number: n, File: file}
	inDecision := false
	decisionLine := ""
	for _, l := range lines(content) {
		t := strings.TrimSpace(l)
		switch {
		case r.Title == "" && strings.HasPrefix(t, "# "):
			r.Title = titleNumberRe.ReplaceAllString(strings.TrimSpace(t[2:]), "")
		case r.Status == "" && strings.HasPrefix(t, "Status:"):
			r.Status = strings.TrimSpace(strings.TrimPrefix(t, "Status:"))
		case r.Summary == "" && strings.HasPrefix(t, "Summary:"):
			r.Summary = strings.TrimSpace(strings.TrimPrefix(t, "Summary:"))
		case strings.HasPrefix(t, "## "):
			inDecision = strings.EqualFold(strings.TrimSpace(t[3:]), "Decision")
		case inDecision && decisionLine == "" && t != "":
			decisionLine = t
		}
	}
	if r.Title == "" {
		r.Title = slug
	}
	if r.Status == "" {
		r.Status = "Accepted"
	}
	if r.Summary == "" {
		r.Summary = decisionLine
	}
	return r
}

// Snapshot is the state of an ADR directory before the agent ran: whether
// it existed, the bytes of each file in it (the ADRs, the index and
// whatever else the project keeps there) and its subdirectories.
type Snapshot struct {
	existed bool
	files   map[string][]byte
	dirs    map[string]bool
}

// Files are the base names in the snapshot, sorted.
func (s Snapshot) Files() []string {
	var names []string
	for name := range s.files {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Take snapshots dir.
func Take(dir string) (Snapshot, error) {
	s := Snapshot{files: map[string][]byte{}, dirs: map[string]bool{}}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.existed = true
	for _, e := range entries {
		if e.IsDir() {
			s.dirs[e.Name()] = true
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return s, err
		}
		s.files[e.Name()] = data
	}
	return s, nil
}

// Restore puts dir back the way snap found it: files that weren't there are
// removed, snapshotted ones rewritten, and a dir that didn't exist is
// removed (with its parent docs/, when that is left empty).
func Restore(dir string, snap Snapshot) error {
	if !snap.existed {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		_ = os.Remove(filepath.Dir(dir)) // only succeeds when empty
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var errs []error
	for _, e := range entries {
		if _, ok := snap.files[e.Name()]; !ok && !snap.dirs[e.Name()] {
			errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for name, data := range snap.files {
		errs = append(errs, os.WriteFile(filepath.Join(dir, name), data, 0o644))
	}
	return errors.Join(errs...)
}

// statusLineIndex is the index of the first "Status:" line of ls, or -1.
func statusLineIndex(ls []string) int {
	for i, l := range ls {
		if strings.HasPrefix(strings.TrimSpace(l), "Status:") {
			return i
		}
	}
	return -1
}

// Guard enforces the one edit the agent may make to an existing ADR: its
// Status line, changed to "Status: Superseded by TMP<k>|NNNN". Every
// snapshotted ADR that was deleted or changed otherwise is restored from
// snap, keeping only such a status change; dropped names the files whose
// other edits were thrown away. Only numbered ADRs are guarded; the index
// is rebuilt anyway.
func Guard(dir string, snap Snapshot) (dropped []string, err error) {
	for _, name := range snap.Files() {
		if !numberedRe.MatchString(name) {
			continue
		}
		orig := snap.files[name]
		path := filepath.Join(dir, name)
		cur, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return dropped, err
		}
		if err == nil && bytes.Equal(cur, orig) {
			continue
		}
		want := orig
		if err == nil {
			curLines, origLines := lines(string(cur)), strings.Split(string(orig), "\n")
			ci, oi := statusLineIndex(curLines), statusLineIndex(origLines)
			if ci >= 0 && oi >= 0 && supersededRe.MatchString(strings.TrimSpace(curLines[ci])) {
				cr := ""
				if strings.HasSuffix(origLines[oi], "\r") {
					cr = "\r"
				}
				origLines[oi] = strings.TrimSpace(curLines[ci]) + cr
				want = []byte(strings.Join(origLines, "\n"))
			}
		}
		if !bytes.Equal(cur, want) {
			dropped = append(dropped, name)
			if err := os.WriteFile(path, want, 0o644); err != nil {
				return dropped, err
			}
		}
	}
	return dropped, nil
}

// Assigned is what AssignNumbers did.
type Assigned struct {
	// Written are the new ADRs under their final names, by number.
	Written []Record
	// Superseded are the existing ADRs whose status now names one of the
	// new numbers.
	Superseded []string
}

// Numbers are the numbers of the written ADRs, e.g. ["0004", "0005"].
func (a Assigned) Numbers() []string {
	var out []string
	for _, r := range a.Written {
		out = append(out, FormatNumber(r.Number))
	}
	return out
}

// placeholder is one TMP<k>-<slug>.md file the agent wrote.
type placeholder struct {
	k    int
	name string
	slug string
}

// slugify keeps a slug to lowercase ascii letters, digits and hyphens.
func slugify(s string) string {
	s = strings.Trim(slugBadRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		return "decision"
	}
	return s
}

// AssignNumbers gives every TMP<k>-<slug>.md in dir, in k order, the next
// free ADR number after the existing ones, renames it to NNNN-<slug>.md and
// replaces the TMP<k> tokens: everywhere in the new files, and only on the
// Status line of existing ones. Existing ADRs keep their numbers.
func AssignNumbers(dir string) (Assigned, error) {
	var res Assigned
	existing, err := List(dir)
	if err != nil {
		return res, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	var phs []placeholder
	for _, e := range entries {
		m := placeholderRe.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			continue
		}
		k, _ := strconv.Atoi(m[1])
		phs = append(phs, placeholder{k: k, name: e.Name(), slug: slugify(m[2])})
	}
	if len(phs) == 0 {
		return res, ErrNoPlaceholders
	}
	slices.SortStableFunc(phs, func(a, b placeholder) int {
		if a.k != b.k {
			return a.k - b.k
		}
		return strings.Compare(a.name, b.name)
	})

	taken := map[int]bool{}
	for _, r := range existing {
		taken[r.Number] = true
	}
	next := NextNumber(existing)
	numbers := map[string]string{} // "TMP<k>" -> "NNNN"
	var newFiles []string
	for _, ph := range phs {
		for taken[next] {
			next++
		}
		num := FormatNumber(next)
		taken[next] = true
		next++
		name := num + "-" + ph.slug + ".md"
		if err := os.Rename(filepath.Join(dir, ph.name), filepath.Join(dir, name)); err != nil {
			return res, err
		}
		token := fmt.Sprintf("%s%d", PlaceholderPrefix, ph.k)
		if _, dup := numbers[token]; !dup {
			numbers[token] = num
		}
		newFiles = append(newFiles, name)
	}
	replace := func(s string) string {
		return tokenRe.ReplaceAllStringFunc(s, func(tok string) string {
			if n, ok := numbers[tok]; ok {
				return n
			}
			return tok
		})
	}

	for _, name := range newFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return res, err
		}
		if fixed := replace(string(data)); fixed != string(data) {
			if err := os.WriteFile(path, []byte(fixed), 0o644); err != nil {
				return res, err
			}
		}
	}

	newNumbers := map[string]bool{}
	for _, n := range numbers {
		newNumbers[n] = true
	}
	for _, r := range existing {
		path := filepath.Join(dir, r.File)
		data, err := os.ReadFile(path)
		if err != nil {
			return res, err
		}
		ls := strings.Split(string(data), "\n")
		i := statusLineIndex(ls)
		if i < 0 {
			continue
		}
		if fixed := replace(ls[i]); fixed != ls[i] {
			ls[i] = fixed
			if err := os.WriteFile(path, []byte(strings.Join(ls, "\n")), 0o644); err != nil {
				return res, err
			}
		}
		if m := supersededRe.FindStringSubmatch(strings.TrimSpace(ls[i])); m != nil && newNumbers[m[1]] {
			res.Superseded = append(res.Superseded, r.File)
		}
	}

	all, err := List(dir)
	if err != nil {
		return res, err
	}
	for _, r := range all {
		if slices.Contains(newFiles, r.File) {
			res.Written = append(res.Written, r)
		}
	}
	return res, nil
}

// escapeCell makes s safe for one markdown table cell.
func escapeCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

// Index is the content of docs/adr/README.md for records.
func Index(records []Record) string {
	var b strings.Builder
	b.WriteString("# Architecture decision records\n\n")
	b.WriteString("Generated by diadoche from the ADR files in this directory; do not edit it by hand.\n\n")
	b.WriteString("| Number | Title | Status | Summary |\n")
	b.WriteString("| --- | --- | --- | --- |\n")
	for _, r := range records {
		num := FormatNumber(r.Number)
		fmt.Fprintf(&b, "| [%s](%s) | %s | %s | %s |\n", num, r.File, escapeCell(r.Title), escapeCell(r.Status), escapeCell(r.Summary))
	}
	return b.String()
}

// WriteIndex rebuilds dir/README.md from the ADRs in dir.
func WriteIndex(dir string) error {
	records, err := List(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, IndexFile), []byte(Index(records)), 0o644)
}

// EnsureClaudeLine adds ClaudeLine to root/CLAUDE.md unless a line of it
// already is ClaudeLine, creating the file when missing. added reports
// whether the file changed.
func EnsureClaudeLine(root string) (added bool, err error) {
	path := filepath.Join(root, ClaudeFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, os.WriteFile(path, []byte(ClaudeLine+"\n"), 0o644)
	}
	if err != nil {
		return false, err
	}
	for _, l := range lines(string(data)) {
		if strings.TrimSpace(l) == ClaudeLine {
			return false, nil
		}
	}
	content := string(data)
	switch {
	case content == "":
	case strings.HasSuffix(content, "\n\n"):
	case strings.HasSuffix(content, "\n"):
		content += "\n"
	default:
		content += "\n\n"
	}
	return true, os.WriteFile(path, []byte(content+ClaudeLine+"\n"), 0o644)
}
