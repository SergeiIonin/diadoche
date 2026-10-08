package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"diadoche/internal/orchestrator"
)

func TestExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"interrupted", fmt.Errorf("interrupted while paused: %w", context.Canceled), exitInterrupted},
		{"run budget", fmt.Errorf("budget exceeded: %w", orchestrator.ErrRunBudgetExceeded), 1},
		{"other", errors.New("boom"), 1},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("%s: exitCode(%v) = %d, want %d", c.name, c.err, got, c.want)
		}
	}
}
