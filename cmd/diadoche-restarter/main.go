// Command diadoche-restarter runs diadoche and restarts it whenever the
// claude account's usage window runs out, at the moment the window reopens.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"diadoche/internal/config"
	"diadoche/internal/restarter"
	"diadoche/internal/specs"
)

const help = `NAME
    diadoche-restarter - run diadoche, restarting it after claude usage limits

SYNOPSIS
    diadoche-restarter [--diadoche-bin <path>] [--reset-buffer <dur>]
                       [--fallback-wait <dur>] [--max-restarts <n>]
                       [-- <diadoche flags>...]
    diadoche-restarter -h | -help | --help

DESCRIPTION
    Runs "diadoche <diadoche flags>" and passes its output through. When
    that output shows the claude CLI's usage-limit error ("You've hit your
    session limit · resets 7pm (Europe/Moscow)", or the legacy "usage limit
    reached|<epoch>"), diadoche-restarter:

      1. stops diadoche (and any claude call it has in flight) right away,
         before it fails the rest of the queue against an exhausted account;
      2. removes every task from spec/STATUS.md except DONE, BLOCKED and
         parked FAILED ones (those with a spec/<NNN>_FAILED.md), so the next
         run re-queues them instead of treating them as FAILED — diadoche
         then resumes each one from the plan/reviews already on disk (and,
         with --worktree, in its worktree);
      3. sleeps until the reset time from the error message (plus
         --reset-buffer), or --fallback-wait if it has none;
      4. starts diadoche again with the same flags.

    Only a usage-limit error triggers a restart. If diadoche exits for any
    other reason, diadoche-restarter exits with its exit status.

    Everything after "--" is passed to diadoche as-is (--dir is read from
    there to find STATUS.md). --preprocess and --initialize are rejected:
    only the main loop is worth restarting.

FLAGS
`

const examples = `
EXAMPLES
    diadoche-restarter -- --dir /path/to/project --ITER 5
    diadoche-restarter --reset-buffer 5m --max-restarts 10 -- --dir /path/to/project
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("diadoche-restarter", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	bin := fs.String("diadoche-bin", defaultBin(), "path to the diadoche binary (the one next to diadoche-restarter if present, else diadoche on $PATH)")
	resetBuffer := fs.Duration("reset-buffer", 2*time.Minute, "extra time to wait after the reported reset time before restarting")
	fallbackWait := fs.Duration("fallback-wait", 30*time.Minute, "how long to wait when the limit message has no parseable reset time")
	maxRestarts := fs.Int("max-restarts", 0, "give up after this many restarts (0 = unlimited)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Print(help)
			fs.SetOutput(os.Stdout)
			fs.PrintDefaults()
			fmt.Print(examples)
			return 0
		}
		fmt.Fprintln(os.Stderr, "diadoche-restarter:", err)
		fmt.Fprintln(os.Stderr, "Run 'diadoche-restarter --help' for usage.")
		return 2
	}
	if *resetBuffer < 0 || *fallbackWait <= 0 || *maxRestarts < 0 {
		fmt.Fprintln(os.Stderr, "diadoche-restarter: --reset-buffer and --max-restarts must be >= 0, --fallback-wait > 0")
		return 2
	}

	diadocheArgs := fs.Args()
	cfg, err := config.Parse(diadocheArgs)
	if errors.Is(err, config.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "diadoche-restarter: invalid diadoche flags:", err)
		return 2
	}
	if cfg.Preprocess || cfg.Initialize != "" {
		fmt.Fprintln(os.Stderr, "diadoche-restarter: --preprocess and --initialize are one-shot modes; run diadoche directly")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	r := &restarter.Restarter{
		Bin:          *bin,
		Args:         diadocheArgs,
		Layout:       specs.NewLayout(cfg.Dir),
		ResetBuffer:  *resetBuffer,
		FallbackWait: *fallbackWait,
		MaxRestarts:  *maxRestarts,
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
		Log:          log.New(os.Stdout, "", log.LstdFlags),
	}
	err = r.Run(ctx)
	var exitErr *restarter.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exitErr):
		return exitErr.Code
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "diadoche-restarter: interrupted")
		return 130
	default:
		fmt.Fprintln(os.Stderr, "diadoche-restarter:", err)
		return 1
	}
}

// defaultBin prefers a diadoche binary sitting next to this one (e.g. both
// built into the repo root by `make build`), falling back to $PATH.
func defaultBin() string {
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "diadoche")
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() {
			return sibling
		}
	}
	return "diadoche"
}
