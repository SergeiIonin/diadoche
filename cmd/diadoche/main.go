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
	"time"

	"diadoche/internal/config"
	"diadoche/internal/initializer"
	"diadoche/internal/orchestrator"
	"diadoche/internal/preprocessor"
	"diadoche/internal/shutdown"
)

// exitInterrupted is the exit code of a run stopped by SIGINT/SIGTERM
// (128 + SIGINT, the shell convention).
const exitInterrupted = 130

// forceExitGrace is how long Run gets to return once the agent calls were
// cancelled (second signal or --shutdown-timeout) before diadoche exits
// anyway.
const forceExitGrace = 5 * time.Second

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

	// The first SIGINT/SIGTERM asks the loop to stop once the agent call in
	// flight is done (a usage-limit pause ends at once); the call itself is
	// killed only after --shutdown-timeout or on a second signal, which is
	// also what diadoche-restarter's repeated SIGTERM amounts to. Either
	// way the run state is saved and diadoche exits with exitInterrupted.
	// --preprocess has nothing to resume and stops on the first signal.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	logger := log.New(os.Stdout, "", log.LstdFlags)

	if cfg.Preprocess {
		ctx, _, release := shutdown.Watch(context.Background(), sigs, 0, logger.Printf)
		defer release()
		if err := preprocessor.Run(ctx, *cfg); err != nil {
			fmt.Fprintln(os.Stderr, "diadoche:", err)
			return exitCode(err)
		}
		return 0
	}

	ctx, stopRequested, release := shutdown.Watch(context.Background(), sigs, cfg.ShutdownTimeout, logger.Printf)
	defer release()
	returned := make(chan struct{})
	defer close(returned)
	go func() {
		select {
		case <-ctx.Done():
		case <-returned:
			return
		}
		select {
		case <-returned:
		case <-time.After(forceExitGrace):
			fmt.Fprintln(os.Stderr, "diadoche: forced exit after second signal/timeout")
			os.Exit(exitInterrupted)
		}
	}()

	o := orchestrator.New(*cfg)
	o.StopRequested = stopRequested
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
