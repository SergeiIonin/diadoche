// Package implplan parses the two required sections of a diadoche
// NNN_IMPL_PLAN.md file: the exact list of files to create or modify, and
// the command to run this project's tests and linter. Requiring these
// sections (validated by HasRequiredSections) lets worker skip re-exploring
// the repository planner already searched, and lets the orchestrator hand
// tester a targeted diff instead of the whole repo.
//
// The "## Files" section lists one file per bullet, the path first
// (optionally back-quoted), followed by a create/modify marker and a
// one-line purpose:
//
//	## Files
//	- internal/foo/foo.go — create: the new validator
//	- `internal/bar/bar.go` — modify: call the validator
//
// Only the path token is parsed; the marker and the purpose are written for
// the humans and agents reading the plan, so the parser stays compatible
// with older plans that list bare paths. Files documents the exact
// extraction rule.
package implplan

import (
	"regexp"
	"strings"
)

// FilesHeading and TestCommandHeading are the exact markdown headings
// planner is instructed (via its system prompt) to use. Keep these in sync
// with internal/agents/defaults/planner.md.
const (
	FilesHeading       = "Files"
	TestCommandHeading = "Test Command"
)

// FilesBulletFormat is the per-file bullet format planner's prompt asks for
// and this package parses the path out of. It is reused verbatim in the
// orchestrator's rejection note so the plan contract is described in one
// place only.
const FilesBulletFormat = "- <path> — create|modify: <one line on what this file is for>"

var headingRe = regexp.MustCompile(`(?m)^##[ \t]+(.+?)[ \t]*$`)

// section holds the raw body text found under one "## <name>" heading.
type section struct {
	name string
	body string
}

// sections splits plan into its "## <name>" sections, in order.
func sections(plan string) []section {
	locs := headingRe.FindAllStringSubmatchIndex(plan, -1)
	if len(locs) == 0 {
		return nil
	}
	out := make([]section, 0, len(locs))
	for i, loc := range locs {
		name := strings.TrimSpace(plan[loc[2]:loc[3]])
		bodyStart := loc[1]
		bodyEnd := len(plan)
		if i+1 < len(locs) {
			bodyEnd = locs[i+1][0]
		}
		out = append(out, section{name: name, body: strings.TrimSpace(plan[bodyStart:bodyEnd])})
	}
	return out
}

func find(plan, heading string) (string, bool) {
	for _, s := range sections(plan) {
		if strings.EqualFold(s.name, heading) {
			return s.body, true
		}
	}
	return "", false
}

// HasRequiredSections reports whether plan contains both required
// sections: a "## Files" section listing at least one file path as a
// bullet, and a non-empty "## Test Command" section. A "## Files" section
// holding only prose ("(none yet)", "TBD") counts as empty — the whole
// point of the section is a machine-readable list of paths.
func HasRequiredSections(plan string) bool {
	if len(Files(plan)) == 0 {
		return false
	}
	cmd, ok := find(plan, TestCommandHeading)
	if !ok || strings.TrimSpace(cmd) == "" {
		return false
	}
	return true
}

// pathFromBullet extracts the file path from one line of the "## Files"
// section, reporting false for any line that is not a file bullet
// (continuation lines, prose, sub-headings, horizontal rules).
//
// The rule, kept deliberately narrow so the planner prompt can state it:
//
//   - the line must start with "-" or "*" followed by whitespace;
//   - if the rest starts with a backtick, the path runs to the next
//     backtick;
//   - otherwise the path ends at the first whitespace or the first em/en
//     dash, whichever comes first;
//   - surrounding "*" (bold) and trailing ":,;)" are stripped.
func pathFromBullet(line string) (string, bool) {
	if len(line) < 2 || (line[0] != '-' && line[0] != '*') {
		return "", false
	}
	// Whitespace after the marker is what separates a bullet from a
	// horizontal rule ("---") or a bold run ("**note**").
	rest := strings.TrimLeft(line[1:], " \t")
	if len(rest) == len(line)-1 {
		return "", false
	}

	var path string
	if strings.HasPrefix(rest, "`") {
		if end := strings.Index(rest[1:], "`"); end >= 0 {
			path = rest[1 : 1+end]
		} else {
			path = rest[1:]
		}
	} else {
		path = rest
		for _, sep := range []string{" ", "\t", "—", "–"} {
			if i := strings.Index(path, sep); i >= 0 {
				path = path[:i]
			}
		}
	}

	path = strings.TrimSpace(path)
	path = strings.Trim(path, "*")
	path = strings.TrimRight(path, ":,;)")
	if path == "" {
		return "", false
	}
	return path, true
}

// Files returns the file paths listed under the "## Files" section, one per
// bullet, in order. Only the path token of each bullet is returned (see
// pathFromBullet); the create/modify marker and the purpose that follow it
// are ignored, so both the annotated format and plain "- path" bullets
// work. Returns nil if the section is missing or lists no paths.
func Files(plan string) []string {
	body, ok := find(plan, FilesHeading)
	if !ok {
		return nil
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if path, ok := pathFromBullet(strings.TrimSpace(line)); ok {
			out = append(out, path)
		}
	}
	return out
}

// TestCommand returns the raw body of the "## Test Command" section
// (trimmed), or "" if the section is missing. This is the plan's
// commands section: the project's test and lint command(s).
func TestCommand(plan string) string {
	body, _ := find(plan, TestCommandHeading)
	return body
}
