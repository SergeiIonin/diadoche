package config

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"diadoche/internal/agents"
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
			for _, want := range []string{"SYNOPSIS", "DESCRIPTION", "MODES", "--initialize", "--preprocess", "--tester-diff-limit", "EXAMPLES"} {
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
	want := map[string]agents.ModelOverride{
		"planner":      {Value: "claude-sonnet-5", Source: agents.ModelSourceFlag},
		"worker":       {Value: "claude-haiku-4-5-20251001", Source: agents.ModelSourceFlag},
		"tester":       {Value: "claude-fable-5-1", Source: agents.ModelSourceFlag},
		"preprocessor": {Value: "claude-opus-5", Source: agents.ModelSourceFlag},
	}
	got := map[string]agents.ModelOverride{
		"planner":      cfg.PlannerModel,
		"worker":       cfg.WorkerModel,
		"tester":       cfg.TesterModel,
		"preprocessor": cfg.PreprocessorModel,
	}
	for agent, w := range want {
		if got[agent] != w {
			t.Errorf("%s model override = %+v, want %+v", agent, got[agent], w)
		}
	}
}

func TestParseModelEnvVarsApplyWhenFlagAbsent(t *testing.T) {
	t.Setenv("DIADOCHE_PLANNER_MODEL", "claude-sonnet-5")
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlannerModel.Value != "claude-sonnet-5" || cfg.PlannerModel.Source != agents.ModelSourceEnv {
		t.Fatalf("PlannerModel = %+v, want claude-sonnet-5 from env", cfg.PlannerModel)
	}
}

func TestParseModelFlagTakesPriorityOverEnvVar(t *testing.T) {
	t.Setenv("DIADOCHE_PLANNER_MODEL", "claude-sonnet-5")
	cfg, err := Parse([]string{"--planner-model", "claude-opus-5"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlannerModel.Value != "claude-opus-5" || cfg.PlannerModel.Source != agents.ModelSourceFlag {
		t.Fatalf("PlannerModel = %+v, want flag value claude-opus-5 to win over env", cfg.PlannerModel)
	}
}

func TestParseNoModelOverrideLeavesFieldEmpty(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlannerModel.IsSet() || cfg.WorkerModel.IsSet() || cfg.TesterModel.IsSet() || cfg.PreprocessorModel.IsSet() {
		t.Fatalf("expected all model overrides unset by default, got %+v", cfg)
	}
}

func TestParseModelFlagExplicitlyEmptyIsError(t *testing.T) {
	for _, args := range [][]string{
		{"--planner-model="},
		{"--worker-model", ""},
		{"--tester-model", "   "},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("Parse(%q): expected an error for an explicitly empty model flag", args)
		} else if !strings.Contains(err.Error(), "must not be empty") {
			t.Errorf("Parse(%q): error = %v, want it to say the flag must not be empty", args, err)
		}
	}
}

func TestParseModelEnvSetButEmptyIsError(t *testing.T) {
	t.Setenv("DIADOCHE_TESTER_MODEL", "")
	_, err := Parse(nil)
	if err == nil {
		t.Fatal("expected an error for DIADOCHE_TESTER_MODEL set to the empty string")
	}
	if !strings.Contains(err.Error(), "DIADOCHE_TESTER_MODEL") {
		t.Errorf("error = %v, want it to name DIADOCHE_TESTER_MODEL", err)
	}
}

// TestParseModelEnvSetButEmptyIsOverriddenByFlag guards the priority order
// at the point where it matters most: a broken env var must not sink a run
// that explicitly passes the flag.
func TestParseModelEnvSetButEmptyIsOverriddenByFlag(t *testing.T) {
	t.Setenv("DIADOCHE_TESTER_MODEL", "")
	cfg, err := Parse([]string{"--tester-model", "claude-sonnet-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TesterModel.Value != "claude-sonnet-5-5" || cfg.TesterModel.Source != agents.ModelSourceFlag {
		t.Fatalf("TesterModel = %+v, want the flag value", cfg.TesterModel)
	}
}

// TestParseModelEnvUnsetIsNoOverride pins the LookupEnv (not Getenv)
// distinction an empty-is-an-error rule depends on: unset is not "set but
// empty".
func TestParseModelEnvUnsetIsNoOverride(t *testing.T) {
	// t.Setenv first so the ambient value, whatever it is, is restored at
	// cleanup; then unset it for the duration of this test.
	t.Setenv("DIADOCHE_TESTER_MODEL", "placeholder")
	if err := os.Unsetenv("DIADOCHE_TESTER_MODEL"); err != nil {
		t.Fatal(err)
	}

	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TesterModel.IsSet() {
		t.Fatalf("TesterModel = %+v, want no override when the env var is unset", cfg.TesterModel)
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
	// 004_tester_gets_diff.md pins the default at 200 KB, so pin the
	// number itself and not just "whatever the constant says".
	if DefaultMaxDiffBytes != 204800 {
		t.Fatalf("DefaultMaxDiffBytes = %d, want 204800 (200 KB)", DefaultMaxDiffBytes)
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

func TestParseTesterDiffLimitIsSynonymOfMaxDiffBytes(t *testing.T) {
	cfg, err := Parse([]string{"--tester-diff-limit", "1000"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxDiffBytes != 1000 {
		t.Fatalf("MaxDiffBytes = %d, want 1000 (set via --tester-diff-limit)", cfg.MaxDiffBytes)
	}

	cfg, err = Parse([]string{"--tester-diff-limit=0"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxDiffBytes != 0 {
		t.Fatalf("MaxDiffBytes = %d, want 0", cfg.MaxDiffBytes)
	}
}

func TestParseBothDiffLimitFlagsIsError(t *testing.T) {
	// Equal values on purpose: two names for one setting is still
	// ambiguous intent, so it must be rejected rather than resolved.
	_, err := Parse([]string{"--max-diff-bytes", "1", "--tester-diff-limit", "1"})
	if err == nil {
		t.Fatal("expected an error when both --max-diff-bytes and --tester-diff-limit are passed")
	}
	if errors.Is(err, ErrHelp) {
		t.Fatalf("err = %v, want a validation error, not ErrHelp", err)
	}
	for _, want := range []string{"--max-diff-bytes", "--tester-diff-limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestParseTesterDiffLimitRejectsNegative(t *testing.T) {
	_, err := Parse([]string{"--tester-diff-limit", "-1"})
	if err == nil {
		t.Fatal("expected an error for a negative --tester-diff-limit")
	}
	if !strings.Contains(err.Error(), "--tester-diff-limit") {
		t.Errorf("error %q should name the flag the user actually passed", err)
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
