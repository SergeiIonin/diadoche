// Command entelecheia is a loop-engineering CLI: it drives an autonomous
// planner -> worker -> tester agent cycle over task specs found in
// .claude/entelecheia/spec.
package main

import (
	"context"
	"fmt"
	"os"

	"entelecheia/internal/config"
	"entelecheia/internal/orchestrator"
	"entelecheia/internal/preprocessor"
)

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "entelecheia:", err)
		os.Exit(2)
	}

	if cfg.Preprocess {
		if err := preprocessor.Run(context.Background(), *cfg); err != nil {
			fmt.Fprintln(os.Stderr, "entelecheia:", err)
			os.Exit(1)
		}
		return
	}

	o := orchestrator.New(*cfg)
	if err := o.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "entelecheia:", err)
		os.Exit(1)
	}
}
