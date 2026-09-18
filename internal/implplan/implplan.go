// Package implplan parses the two required sections of a diadoche
// NNN_IMPL_PLAN.md file: the exact list of files to create or modify, and
// the command to run this project's tests and linter. Requiring these
// sections (validated by HasRequiredSections) lets worker skip re-exploring
// the repository planner already searched, and lets the orchestrator hand
// tester a targeted diff instead of the whole repo.
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
// sections, each with non-empty content.
func HasRequiredSections(plan string) bool {
	files, ok := find(plan, FilesHeading)
	if !ok || strings.TrimSpace(files) == "" {
		return false
	}
	cmd, ok := find(plan, TestCommandHeading)
	if !ok || strings.TrimSpace(cmd) == "" {
		return false
	}
	return true
}

var bulletRe = regexp.MustCompile(`^[-*]\s*` + "`?" + `(.+?)` + "`?" + `\s*$`)

// Files returns the file paths listed under the "## Files" section, one
// per bullet ("- path" or "* path", optionally backtick-quoted). Returns
// nil if the section is missing or empty.
func Files(plan string) []string {
	body, ok := find(plan, FilesHeading)
	if !ok {
		return nil
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := bulletRe.FindStringSubmatch(line); m != nil {
			if path := strings.TrimSpace(m[1]); path != "" {
				out = append(out, path)
			}
		}
	}
	return out
}

// TestCommand returns the raw body of the "## Test Command" section
// (trimmed), or "" if the section is missing.
func TestCommand(plan string) string {
	body, _ := find(plan, TestCommandHeading)
	return body
}
