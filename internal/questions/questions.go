// Package questions owns QUESTIONS.md, the file in the repository root
// where planner and worker put the open questions they can't decide alone,
// and the NEEDS_HUMAN marker an agent ends its answer with when such a
// question blocks the task.
//
// The file is append-only: an agent adds one entry per question at the end
// and never rewrites, reorders or answers existing ones; a human writes the
// answer after "Answer:". Any text before the first entry (a "# Questions"
// title, notes) is ignored. One entry, exactly as the built-in planner and
// worker prompts describe it:
//
//	## Question <NNN>-<k>: <one-line summary>
//	Task: <NNN>_<slug>.md
//	Asked-by: planner|worker
//	Question: <the question; may continue on following lines>
//	Options:
//	- A: <option>
//	- B: <option>
//	Default: <the option the agent picks by default, e.g. A>
//	Why: <why that option is the default>
//	Reversible: yes|no
//	Answer:
//
// <k> numbers a task's questions 1, 2, ... in file order. A label starts a
// field whose text runs on over the following lines up to the next label,
// the next "## " heading or the end of the file; labels are matched
// case-insensitively at the start of a line.
//
// NEEDS_HUMAN triggers only as a line of an agent's final answer that is,
// trimmed of surrounding whitespace, exactly "NEEDS_HUMAN" and not inside a
// fenced code block; an inline mention, `NEEDS_HUMAN`, **NEEDS_HUMAN** or
// "NEEDS_HUMAN." does not (see NeedsHuman).
package questions

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"diadoche/internal/specs"
)

// FileName is the questions file's name in the repository root.
const FileName = "QUESTIONS.md"

// Marker is the line an agent's final answer carries when it can't go on
// without a human's decision.
const Marker = "NEEDS_HUMAN"

// UnansweredLabel is how PromptBlock renders an entry nobody answered yet.
const UnansweredLabel = "Answer: (unanswered — no human answer yet)"

// Entry is one question of QUESTIONS.md; every field is trimmed.
type Entry struct {
	ID, Summary string
	Task        string
	AskedBy     string
	Question    string
	Options     string
	Default     string
	Why         string
	Reversible  string
	Answer      string
}

// placeholders are Answer values that mean "no answer yet".
var placeholders = map[string]bool{"-": true, "tbd": true, "unanswered": true, "(unanswered)": true}

// Answered reports whether a human answered e: Answer is non-empty and not
// a placeholder such as "TBD".
func (e Entry) Answered() bool {
	a := strings.TrimSpace(e.Answer)
	return a != "" && !placeholders[strings.ToLower(a)]
}

var (
	headingRe = regexp.MustCompile(`^##\s+Question\s+(\S+?):\s*(.*)$`)
	labelRe   = regexp.MustCompile(`(?i)^(Task|Asked-by|Question|Options|Default|Why|Reversible|Answer):\s*(.*)$`)
)

// Parse reads the entries of a QUESTIONS.md file, in file order.
func Parse(content []byte) []Entry {
	text := strings.ReplaceAll(string(content), "\r\n", "\n")
	var (
		out   []Entry
		cur   *Entry
		field *string
	)
	flush := func() {
		if cur == nil {
			return
		}
		for _, f := range []*string{&cur.Task, &cur.AskedBy, &cur.Question, &cur.Options, &cur.Default, &cur.Why, &cur.Reversible, &cur.Answer} {
			*f = strings.TrimSpace(*f)
		}
		out = append(out, *cur)
		cur, field = nil, nil
	}
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := headingRe.FindStringSubmatch(trimmed); m != nil {
			flush()
			cur = &Entry{ID: m[1], Summary: strings.TrimSpace(m[2])}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			// Another section ends the entry.
			flush()
			continue
		}
		if m := labelRe.FindStringSubmatch(trimmed); m != nil {
			field = cur.field(m[1])
			*field = m[2]
			continue
		}
		if field != nil {
			*field += "\n" + line
		}
	}
	flush()
	return out
}

// field is the Entry field of a label.
func (e *Entry) field(label string) *string {
	switch strings.ToLower(label) {
	case "task":
		return &e.Task
	case "asked-by":
		return &e.AskedBy
	case "question":
		return &e.Question
	case "options":
		return &e.Options
	case "default":
		return &e.Default
	case "why":
		return &e.Why
	case "reversible":
		return &e.Reversible
	}
	return &e.Answer
}

// Load parses the questions file at path; a missing file has no entries.
func Load(path string) ([]Entry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(b), nil
}

// ForTask keeps the entries about specFile: their Task names the same task
// number (numerically when both are integers, so 18, 018 and 018_x.md all
// match 018_x.md), or, without a Task, their ID starts with it.
func ForTask(entries []Entry, specFile string) []Entry {
	want := specs.TaskNumber(specFile)
	var out []Entry
	for _, e := range entries {
		task := strings.TrimSpace(strings.Trim(e.Task, "`"))
		if task == "" {
			task, _, _ = strings.Cut(e.ID, "-")
		}
		if sameNumber(specs.TaskNumber(task), want) {
			out = append(out, e)
		}
	}
	return out
}

func sameNumber(a, b string) bool {
	x, errA := strconv.Atoi(a)
	y, errB := strconv.Atoi(b)
	if errA == nil && errB == nil {
		return x == y
	}
	return a == b
}

// NeedsHuman reports whether an agent's final answer asks for a human: a
// line that, trimmed, is exactly Marker, outside any fenced code block
// (``` or ~~~).
func NeedsHuman(output string) bool {
	inFence := false
	for line := range strings.SplitSeq(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence && t == Marker {
			return true
		}
	}
	return false
}

// PromptBlock is the part of a planner or worker prompt about open
// questions: where the questions file is (path) and, when entries is not
// empty, this task's questions with their answers, an unanswered one
// labelled UnansweredLabel.
func PromptBlock(path string, entries []Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nOpen questions file: %s (append only; entry layout and rules in your instructions).\n", path)
	if len(entries) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "\nQuestions about this task in %s — plan against the answers, do not ask an answered question again:\n", FileName)
	for _, e := range entries {
		fmt.Fprintf(&b, "\nQuestion %s: %s\n", e.ID, e.Question)
		if e.Options != "" {
			fmt.Fprintf(&b, "Options:\n%s\n", e.Options)
		}
		if e.Default != "" {
			fmt.Fprintf(&b, "Default: %s\n", e.Default)
		}
		if e.Why != "" {
			fmt.Fprintf(&b, "Why: %s\n", e.Why)
		}
		if e.Answered() {
			fmt.Fprintf(&b, "Answer: %s\n", e.Answer)
		} else {
			b.WriteString(UnansweredLabel + "\n")
		}
	}
	return b.String()
}
