package orchestrator

// The ADR step (--adr) is a built-in task-done handler (see hooks.go): once
// a task is DONE and merged it turns the decisions worker reported (the
// task's decisions.json) into architecture decision records under
// docs/adr/ in the repository root. No decisions, no step: the agent is not
// called and nothing is written. Otherwise the adr-writer agent writes one
// docs/adr/TMP<k>-<slug>.md per decision, and diadoche does the rest
// itself (package adr): it undoes any edit to an existing ADR other than a
// "Superseded by" status line, assigns the real numbers, rebuilds the index
// docs/adr/README.md and makes sure CLAUDE.md points at it. Like a hook,
// the step never fails the task: a failure rolls docs/adr/ back and is a
// warning fragment in STATUS.md. The step runs on the merge owner
// (finishTask), so with --parallel two tasks' ADR steps never overlap and
// adr.AssignNumbers always sees the previous task's numbered ADRs.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"diadoche/internal/adr"
	"diadoche/internal/decisions"
	"diadoche/internal/gitutil"
	"diadoche/internal/runlog"
	"diadoche/internal/usage"
)

// runADRStep is the ADR handler for specFile, a task that is DONE and
// merged.
func (o *Orchestrator) runADRStep(ctx context.Context, specFile, num string, ws *workspace, uw *usage.Writer) handlerOutcome {
	skip := func(why string) handlerOutcome {
		o.Log.Printf("task %s: adr step: %s; nothing to record", specFile, why)
		o.event(specFile, 0, "", "adr", runlog.F("result", "skipped"), runlog.F("decisions", 0))
		return handlerOutcome{}
	}
	path := o.DecisionsPath(specFile)
	if path == "" {
		return skip("no decisions file")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return skip("no decisions file")
	}
	var ds []decisions.Decision
	if err == nil {
		err = json.Unmarshal(data, &ds)
	}
	if err != nil {
		o.event(specFile, 0, "", "adr", runlog.F("result", "failed"))
		return handlerOutcome{fragment: "adr step failed (decisions unreadable)", err: err}
	}
	if len(ds) == 0 {
		return skip("no decisions")
	}
	if o.adrWriter == nil {
		o.event(specFile, 0, "", "adr", runlog.F("result", "failed"))
		return handlerOutcome{fragment: "adr step failed (adr-writer not loaded)"}
	}

	root, err := filepath.Abs(o.Cfg.Dir)
	if err != nil {
		root = o.Cfg.Dir
	}
	adrDir := filepath.Join(root, filepath.FromSlash(adr.Dir))
	snap, err := adr.Take(adrDir)
	if err != nil {
		o.event(specFile, 0, "", "adr", runlog.F("result", "failed"))
		return handlerOutcome{fragment: "adr step failed (couldn't read docs/adr)", err: err}
	}
	// fail rolls docs/adr/ back to before the call and reports reason.
	fail := func(reason string, err error) handlerOutcome {
		if rerr := adr.Restore(adrDir, snap); rerr != nil {
			o.Log.Printf("warning: task %s: adr step: couldn't roll back %s: %v", specFile, adr.Dir, rerr)
		}
		o.event(specFile, 0, "", "adr", runlog.F("result", "failed"))
		return handlerOutcome{fragment: "adr step failed (" + reason + ")", err: err}
	}

	decisionsJSON, err := decisions.Marshal(ds)
	if err != nil {
		return fail("decisions unreadable", err)
	}
	index := "(no ADRs yet)"
	if b, err := os.ReadFile(filepath.Join(adrDir, adr.IndexFile)); err == nil && strings.TrimSpace(string(b)) != "" {
		index = string(b)
	}
	var existing []string
	if records, err := adr.List(adrDir); err == nil {
		for _, r := range records {
			existing = append(existing, r.File)
		}
	}
	diffNote, diff := o.adrDiff(ctx, ws)
	prompt := adrWriterPrompt(adrPromptInput{
		specFile:  specFile,
		specLink:  o.adrSpecLink(adrDir, specFile),
		date:      o.now().Format("2006-01-02"),
		decisions: string(decisionsJSON),
		diffNote:  diffNote,
		diff:      diff,
		index:     index,
		existing:  existing,
	})

	o.Log.Printf("task %s: adr step: %d decision(s), calling adr-writer", specFile, len(ds))
	o.event(specFile, 0, "", "adr_start", runlog.F("decisions", len(ds)))
	call := agentCall{specFile: specFile, num: num, iter: 0, workDir: root, uw: uw, tally: &iterationTally{}}
	result, err := o.runAgent(ctx, call, o.adrWriter, prompt)
	if err != nil {
		if result.ExitCode != 0 {
			return fail(fmt.Sprintf("adr-writer exit %d", result.ExitCode), err)
		}
		return fail("adr-writer call failed", err)
	}

	dropped, err := adr.Guard(adrDir, snap)
	if err != nil {
		return fail("couldn't check the existing ADRs", err)
	}
	for _, name := range dropped {
		o.Log.Printf("warning: task %s: adr step: dropped adr-writer's edits to %s/%s (only its Status line may change)", specFile, adr.Dir, name)
		o.event(specFile, 0, "", "warning", runlog.F("what", "adr"), runlog.F("error", "existing ADR edited"), runlog.F("file", name))
	}
	assigned, err := adr.AssignNumbers(adrDir)
	if errors.Is(err, adr.ErrNoPlaceholders) {
		return fail("no ADR written", nil)
	}
	if err != nil {
		return fail("couldn't number the ADRs", err)
	}
	if err := adr.WriteIndex(adrDir); err != nil {
		return fail("couldn't write the ADR index", err)
	}
	if _, err := adr.EnsureClaudeLine(root); err != nil {
		o.event(specFile, 0, "", "adr", runlog.F("result", "failed"))
		return handlerOutcome{fragment: "adr step failed (couldn't update CLAUDE.md)", err: err}
	}

	numbers := strings.Join(assigned.Numbers(), ",")
	o.Log.Printf("task %s: adr step: wrote ADR(s) %s, superseded %d", specFile, numbers, len(assigned.Superseded))
	o.event(specFile, 0, "", "adr", runlog.F("result", "ok"), runlog.F("written", len(assigned.Written)),
		runlog.F("superseded", len(assigned.Superseded)), runlog.F("numbers", numbers))
	return handlerOutcome{}
}

