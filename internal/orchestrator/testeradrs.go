package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"diadoche/internal/adr"
	"diadoche/internal/decisions"
	"diadoche/internal/implplan"
)

// The relevant-ADRs block of the tester prompt: the ADRs the plan lists
// under "## Relevant ADRs", with their status and full contents, and
// worker's ## Decisions of this iteration, so tester can apply the
// blocking rule for an unrecorded contradiction without looking anything
// up. It is never empty: no listed ADRs, or no section, is said in words.

// noRelevantADRsListed is the block for a plan whose section lists none.
const noRelevantADRsListed = "The plan's \"## Relevant ADRs\" section lists none: there are no relevant ADRs to check this diff against."

// noRelevantADRsSection is the block for a plan without the section.
const noRelevantADRsSection = "The plan has no \"## Relevant ADRs\" section: there are no relevant ADRs to check this diff against."

// dryRunADRs stands in for the relevant-ADRs block in a dry run.
const dryRunADRs = dryRunPlaceholder + " no relevant ADRs: planner is not called in a dry run. " +
	"In a real run this lists the ADR numbers from the plan's \"## Relevant ADRs\" section with the contents " +
	"of those ADR files and worker's ## Decisions, or says there are none."

// adrBlockInput is what relevantADRsBlock is built from.
type adrBlockInput struct {
	specFile     string
	iter         int
	plan         string
	root         string // the task's work dir; ADRs are read from root/docs/adr
	workerOutput string
	workerRan    bool
}

// relevantADRsBlock builds the tester's relevant-ADRs block. A listed ADR
// with no file is named as not found and logged as a warning; it is not an
// error.
func (o *Orchestrator) relevantADRsBlock(in adrBlockInput) string {
	refs, present := implplan.RelevantADRs(in.plan)
	if !present {
		return noRelevantADRsSection
	}
	if len(refs) == 0 {
		return noRelevantADRsListed
	}

	dir := filepath.Join(in.root, filepath.FromSlash(adr.Dir))
	records, err := adr.List(dir)
	if err != nil {
		o.Log.Printf("warning: %s iteration %d: couldn't list %s: %v", in.specFile, in.iter, adr.Dir, err)
		records = nil
	}
	byNumber := map[string]adr.Record{}
	for _, r := range records {
		byNumber[adr.FormatNumber(r.Number)] = r
	}

	numbers := make([]string, len(refs))
	for i, ref := range refs {
		numbers[i] = ref.Number
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ADRs listed in the plan: %s\n", strings.Join(numbers, ", "))
	for _, ref := range refs {
		r, ok := byNumber[ref.Number]
		var content []byte
		if ok {
			content, err = os.ReadFile(filepath.Join(dir, r.File))
			ok = err == nil
		}
		if !ok {
			fmt.Fprintf(&b, "\n--- ADR %s — not found in %s/ ---\n", ref.Number, adr.Dir)
			o.Log.Printf("warning: %s iteration %d: plan lists ADR %s, but no %s/%s-*.md exists", in.specFile, in.iter, ref.Number, adr.Dir, ref.Number)
			continue
		}
		fmt.Fprintf(&b, "\n--- ADR %s: %s (Status: %s) — %s/%s ---\n", ref.Number, r.Title, r.Status, adr.Dir, r.File)
		b.Write(content)
		if !strings.HasSuffix(string(content), "\n") {
			b.WriteString("\n")
		}
	}

	b.WriteString("\nWorker's ## Decisions in this iteration:\n")
	switch ds, found := decisions.Parse(in.workerOutput); {
	case !in.workerRan:
		b.WriteString("(not available: worker was not run in this iteration — resumed after it)")
	case !found:
		b.WriteString("(worker's answer had no ## Decisions section)")
	case len(ds) == 0:
		b.WriteString("none")
	default:
		data, err := decisions.Marshal(ds)
		if err != nil {
			b.WriteString("(worker's ## Decisions could not be serialized)")
			break
		}
		b.WriteString(strings.TrimSuffix(string(data), "\n"))
	}
	return b.String()
}

// warnMissingRelevantADRs logs a warning when plan has no "## Relevant
// ADRs" section. The section is optional: this never rejects the plan,
// and it is not a run.log event, whose sequence stays as it was.
func (o *Orchestrator) warnMissingRelevantADRs(specFile, implPlanFile, plan string, iter int) {
	if _, present := implplan.RelevantADRs(plan); present {
		return
	}
	o.Log.Printf("warning: %s iteration %d: %s has no \"## %s\" section; tester will be told there are no relevant ADRs",
		specFile, iter, implPlanFile, implplan.RelevantADRsHeading)
}
