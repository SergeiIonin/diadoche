package adr

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func adrText(number, title, status, summary string) string {
	return "# " + number + ". " + title + "\n\n" +
		"Status: " + status + "\n" +
		"Date: 2026-10-08\n" +
		"Summary: " + summary + "\n" +
		"Task: [001_example.md](../../.claude/diadoche/spec/001_example.md)\n\n" +
		"## Context\n\nWhy " + title + ".\n\n" +
		"## Decision\n\nWe do " + title + ".\n\n" +
		"## Alternatives considered\n\nNone recorded.\n\n" +
		"## Consequences\n\nSome.\n"
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestListParsesFieldsAndFallbacks(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "0002-second.md"), adrText("0002", "Second | piped", "Superseded by 0003", "Two"))
	write(t, filepath.Join(dir, "0001-first.md"), "no heading\n\n## Decision\n\nFirst line of the decision.\nMore.\n")
	write(t, filepath.Join(dir, "TMP1-x.md"), "ignored")
	write(t, filepath.Join(dir, "README.md"), "ignored")

	recs, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{Number: 1, File: "0001-first.md", Title: "first", Status: "Accepted", Summary: "First line of the decision."},
		{Number: 2, File: "0002-second.md", Title: "Second | piped", Status: "Superseded by 0003", Summary: "Two"},
	}
	if !slices.Equal(recs, want) {
		t.Fatalf("List = %+v\nwant %+v", recs, want)
	}
	if NextNumber(recs) != 3 || NextNumber(nil) != 1 {
		t.Errorf("NextNumber = %d / %d, want 3 / 1", NextNumber(recs), NextNumber(nil))
	}
	if recs, err := List(filepath.Join(dir, "missing")); err != nil || recs != nil {
		t.Errorf("missing dir: %v, %v", recs, err)
	}
}

func TestAssignNumbersContinuesExistingAndFixesTitles(t *testing.T) {
	dir := t.TempDir()
	one, two := adrText("0001", "One", "Accepted", "1"), adrText("0002", "Two", "Accepted", "2")
	write(t, filepath.Join(dir, "0001-one.md"), one)
	write(t, filepath.Join(dir, "0002-two.md"), two)
	// TMP10 sorts before TMP2 by name; numbering must follow k.
	write(t, filepath.Join(dir, "TMP10-later.md"), adrText("TMP10", "Later", "Accepted", "later"))
	write(t, filepath.Join(dir, "TMP2-b.md"), adrText("TMP2", "B", "Accepted", "b, see TMP10"))
	write(t, filepath.Join(dir, "TMP1-a.md"), adrText("TMP1", "A", "Accepted", "a"))

	res, err := AssignNumbers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Numbers(), ","); got != "0003,0004,0005" {
		t.Errorf("numbers = %s", got)
	}
	want := []string{"0001-one.md", "0002-two.md", "0003-a.md", "0004-b.md", "0005-later.md"}
	if got := names(t, dir); !slices.Equal(got, want) {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if got := read(t, filepath.Join(dir, "0003-a.md")); !strings.HasPrefix(got, "# 0003. A\n") {
		t.Errorf("0003 title: %q", got)
	}
	b := read(t, filepath.Join(dir, "0004-b.md"))
	if !strings.HasPrefix(b, "# 0004. B\n") || !strings.Contains(b, "Summary: b, see 0005") || strings.Contains(b, "TMP") {
		t.Errorf("0004 = %q", b)
	}
	if read(t, filepath.Join(dir, "0001-one.md")) != one || read(t, filepath.Join(dir, "0002-two.md")) != two {
		t.Error("existing ADRs changed")
	}
	if len(res.Superseded) != 0 {
		t.Errorf("superseded = %v", res.Superseded)
	}
}

func TestAssignNumbersSupersede(t *testing.T) {
	dir := t.TempDir()
	one := adrText("0001", "One", "Accepted", "1")
	write(t, filepath.Join(dir, "0001-one.md"), strings.Replace(one, "Status: Accepted", "Status: Superseded by TMP2", 1))
	write(t, filepath.Join(dir, "TMP1-a.md"), adrText("TMP1", "A", "Accepted", "a"))
	write(t, filepath.Join(dir, "TMP2-b.md"), adrText("TMP2", "B", "Accepted", "Supersedes 0001"))

	res, err := AssignNumbers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "0001-one.md")); got != strings.Replace(one, "Status: Accepted", "Status: Superseded by 0003", 1) {
		t.Errorf("0001 = %q", got)
	}
	if !slices.Equal(res.Superseded, []string{"0001-one.md"}) {
		t.Errorf("superseded = %v", res.Superseded)
	}
	if got := read(t, filepath.Join(dir, "0003-b.md")); !strings.Contains(got, "Supersedes 0001") {
		t.Errorf("0003 lost its reference: %q", got)
	}
}

func TestAssignNumbersOnlyStatusLineOfExisting(t *testing.T) {
	dir := t.TempDir()
	one := adrText("0001", "One", "Accepted", "mentions TMP1 in the summary")
	write(t, filepath.Join(dir, "0001-one.md"), one)
	write(t, filepath.Join(dir, "TMP1-a.md"), adrText("TMP1", "A", "Accepted", "a"))
	if _, err := AssignNumbers(dir); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "0001-one.md")) != one {
		t.Error("a TMP token outside an existing ADR's Status line was replaced")
	}
}

