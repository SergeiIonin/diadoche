// Package config parses entelecheia's CLI flags.
package config

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"
)

const (
	DefaultIter = 3
	MaxIter     = 8
)

// Config holds all runtime settings for a single entelecheia run.
type Config struct {
	Dir        string
	Iter       int
	Langs      []string
	Frameworks []string
	DryRun     bool
	ClaudeBin  string
	Preprocess bool
	Task       string
}

// TaskPath resolves --task relative to --dir, unless it's already absolute.
func (c Config) TaskPath() string {
	if filepath.IsAbs(c.Task) {
		return c.Task
	}
	return filepath.Join(c.Dir, c.Task)
}

// Parse parses CLI args (excluding argv[0]) into a Config.
func Parse(args []string) (*Config, error) {
	fs := flag.NewFlagSet("entelecheia", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project root directory (must contain .claude/entelecheia)")
	iter := fs.Int("ITER", DefaultIter, fmt.Sprintf("max iterations per task (1-%d)", MaxIter))
	langs := fs.String("langs", "", "comma-separated languages, matched against .claude/skills subfolders")
	frameworks := fs.String("frameworks", "", "comma-separated frameworks, matched against .claude/skills subfolders")
	dryRun := fs.Bool("dry-run", false, "log what would be sent to each agent instead of invoking the claude CLI")
	claudeBin := fs.String("claude-bin", "claude", "path to the claude CLI binary")
	preprocess := fs.Bool("preprocess", false, "split a raw task file into atomic task specs and exit, instead of running the main loop")
	task := fs.String("task", "", "path to the raw task file for --preprocess, e.g. .claude/entelecheia/spec/raw/foo.md")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if *iter < 1 || *iter > MaxIter {
		return nil, fmt.Errorf("--ITER must be between 1 and %d, got %d", MaxIter, *iter)
	}
	if *preprocess && strings.TrimSpace(*task) == "" {
		return nil, fmt.Errorf("--preprocess requires --task <path to raw task file>")
	}

	return &Config{
		Dir:        *dir,
		Iter:       *iter,
		Langs:      splitCSV(*langs),
		Frameworks: splitCSV(*frameworks),
		DryRun:     *dryRun,
		ClaudeBin:  *claudeBin,
		Preprocess: *preprocess,
		Task:       *task,
	}, nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
