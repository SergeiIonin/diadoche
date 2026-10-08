// Command diadoche is a loop-engineering CLI: it drives an autonomous
// planner -> worker -> tester agent cycle over task specs found in
// .claude/diadoche/spec.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"diadoche/internal/config"
	"diadoche/internal/initializer"
	"diadoche/internal/orchestrator"
	"diadoche/internal/preprocessor"
)

// exitInterrupted is the exit code of a run stopped by SIGINT/SIGTERM
// (128 + SIGINT, the shell convention).
const exitInterrupted = 130

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Parse(os.Args[1:])
	if errors.Is(err, config.ErrHelp) {
		// Help text was already printed to stdout by config.Parse.
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "diadoche:", err)
		return 2
	}

	if cfg.Initialize != "" {
		logger := log.New(os.Stdout, "", log.LstdFlags)
		if err := initializer.Run(cfg.Initialize, logger); err != nil {
			fmt.Fprintln(os.Stderr, "diadoche:", err)
			return 1
		}
		return 0
	}

	// SIGINT/SIGTERM cancel the context: an in-flight agent call is killed
	// and a usage-limit pause ends at once instead of sleeping it out.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Preprocess {
		if err := preprocessor.Run(ctx, *cfg); err != nil {
			fmt.Fprintln(os.Stderr, "diadoche:", err)
			return exitCode(err)
		}
		return 0
	}

	o := orchestrator.New(*cfg)
	err = o.Run(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "diadoche:", err)
	}
	return exitCode(err)
}

// exitCode maps the error a run ended with to diadoche's exit status: 0 on
// success, exitInterrupted when stopped by a signal, 1 for anything else —
// including a run stopped by --max-cost-per-run.
func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		return exitInterrupted
	default:
		return 1
	}
}
