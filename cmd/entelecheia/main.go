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
)

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "entelecheia:", err)
		os.Exit(2)
	}

	o := orchestrator.New(*cfg)
	if err := o.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "entelecheia:", err)
		os.Exit(1)
	}
}