func TestAssignNumbersWithoutPlaceholders(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "0001-one.md"), adrText("0001", "One", "Accepted", "1"))
	if _, err := AssignNumbers(dir); !errors.Is(err, ErrNoPlaceholders) {
		t.Errorf("err = %v, want ErrNoPlaceholders", err)
	}
	if _, err := AssignNumbers(filepath.Join(dir, "missing")); !errors.Is(err, ErrNoPlaceholders) {
		t.Errorf("missing dir: err = %v, want ErrNoPlaceholders", err)
	}
}

func TestGuardKeepsOnlyTheStatusLineChange(t *testing.T) {
	dir := t.TempDir()
	one := adrText("0001", "One", "Accepted", "1")
	two := adrText("0002", "Two", "Accepted", "2")
	three := adrText("0003", "Three", "Accepted", "3")
	write(t, filepath.Join(dir, "0001-one.md"), one)
	write(t, filepath.Join(dir, "0002-two.md"), two)
	write(t, filepath.Join(dir, "0003-three.md"), three)
	snap, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Status changed plus a body edit; body edit only; deleted.
	edited := strings.Replace(strings.Replace(one, "Status: Accepted", "Status: Superseded by TMP1", 1), "Why One.", "Rewritten.", 1)
	write(t, filepath.Join(dir, "0001-one.md"), edited)
	write(t, filepath.Join(dir, "0002-two.md"), strings.Replace(two, "Some.", "Other.", 1))
	if err := os.Remove(filepath.Join(dir, "0003-three.md")); err != nil {
		t.Fatal(err)
	}

	dropped, err := Guard(dir, snap)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dropped, []string{"0001-one.md", "0002-two.md", "0003-three.md"}) {
		t.Errorf("dropped = %v", dropped)
	}
	if got := read(t, filepath.Join(dir, "0001-one.md")); got != strings.Replace(one, "Status: Accepted", "Status: Superseded by TMP1", 1) {
		t.Errorf("0001 = %q", got)
	}
	if read(t, filepath.Join(dir, "0002-two.md")) != two || read(t, filepath.Join(dir, "0003-three.md")) != three {
		t.Error("0002/0003 not restored")
	}

	// A plain status change alone is not "dropped".
	write(t, filepath.Join(dir, "0002-two.md"), strings.Replace(two, "Status: Accepted", "Status: Superseded by 0001", 1))
	if dropped, err := Guard(dir, snap); err != nil || len(dropped) != 0 {
		t.Errorf("dropped = %v, err = %v; want nothing dropped", dropped, err)
	}
	if got := read(t, filepath.Join(dir, "0002-two.md")); !strings.Contains(got, "Status: Superseded by 0001") {
		t.Errorf("0002 lost its status change: %q", got)
	}
}

func TestRestore(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "adr")
	snap, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "TMP1-a.md"), "x")
	if err := Restore(dir, snap); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "docs")); !os.IsNotExist(err) {
		t.Errorf("docs/ left behind: %v", err)
	}

	one := adrText("0001", "One", "Accepted", "1")
	write(t, filepath.Join(dir, "0001-one.md"), one)
	write(t, filepath.Join(dir, "README.md"), "index")
	write(t, filepath.Join(dir, "template.md"), "template")
	snap, err = Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "0001-one.md"), "broken")
	write(t, filepath.Join(dir, "TMP1-a.md"), "x")
	write(t, filepath.Join(dir, "README.md"), "agent index")
	if err := Restore(dir, snap); err != nil {
		t.Fatal(err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"0001-one.md", "README.md", "template.md"}) {
		t.Errorf("files = %v", got)
	}
	if read(t, filepath.Join(dir, "0001-one.md")) != one || read(t, filepath.Join(dir, "README.md")) != "index" {
		t.Error("not restored")
	}
}

func TestWriteIndex(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "0001-one.md"), adrText("0001", "One", "Superseded by 0002", "First | choice"))
	write(t, filepath.Join(dir, "0002-two.md"), adrText("0002", "Two", "Accepted", "Second choice"))
	if err := WriteIndex(dir); err != nil {
		t.Fatal(err)
	}
	want := "# Architecture decision records\n\n" +
		"Generated by diadoche from the ADR files in this directory; do not edit it by hand.\n\n" +
		"| Number | Title | Status | Summary |\n" +
		"| --- | --- | --- | --- |\n" +
		"| [0001](0001-one.md) | One | Superseded by 0002 | First \\| choice |\n" +
		"| [0002](0002-two.md) | Two | Accepted | Second choice |\n"
	if got := read(t, filepath.Join(dir, "README.md")); got != want {
		t.Errorf("index =\n%s\nwant\n%s", got, want)
	}
}

func TestEnsureClaudeLine(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "CLAUDE.md")
	added, err := EnsureClaudeLine(root)
	if err != nil || !added {
		t.Fatalf("missing file: added=%v err=%v", added, err)
	}
	if got := read(t, path); got != ClaudeLine+"\n" {
		t.Errorf("created = %q", got)
	}
	added, err = EnsureClaudeLine(root)
	if err != nil || added || read(t, path) != ClaudeLine+"\n" {
		t.Errorf("second call: added=%v err=%v content=%q", added, err, read(t, path))
	}

	write(t, path, "# Project\nBuild with make.")
	if added, err := EnsureClaudeLine(root); err != nil || !added {
		t.Fatalf("existing file: added=%v err=%v", added, err)
	}
	want := "# Project\nBuild with make.\n\n" + ClaudeLine + "\n"
	if got := read(t, path); got != want {
		t.Errorf("appended = %q, want %q", got, want)
	}
	if added, _ := EnsureClaudeLine(root); added || strings.Count(read(t, path), ClaudeLine) != 1 {
		t.Errorf("line duplicated: %q", read(t, path))
	}
}
