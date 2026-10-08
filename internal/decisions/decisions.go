// Package decisions parses the architectural decisions worker reports at
// the end of its final answer and serializes them to JSON, which later
// steps (a hook, the ADR step) consume.
//
// # Markdown layout
//
// Worker's answer ends with a section headed exactly "## Decisions" (the
// built-in worker prompt documents the same layout):
//
//	## Decisions
//
//	### <short title, one line>
//	Context: <1–3 sentences: what forced the choice>
//	Decision: <what was chosen>
//	Alternatives: <which alternatives were rejected and why>
//
//	### <next title>
//	Context: ...
//	Decision: ...
//	Alternatives: ...
//
// or, when there is nothing to record, the section holds exactly "none".
// The last "## Decisions" line of the answer starts the section; it runs
// to the next level-1 or level-2 heading or to the end of the answer. Each
// "### " heading starts an entry; inside it the labels "Context:",
// "Decision:" and "Alternatives:" each start a field, whose text may
// continue on the following lines up to the next label, the next "### "
// or the end of the section. An empty section counts as "none".
//
// # JSON layout
//
// Marshal writes a JSON array — always an array, "[]" when empty, never
// null — of objects with exactly these four string keys, in this order,
// indented by two spaces and followed by a newline:
//
//	[
//	  {
//	    "title": "...",
//	    "context": "...",
//	    "decision": "...",
//	    "alternatives": "..."
//	  }
//	]
//
// This layout is stable: fields may be added, never renamed or removed.
package decisions

import (
	"encoding/json"
	"strings"
)

// FileName is the decisions artifact's name inside a task's run directory.
const FileName = "decisions.json"

// Heading is the heading of worker's decisions section.
const Heading = "## Decisions"

// Decision is one decision worker reported.
type Decision struct {
	Title        string `json:"title"`
	Context      string `json:"context"`
	Decision     string `json:"decision"`
	Alternatives string `json:"alternatives"`
}

// Parse extracts the decisions section from worker's answer. found is
// false when the answer has no "## Decisions" heading; otherwise ds is
// non-nil, and empty for an empty or "none" section.
func Parse(output string) (ds []Decision, found bool) {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == Heading {
			start = i
		}
	}
	if start < 0 {
		return nil, false
	}
	body := lines[start+1:]
	for i, l := range body {
		if strings.HasPrefix(l, "## ") || strings.HasPrefix(l, "# ") {
			body = body[:i]
			break
		}
	}
	ds = []Decision{}
	if isNone(strings.Join(body, "\n")) {
		return ds, true
	}

	var cur *Decision
	var field *string
	flush := func() {
		if cur == nil {
			return
		}
		cur.Context = strings.TrimSpace(cur.Context)
		cur.Decision = strings.TrimSpace(cur.Decision)
		cur.Alternatives = strings.TrimSpace(cur.Alternatives)
		if cur.Title != "" {
			ds = append(ds, *cur)
		}
	}
	for _, l := range body {
		if title, ok := strings.CutPrefix(l, "### "); ok {
			flush()
			cur, field = &Decision{Title: strings.TrimSpace(title)}, nil
			continue
		}
		if cur == nil {
			continue
		}
		if f, rest, ok := label(cur, l); ok {
			field = f
			*field = rest
			continue
		}
		if field == nil || strings.TrimSpace(l) == "" {
			continue
		}
		if *field == "" {
			*field = l
		} else {
			*field += "\n" + l
		}
	}
	flush()
	return ds, true
}

// isNone reports whether a section body means "no decisions": empty, or
// "none" (any case, optionally with a trailing dot or wrapped in
// backticks or asterisks).
func isNone(body string) bool {
	s := strings.Trim(strings.TrimSpace(body), "`*")
	s = strings.TrimSuffix(s, ".")
	return s == "" || strings.EqualFold(s, "none")
}

// label reports whether line starts one of d's fields and returns that
// field and the text after the label. A leading "- " and a label wrapped
// in "**" are accepted.
func label(d *Decision, line string) (*string, string, bool) {
	s := strings.TrimSpace(line)
	s = strings.TrimPrefix(s, "- ")
	for _, l := range []struct {
		name  string
		field *string
	}{{"Context:", &d.Context}, {"Decision:", &d.Decision}, {"Alternatives:", &d.Alternatives}} {
		for _, p := range []string{l.name, "**" + l.name + "**", "**" + strings.TrimSuffix(l.name, ":") + "**:"} {
			if rest, ok := strings.CutPrefix(s, p); ok {
				return l.field, strings.TrimSpace(rest), true
			}
		}
	}
	return nil, "", false
}

// Marshal serializes ds in the package's stable JSON layout.
func Marshal(ds []Decision) ([]byte, error) {
	if ds == nil {
		ds = []Decision{}
	}
	b, err := json.MarshalIndent(ds, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
