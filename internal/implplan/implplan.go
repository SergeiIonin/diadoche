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
//
// A third, optional section, "## Relevant ADRs", lists the architecture
// decision records (docs/adr/) planner found relevant to the task, one per
// bullet, the number first, or "none". It is never a pass/fail criterion:
// HasRequiredSections ignores it, and RelevantADRs parses it.
//
//	## Relevant ADRs
//	- 0003 — Use JSONL for the run log
package implplan

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// FilesHeading and TestCommandHeading are the exact markdown headings
// planner is instructed (via its system prompt) to use. Keep these in sync
// with internal/agents/defaults/planner.md.
const (
	FilesHeading       = "Files"
	TestCommandHeading = "Test Command"

	// RelevantADRsHeading is the optional section listing the ADRs
	// relevant to the task. It is not part of HasRequiredSections. Keep
	// in sync with internal/agents/defaults/planner.md.
	RelevantADRsHeading = "Relevant ADRs"
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

// ADRRef is one ADR listed under "## Relevant ADRs".
type ADRRef struct {
	Number string // four digits, e.g. "0003"
	Title  string // may be empty
}

// adrBulletRe matches the text of a "## Relevant ADRs" bullet: optional
// backticks, "[" and "*", an optional ADR / ADR- prefix, then 1–4 digits
// not followed by another digit.
var adrBulletRe = regexp.MustCompile("^[`\\[*]*(?i:ADR[- ]?)?(\\d{1,4})(?:$|[^\\d])")

// RelevantADRs returns the ADRs listed under "## Relevant ADRs", in order,
// numbers normalised to four digits and de-duplicated. present reports
// whether the section exists at all, so an empty list ("none") is
// distinguishable from a missing section. A line counts only if it is a
// bullet ("-" or "*" plus whitespace) whose text starts with the number
// (optionally back-quoted, linked or prefixed with "ADR"); prose and
// "none" give no entries.
func RelevantADRs(plan string) (refs []ADRRef, present bool) {
	body, ok := find(plan, RelevantADRsHeading)
	if !ok {
		return nil, false
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 2 || (line[0] != '-' && line[0] != '*') {
			continue
		}
		rest := strings.TrimLeft(line[1:], " \t")
		if len(rest) == len(line)-1 {
			continue
		}
		m := adrBulletRe.FindStringSubmatchIndex(rest)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(rest[m[2]:m[3]])
		num := fmt.Sprintf("%04d", n)
		if seen[num] {
			continue
		}
		seen[num] = true
		title := rest[m[3]:]
		if strings.HasPrefix(title, "](") {
			if end := strings.Index(title, ")"); end >= 0 {
				title = title[end+1:]
			}
		}
		title = strings.TrimSpace(strings.TrimLeft(title, "`*]—–-:. \t"))
		title = strings.TrimSpace(strings.TrimRight(title, "`*"))
		refs = append(refs, ADRRef{Number: num, Title: title})
	}
	return refs, true
}

// TestCommand returns the raw body of the "## Test Command" section
// (trimmed), or "" if the section is missing. This is the plan's
// commands section: the project's test and lint command(s).
func TestCommand(plan string) string {
	body, _ := find(plan, TestCommandHeading)
	return body
}

// TestCommandScript turns the "## Test Command" body into a shell script
// that can be run with `sh -c`. When the body has fenced code blocks
// (``` lines, with or without a language tag), the script is the content
// of all of them, in order. Otherwise every non-empty line is taken as a
// command, with a leading "- " or "* " bullet and surrounding single
// backticks stripped; prose is not filtered out — planner is asked to put
// only commands there. Returns "" when nothing remains.
func TestCommandScript(plan string) string {
	body := TestCommand(plan)
	lines := strings.Split(body, "\n")

	var fenced []string
	hasFence, inFence := false, false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			hasFence = true
			inFence = !inFence
			continue
		}
		if inFence && strings.TrimSpace(line) != "" {
			fenced = append(fenced, line)
		}
	}
	if hasFence {
		return strings.Join(fenced, "\n")
	}

	var out []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if l, ok := strings.CutPrefix(line, "- "); ok {
			line = l
		} else if l, ok := strings.CutPrefix(line, "* "); ok {
			line = l
		}
		line = strings.TrimSpace(line)
		if len(line) >= 2 && strings.HasPrefix(line, "`") && strings.HasSuffix(line, "`") {
			line = strings.TrimSpace(line[1 : len(line)-1])
		}
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// testGlobRules maps substrings of a test command (matched
// case-insensitively) to the test-file globs of the ecosystem they belong
// to. Order is the order InferTestGlobs reports globs in.
var testGlobRules = []struct {
	needles []string
	globs   []string
}{
	{[]string{"pytest", "python -m unittest", "tox"}, []string{"test_*.py", "*_test.py"}},
	{
		[]string{"jest", "vitest", "mocha", "npm test", "npm run test", "yarn test", "pnpm test", "bun test"},
		[]string{"*.test.js", "*.test.jsx", "*.test.ts", "*.test.tsx", "*.spec.js", "*.spec.jsx", "*.spec.ts", "*.spec.tsx"},
	},
	{[]string{"cargo test"}, []string{"tests/*.rs", "**/tests/*.rs"}},
	{[]string{"mvn", "gradle", "gradlew"}, []string{"*Test.java", "*Tests.java", "*Test.kt", "*Tests.kt"}},
	{[]string{"rspec"}, []string{"*_spec.rb"}},
	{[]string{"go test"}, []string{"*_test.go"}},
}

// InferTestGlobs guesses test-file globs from a test command by the test
// runners it mentions (pytest, jest, cargo test, ...). It returns the union
// for every runner recognised, deduplicated and in a stable order, or nil
// when none is.
func InferTestGlobs(testCommand string) []string {
	cmd := strings.ToLower(testCommand)
	seen := map[string]bool{}
	var out []string
	for _, r := range testGlobRules {
		for _, n := range r.needles {
			if !strings.Contains(cmd, n) {
				continue
			}
			for _, g := range r.globs {
				if !seen[g] {
					seen[g] = true
					out = append(out, g)
				}
			}
			break
		}
	}
	return out
}
