package implplan

import "testing"

const validPlan = `# Plan

Some intro text.

## Files
- internal/foo/foo.go
- ` + "`internal/foo/foo_test.go`" + `
* internal/bar/bar.go

## Test Command
go test ./... && go vet ./...
`

func TestHasRequiredSectionsAcceptsValidPlan(t *testing.T) {
	if !HasRequiredSections(validPlan) {
		t.Fatal("expected a plan with both sections filled in to be valid")
	}
}

func TestHasRequiredSectionsRejectsMissingFilesSection(t *testing.T) {
	plan := "# Plan\n\n## Test Command\ngo test ./...\n"
	if HasRequiredSections(plan) {
		t.Fatal("expected a plan without a Files section to be invalid")
	}
}

func TestHasRequiredSectionsRejectsMissingTestCommandSection(t *testing.T) {
	plan := "# Plan\n\n## Files\n- a.go\n"
	if HasRequiredSections(plan) {
		t.Fatal("expected a plan without a Test Command section to be invalid")
	}
}

func TestHasRequiredSectionsRejectsEmptySections(t *testing.T) {
	plan := "# Plan\n\n## Files\n\n## Test Command\n\n"
	if HasRequiredSections(plan) {
		t.Fatal("expected a plan with empty required sections to be invalid")
	}
}

func TestHasRequiredSectionsRejectsPlanWithNoHeadings(t *testing.T) {
	if HasRequiredSections("just some free-form text with no sections at all") {
		t.Fatal("expected a plan with no headings to be invalid")
	}
}

func TestFilesParsesBulletList(t *testing.T) {
	got := Files(validPlan)
	want := []string{"internal/foo/foo.go", "internal/foo/foo_test.go", "internal/bar/bar.go"}
	if len(got) != len(want) {
		t.Fatalf("Files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Files[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTestCommandExtractsBody(t *testing.T) {
	got := TestCommand(validPlan)
	want := "go test ./... && go vet ./..."
	if got != want {
		t.Fatalf("TestCommand = %q, want %q", got, want)
	}
}

func TestFilesAndTestCommandEmptyWhenSectionMissing(t *testing.T) {
	plan := "# Plan\nno sections here\n"
	if got := Files(plan); got != nil {
		t.Fatalf("Files = %v, want nil", got)
	}
	if got := TestCommand(plan); got != "" {
		t.Fatalf("TestCommand = %q, want empty", got)
	}
}

// annotatedPlan uses the per-file format the planner prompt requires: the
// path first (back-quoted or not), then a create/modify marker and a
// one-line purpose. Only the path must survive parsing.
const annotatedPlan = "# Plan\n\n## Files\n" +
	"- `internal/agents/models.go` — modify: `ModelSource`, `ModelOverride`, the lot\n" +
	"- internal/foo/foo.go — create: new validator\n" +
	"- internal/x.go (modify) — purpose\n" +
	"- cmd/diadoche/main.go — modify: wire in the validator\n" +
	"  second line of the purpose, wrapped\n" +
	"- **internal/bold.go** — modify: a planner that bolds paths\n" +
	"- `internal/foo/foo_test.go`\n" +
	"- internal/bar.go\n" +
	"\n## Test Command\ngo test ./...\n"

func assertFiles(t *testing.T, plan string, want []string) {
	t.Helper()
	got := Files(plan)
	if len(got) != len(want) {
		t.Fatalf("Files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Files[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestFilesExtractsPathFromAnnotatedBullets(t *testing.T) {
	assertFiles(t, annotatedPlan, []string{
		"internal/agents/models.go",
		"internal/foo/foo.go",
		"internal/x.go",
		"cmd/diadoche/main.go",
		"internal/bold.go",
		"internal/foo/foo_test.go",
		"internal/bar.go",
	})
}

func TestFilesSkipsNonBulletLines(t *testing.T) {
	plan := "## Files\n" +
		"Files to touch, in order:\n" +
		"---\n" +
		"- real/path.go — create: the only entry\n" +
		"### a sub-heading\n" +
		"\n## Test Command\ngo test ./...\n"
	assertFiles(t, plan, []string{"real/path.go"})
}

// The three cases the task names explicitly: no required sections, an
// empty Files section, a correct plan.

func TestHasRequiredSectionsRejectsPlanWithoutRequiredSections(t *testing.T) {
	plan := "# Plan\n\n## Steps\n1. do the thing\n\n## Notes\nnothing else here\n"
	if HasRequiredSections(plan) {
		t.Fatal("expected a plan without the Files and Test Command sections to be invalid")
	}
}

func TestHasRequiredSectionsRejectsEmptyFiles(t *testing.T) {
	cases := map[string]string{
		"blank body":      "# Plan\n\n## Files\n\n## Test Command\ngo test ./...\n",
		"prose only":      "# Plan\n\n## Files\n(none yet)\nTBD — fill in\n\n## Test Command\ngo test ./...\n",
		"empty bullet":    "# Plan\n\n## Files\n-\n- \n\n## Test Command\ngo test ./...\n",
		"only a sub-list": "# Plan\n\n## Files\nSee the steps above.\n\n## Test Command\ngo test ./...\n",
	}
	for name, plan := range cases {
		if HasRequiredSections(plan) {
			t.Errorf("%s: expected a plan with an empty Files section to be invalid", name)
		}
	}
}

func TestHasRequiredSectionsAcceptsAnnotatedPlan(t *testing.T) {
	if !HasRequiredSections(annotatedPlan) {
		t.Fatal("expected a plan using the annotated Files format to be valid")
	}
}

// TestHasRequiredSectionsAcceptsLegacyBarePlan pins backwards compatibility:
// plans written before the create/modify marker was required (tasks 001 and
// 002) list bare paths and must keep validating.
func TestHasRequiredSectionsAcceptsLegacyBarePlan(t *testing.T) {
	if !HasRequiredSections(validPlan) {
		t.Fatal("expected a legacy plan with bare paths to stay valid")
	}
	assertFiles(t, validPlan, []string{"internal/foo/foo.go", "internal/foo/foo_test.go", "internal/bar/bar.go"})
}
