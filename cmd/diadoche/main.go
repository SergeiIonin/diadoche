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

	"diadoche/internal/config"
	"diadoche/internal/initializer"
	"diadoche/internal/orchestrator"
	"diadoche/internal/preprocessor"
)

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if errors.Is(err, config.ErrHelp) {
		// Help text was already printed to stdout by config.Parse.
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "diadoche:", err)
		os.Exit(2)
	}

	if cfg.Initialize != "" {
		logger := log.New(os.Stdout, "", log.LstdFlags)
		if err := initializer.Run(cfg.Initialize, logger); err != nil {
			fmt.Fprintln(os.Stderr, "diadoche:", err)
			os.Exit(1)
		}
		return
	}

	if cfg.Preprocess {
		if err := preprocessor.Run(context.Background(), *cfg); err != nil {
			fmt.Fprintln(os.Stderr, "diadoche:", err)
			os.Exit(1)
		}
		return
	}

	o := orchestrator.New(*cfg)
	if err := o.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "diadoche:", err)
		os.Exit(1)
	}
}
