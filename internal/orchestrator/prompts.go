package orchestrator

import "fmt"

// The prompts planner, worker and tester get in each iteration. Both the
// real loop (runIterations) and --dry-run (dryrun.go) build them here, so
// a dry run prints exactly what a real run would send.

// reviewFeedback is the block that hands an agent the previous iteration's
// review; empty when there is none.
func reviewFeedback(priorReview string) string {
	if priorReview == "" {
		return ""
	}
	return fmt.Sprintf("\nFeedback from the previous review, address every point:\n%s\n", priorReview)
}

// plannerPrompt asks planner to write (or update) the plan at implPlanPath.
func plannerPrompt(specFile string, specContent []byte, priorReview, implPlanPath string) string {
	return fmt.Sprintf("Task spec (%s):\n%s\n", specFile, specContent) +
		reviewFeedback(priorReview) +
		fmt.Sprintf("\nWrite (or update) the implementation plan to %s.", implPlanPath)
}

// workerPrompt asks worker to implement plan.
func workerPrompt(specFile string, specContent []byte, implPlanFile, plan, priorReview string) string {
	return fmt.Sprintf("Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n", specFile, specContent, implPlanFile, plan) +
		reviewFeedback(priorReview) +
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
	reviewPath   string
	statusPath   string
}

// testerPrompt asks tester to review the diff, write the review and, if the
// task is done, mark it DONE in STATUS.md.
func testerPrompt(in testerPromptInput) string {
	return fmt.Sprintf(
		"Task spec (%s):\n%s\n\nImplementation plan (%s):\n%s\n\n"+
			"Test/lint command from the plan:\n%s\n\n"+
			"Diff of the worker's changes since this task started%s:\n%s\n\n"+
			"%s\n\n"+
			"Review the diff and the test results against the spec and plan. "+
			"Write your findings to %s. If, and only if, the task is fully and correctly done, "+
			"update the row for %q in %s to status DONE.",
		in.specFile, in.specContent, in.implPlanFile, in.plan, in.testCommand, in.diffNote, in.diff, in.guidance, in.reviewPath, in.specFile, in.statusPath)
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
const dryRunTestCommand = dryRunPlaceholder + ` the command(s) from the plan's "## Test Command" section`

// dryRunDiff stands in for the diff of worker's changes.
const dryRunDiff = dryRunPlaceholder + " no diff: worker is not called and git is not touched in a dry run. " +
	"In a real run this is the diff of the worker's changes since the task's base commit."
