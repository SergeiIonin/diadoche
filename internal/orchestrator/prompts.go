package orchestrator

import (
	"fmt"
	"strings"
)

// The prompts planner, worker and tester get in each iteration, and the one
// adr-writer gets after a DONE task (--adr). Both the
// real loop (runIterations) and --dry-run (dryrun.go) build them here, so
// a dry run prints exactly what a real run would send. The tester's
// relevant-ADRs block comes from testeradrs.go (dryRunADRs in a dry run).

// reviewFeedback is the block that hands an agent the previous iteration's
// review; empty when there is none.
func reviewFeedback(priorReview string) string {
	if priorReview == "" {
		return ""
	}
	return fmt.Sprintf("\nFeedback from the previous review, address every point:\n%s\n", priorReview)
}

// plannerPrompt asks planner to write (or update) the plan at implPlanPath.
// questionsBlock is where QUESTIONS.md is and the task's questions and
// answers (see questions.PromptBlock); per-task data, like the rest.
func plannerPrompt(specFile string, specContent []byte, priorReview, implPlanPath, questionsBlock string) string {
	return fmt.Sprintf("Task spec (%s):\n%s\n", specFile, specContent) +
		reviewFeedback(priorReview) +
		questionsBlock +
		fmt.Sprintf("\nWrite (or update) the implementation plan to %s.", implPlanPath)
}

// workerPrompt asks worker to implement plan; questionsBlock as for
// plannerPrompt.
func workerPrompt(specFile string, specContent []byte, implPlanFile, plan, priorReview, questionsBlock string) string {
	return fmt.Sprintf("Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n", specFile, specContent, implPlanFile, plan) +
		reviewFeedback(priorReview) +
		questionsBlock +
		"\nImplement the plan in this repository."
}

// testerPromptInput is everything the tester prompt is built from.
type testerPromptInput struct {
	specFile     string
	specContent  []byte
	implPlanFile string
	plan         string
	testCommand  string
	diffNote     string // e.g. a note that the diff was truncated; may be empty
	diff         string
	guidance     string
	// adrs is the relevant-ADRs block (relevantADRsBlock), or dryRunADRs;
	// never empty.
	adrs       string
	reviewPath string
	statusPath string
}

// testerPrompt asks tester to review the diff, write the review and, if the
// task is done, mark it DONE in STATUS.md.
func testerPrompt(in testerPromptInput) string {
	return fmt.Sprintf(
		"Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n\n"+
			"Test/lint command from the plan:\n%s\n\n"+
			"Relevant ADRs (from the plan's \"## Relevant ADRs\" section; an Accepted one the diff contradicts "+
			"without a matching entry in worker's ## Decisions is a blocking remark):\n%s\n\n"+
			"Diff of the worker's changes since this task started%s:\n%s\n\n"+
			"%s\n\n"+
			"Review the diff and the test results against the spec and plan. "+
			"Write your findings to %s. If, and only if, the task is fully and correctly done, "+
			"update the row for %q in %s to status DONE.",
		in.specFile, in.specContent, in.implPlanFile, in.plan, in.testCommand, in.adrs, in.diffNote, in.diff, in.guidance, in.reviewPath, in.specFile, in.statusPath)
}

// dryRunPlaceholder labels every stand-in --dry-run puts where a real input
// doesn't exist.
const dryRunPlaceholder = "[dry-run placeholder]"

// dryRunPlan stands in for the plan planner would have written to
// implPlanFile.
func dryRunPlan(implPlanFile string) string {
	return fmt.Sprintf("%s no implementation plan: planner is not called in a dry run. "+
		"In a real run this is the content of %s as planner wrote it in this iteration.", dryRunPlaceholder, implPlanFile)
}

// dryRunTestCommand stands in for the plan's test command.
// dryRunSmoke stands in for the smoke block of tester's prompt when the
// project has a smoke.yaml.
const dryRunSmoke = dryRunPlaceholder + " smoke check result: in a real run the outcome of .claude/diadoche/smoke.yaml " +
	"(steps, failures, log tails, console errors, screenshot paths) goes here."

const dryRunTestCommand = dryRunPlaceholder + ` the command(s) from the plan's "## Test Command" section`

// dryRunDiff stands in for the diff of worker's changes.
const dryRunDiff = dryRunPlaceholder + " no diff: worker is not called and git is not touched in a dry run. " +
	"In a real run this is the diff of the worker's changes since the task's base commit."

// adrPromptInput is everything the adr-writer prompt is built from.
type adrPromptInput struct {
	specFile  string
	specLink  string // the spec, relative to docs/adr/
	date      string // YYYY-MM-DD
	decisions string // the decisions as JSON
	diffNote  string // e.g. that the diff was replaced by its --stat; may be empty
	diff      string
	index     string // docs/adr/README.md, or a note that there are no ADRs
	existing  []string
}

// adrWriterPrompt asks adr-writer to write one TMP<k> ADR per decision of
// a DONE task.
func adrWriterPrompt(in adrPromptInput) string {
	existing := "(none)"
	if len(in.existing) > 0 {
		existing = "- docs/adr/" + strings.Join(in.existing, "\n- docs/adr/")
	}
	return fmt.Sprintf(
		"Task spec: %s\nLink to the spec from docs/adr/ (use it as is in the Task: line): %s\nToday's date (use it in the Date: line): %s\n\n"+
			"Decisions of the task, as JSON (decision k is the k-th element, starting at 1):\n%s\n\n"+
			"Diff of the task against its base commit%s:\n%s\n\n"+
			"Current ADR index (docs/adr/README.md):\n%s\n\n"+
			"Existing ADR files:\n%s\n\n"+
			"Write one ADR per decision as docs/adr/TMP<k>-<slug>.md with the title \"# TMP<k>. <Title>\". "+
			"diadoche assigns the real numbers afterwards: never write a real number for a new ADR, and never rename, renumber or delete a file. "+
			"In an existing ADR that a decision reverses, change only its Status line, to \"Status: Superseded by TMP<k>\"; make no other edit to an existing ADR. "+
			"Do not write docs/adr/README.md or CLAUDE.md.",
		in.specFile, in.specLink, in.date, in.decisions, in.diffNote, in.diff, in.index, existing)
}