// adrSpecLink is the link from docs/adr/ to specFile, slash-separated.
func (o *Orchestrator) adrSpecLink(adrDir, specFile string) string {
	spec, err := filepath.Abs(filepath.Join(o.Layout.SpecDir, specFile))
	if err == nil {
		if rel, err := filepath.Rel(adrDir, spec); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filepath.Join(o.Layout.SpecDir, specFile))
}

// adrDiff is the task's diff for adr-writer: from its base commit to the
// commit it landed as, outside diadoche's own directories. Over
// --max-diff-bytes it is replaced by its --stat; an error leaves a note.
func (o *Orchestrator) adrDiff(ctx context.Context, ws *workspace) (note, diff string) {
	if ws == nil || ws.base == "" {
		return "", "(not available)"
	}
	exclude := ws.exclude
	if len(exclude) == 0 {
		exclude = relExcludeDirs(o.Cfg.Dir, o.Layout.Root)
	}
	var err error
	if ws.mergeCommit != "" {
		diff, err = gitutil.DiffBetweenExcluding(ctx, o.Cfg.Dir, ws.base, ws.mergeCommit, exclude...)
	} else {
		diff, err = gitutil.DiffSinceExcluding(ctx, o.Cfg.Dir, ws.base, exclude...)
	}
	if err != nil {
		o.Log.Printf("warning: adr step: couldn't compute the task's diff: %v", err)
		return "", "(not available: git diff failed)"
	}
	if o.Cfg.MaxDiffBytes > 0 && len(diff) > o.Cfg.MaxDiffBytes && ws.mergeCommit != "" {
		stat, err := gitutil.DiffStatBetweenExcluding(ctx, o.Cfg.Dir, ws.base, ws.mergeCommit, exclude...)
		if err == nil {
			return fmt.Sprintf(" (over %d bytes, so only its --stat; open the files you need yourself, the changes are between %s and %s)",
				o.Cfg.MaxDiffBytes, ws.base, ws.mergeCommit), stat
		}
	}
	return "", diff
}
