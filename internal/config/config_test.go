package config

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestParseInitializeIsStandalone(t *testing.T) {
	cfg, err := Parse([]string{"--initialize", "/tmp/some-project"})
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Initialize != "/tmp/some-project" {
		t.Fatalf("Initialize = %q, want /tmp/some-project", cfg.Initialize)
	}
}

// --initialize scaffolds a project and exits; it must not be tripped up by
// other flags being invalid or absent, since main only ever inspects
// cfg.Initialize before touching anything else.
func TestParseInitializeIgnoresOtherFlagValidation(t *testing.T) {
	cfg, err := Parse([]string{"--initialize", "/tmp/some-project", "--ITER", "999"})
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Initialize != "/tmp/some-project" {
		t.Fatalf("Initialize = %q, want /tmp/some-project", cfg.Initialize)
	}
}

func TestParseWithoutInitializeLeavesItEmpty(t *testing.T) {
	cfg, err := Parse([]string{"--dir", "."})
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Initialize != "" {
		t.Fatalf("Initialize = %q, want empty", cfg.Initialize)
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it, since printHelp writes there directly.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestParseHelpFlagPrintsManPageAndReturnsErrHelp(t *testing.T) {
	for _, arg := range []string{"-h", "-help", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var cfg *Config
			var err error
			out := captureStdout(t, func() {
				cfg, err = Parse([]string{arg})
			})

			if !errors.Is(err, ErrHelp) {
				t.Fatalf("err = %v, want ErrHelp", err)
			}
			if cfg != nil {
				t.Fatalf("cfg = %+v, want nil", cfg)
			}
			for _, want := range []string{"SYNOPSIS", "DESCRIPTION", "MODES", "--initialize", "--preprocess", "EXAMPLES"} {
				if !strings.Contains(out, want) {
					t.Errorf("help output missing %q; got:\n%s", want, out)
				}
			}
		})
	}
}

func TestParseInvalidFlagDoesNotPrintFullManPage(t *testing.T) {
	_, err := Parse([]string{"--not-a-real-flag"})
	if err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
	if errors.Is(err, ErrHelp) {
		t.Fatalf("an unrelated bad flag should not report ErrHelp, got: %v", err)
	}
}

func TestParseModelFlagsSetOverrides(t *testing.T) {
	cfg, err := Parse([]string{
		"--planner-model", "claude-sonnet-5",
		"--worker-model", "claude-haiku-4-5-20251001",
		"--tester-model", "claude-fable-5-1",
		"--preprocessor-model", "claude-opus-5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlannerModel != "claude-sonnet-5" {
		t.Errorf("PlannerModel = %q", cfg.PlannerModel)
	}
	if cfg.WorkerModel != "claude-haiku-4-5-20251001" {
		t.Errorf("WorkerModel = %q", cfg.WorkerModel)
	}
	if cfg.TesterModel != "claude-fable-5-1" {
		t.Errorf("TesterModel = %q", cfg.TesterModel)
	}
	if cfg.PreprocessorModel != "claude-opus-5" {
		t.Errorf("PreprocessorModel = %q", cfg.PreprocessorModel)
	}
}

func TestParseModelEnvVarsApplyWhenFlagAbsent(t *testing.T) {
	t.Setenv("DIADOCHE_PLANNER_MODEL", "claude-sonnet-5")
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlannerModel != "claude-sonnet-5" {
		t.Fatalf("PlannerModel = %q, want claude-sonnet-5 from env", cfg.PlannerModel)
	}
}

func TestParseModelFlagTakesPriorityOverEnvVar(t *testing.T) {
	t.Setenv("DIADOCHE_PLANNER_MODEL", "claude-sonnet-5")
	cfg, err := Parse([]string{"--planner-model", "claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlannerModel != "claude-opus-5" {
		t.Fatalf("PlannerModel = %q, want flag value claude-opus-5 to win over env", cfg.PlannerModel)
	}
}

func TestParseNoModelOverrideLeavesFieldEmpty(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlannerModel != "" || cfg.WorkerModel != "" || cfg.TesterModel != "" || cfg.PreprocessorModel != "" {
		t.Fatalf("expected all model overrides empty by default, got %+v", cfg)
	}
}

func TestParseMaxDiffBytesDefaultAndOverride(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxDiffBytes != DefaultMaxDiffBytes {
		t.Fatalf("MaxDiffBytes = %d, want default %d", cfg.MaxDiffBytes, DefaultMaxDiffBytes)
	}

	cfg, err = Parse([]string{"--max-diff-bytes", "1000"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxDiffBytes != 1000 {
		t.Fatalf("MaxDiffBytes = %d, want 1000", cfg.MaxDiffBytes)
	}
}

func TestParseMaxDiffBytesRejectsNegative(t *testing.T) {
	if _, err := Parse([]string{"--max-diff-bytes", "-1"}); err == nil {
		t.Fatal("expected an error for a negative --max-diff-bytes")
	}
}

func TestParseCallBudgetFlagsDefaultToDisabled(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxCallMinutes != 0 {
		t.Errorf("MaxCallMinutes = %d, want 0 (disabled by default)", cfg.MaxCallMinutes)
	}
	if cfg.MaxCallCostUSD != 0 {
		t.Errorf("MaxCallCostUSD = %v, want 0 (disabled by default)", cfg.MaxCallCostUSD)
	}
}

func TestParseCallBudgetFlagsSetValues(t *testing.T) {
	cfg, err := Parse([]string{"--max-call-minutes", "20", "--max-call-cost", "5.5"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxCallMinutes != 20 {
		t.Errorf("MaxCallMinutes = %d, want 20", cfg.MaxCallMinutes)
	}
	if cfg.MaxCallCostUSD != 5.5 {
		t.Errorf("MaxCallCostUSD = %v, want 5.5", cfg.MaxCallCostUSD)
	}
}

func TestParseCallBudgetFlagsRejectNegative(t *testing.T) {
	if _, err := Parse([]string{"--max-call-minutes", "-1"}); err == nil {
		t.Fatal("expected an error for a negative --max-call-minutes")
	}
	if _, err := Parse([]string{"--max-call-cost", "-1"}); err == nil {
		t.Fatal("expected an error for a negative --max-call-cost")
	}
}
