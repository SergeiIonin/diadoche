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
