package config

import (
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

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
			for _, want := range []string{"SYNOPSIS", "DESCRIPTION", "MODES", "--initialize", "--preprocess", "--tester-diff-limit", "stable-system-prompt", "docs/prompt-caching.md", "prefix.md", "EXAMPLES"} {
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

func TestParseStableSystemPromptDefaultsOnAndCanBeDisabled(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.StableSystemPrompt {
		t.Error("StableSystemPrompt = false, want true by default")
	}

	cfg, err = Parse([]string{"--stable-system-prompt=false"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StableSystemPrompt {
		t.Error("StableSystemPrompt = true, want false when explicitly disabled")
	}
}

func TestParseWorktreeDefaultsOnAndCanBeDisabled(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Worktree {
		t.Error("Worktree = false, want true by default")
	}

	cfg, err = Parse([]string{"--worktree=false"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Worktree {
		t.Error("Worktree = true, want false when explicitly disabled")
	}
}

func TestParseRedGreenDefaultsOnAndCanBeDisabled(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RedGreen {
		t.Error("RedGreen = false, want true by default")
	}
	if cfg.TestGlobs != nil {
		t.Errorf("TestGlobs = %q, want nil by default", cfg.TestGlobs)
	}

	cfg, err = Parse([]string{"--red-green=false"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedGreen {
		t.Error("RedGreen = true, want false when explicitly disabled")
	}
}

func TestParseTestGlobs(t *testing.T) {
	cfg, err := Parse([]string{"--test-globs", "test_*.py, *.spec.ts"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TestGlobs) != 2 || cfg.TestGlobs[0] != "test_*.py" || cfg.TestGlobs[1] != "*.spec.ts" {
		t.Errorf("TestGlobs = %q, want [test_*.py *.spec.ts]", cfg.TestGlobs)
	}

	_, err = Parse([]string{"--test-globs", "["})
	if err == nil || !strings.Contains(err.Error(), "--test-globs") {
		t.Errorf("bad glob: err = %v, want a --test-globs error", err)
	}
}

func TestParseHelpMentionsRedGreen(t *testing.T) {
	out := captureStdout(t, func() {
		_, _ = Parse([]string{"--help"})
	})
	for _, want := range []string{"--red-green", "--test-globs", "RED-GREEN CHECK", "<!-- diadoche: no-tests -->"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestParseBudgetFlags(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxCostPerTaskUSD != 0 || cfg.MaxCostPerRunUSD != 0 {
		t.Errorf("budgets = %v/%v, want 0/0 (disabled by default)", cfg.MaxCostPerTaskUSD, cfg.MaxCostPerRunUSD)
	}
	cfg, err = Parse([]string{"--max-cost-per-task", "2.5", "--max-cost-per-run", "10"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxCostPerTaskUSD != 2.5 || cfg.MaxCostPerRunUSD != 10 {
		t.Errorf("budgets = %v/%v, want 2.5/10", cfg.MaxCostPerTaskUSD, cfg.MaxCostPerRunUSD)
	}
	for _, flag := range []string{"--max-cost-per-task", "--max-cost-per-run"} {
		if _, err := Parse([]string{flag, "-1"}); err == nil {
			t.Errorf("expected an error for a negative %s", flag)
		}
	}
}

func TestParseHelpMentionsBudgetsAndPauses(t *testing.T) {
	out := captureStdout(t, func() {
		_, _ = Parse([]string{"--help"})
	})
	for _, want := range []string{"--max-cost-per-task", "--max-cost-per-run", "USAGE LIMITS", "PAUSED"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestParseFreshAndShutdownTimeout(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fresh || cfg.ShutdownTimeout != 2*time.Minute {
		t.Errorf("defaults: Fresh=%v ShutdownTimeout=%s, want false/2m", cfg.Fresh, cfg.ShutdownTimeout)
	}
	cfg, err = Parse([]string{"--fresh", "--shutdown-timeout", "5s"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Fresh || cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("Fresh=%v ShutdownTimeout=%s, want true/5s", cfg.Fresh, cfg.ShutdownTimeout)
	}
	if _, err := Parse([]string{"--shutdown-timeout", "-1s"}); err == nil || !strings.Contains(err.Error(), "--shutdown-timeout") {
		t.Errorf("negative --shutdown-timeout: err = %v, want a --shutdown-timeout error", err)
	}
}

func TestParseHelpMentionsRestartState(t *testing.T) {
	out := captureStdout(t, func() {
		_, _ = Parse([]string{"--help"})
	})
	for _, want := range []string{"--fresh", "--shutdown-timeout", "run-state.json", "SIGNALS AND RESTART STATE"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestParseOnTaskDoneRepeatableInOrder(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OnTaskDone) != 0 {
		t.Errorf("OnTaskDone = %q, want none by default", cfg.OnTaskDone)
	}
	cfg, err = Parse([]string{"--on-task-done", "echo a", "--on-task-done", "make docs && git add -A"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"echo a", "make docs && git add -A"}
	if len(cfg.OnTaskDone) != len(want) || cfg.OnTaskDone[0] != want[0] || cfg.OnTaskDone[1] != want[1] {
		t.Errorf("OnTaskDone = %q, want %q", cfg.OnTaskDone, want)
	}
}

func TestParseOnTaskDoneRejectsEmpty(t *testing.T) {
	if _, err := Parse([]string{"--on-task-done", " "}); err == nil {
		t.Error("an empty --on-task-done command should be a startup error")
	}
}

func TestParseHookTimeout(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HookTimeout != DefaultHookTimeout || DefaultHookTimeout != 5*time.Minute {
		t.Errorf("HookTimeout = %s, want the 5m default", cfg.HookTimeout)
	}
	cfg, err = Parse([]string{"--hook-timeout", "30s"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HookTimeout != 30*time.Second {
		t.Errorf("HookTimeout = %s, want 30s", cfg.HookTimeout)
	}
	for _, bad := range []string{"0", "-1s"} {
		if _, err := Parse([]string{"--hook-timeout", bad}); err == nil {
			t.Errorf("--hook-timeout %s should be a startup error", bad)
		}
	}
}

func TestParseHelpMentionsOnTaskDoneHooks(t *testing.T) {
	out := captureStdout(t, func() {
		_, _ = Parse([]string{"--help"})
	})
	for _, want := range []string{"--on-task-done", "--hook-timeout", "ON-TASK-DONE HOOKS", "DIADOCHE_TASK_ID",
		"DIADOCHE_TASK_TITLE", "DIADOCHE_REPO_ROOT", "DIADOCHE_BASE_COMMIT", "DIADOCHE_MERGE_COMMIT",
		"DIADOCHE_DECISIONS_FILE", "on-task-done-<N>.stdout"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestParseOnFailDefaultAndValues(t *testing.T) {
	t.Setenv("DIADOCHE_ESCALATION_MODEL", "")
	os.Unsetenv("DIADOCHE_ESCALATION_MODEL")
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.OnFail, []string{"escalate", "block"}) {
		t.Errorf("default OnFail = %v, want [escalate block]", cfg.OnFail)
	}
	want := agents.ModelOverride{Value: "claude-fable-5-1", Source: agents.ModelSourceDefault}
	if cfg.EscalationModel != want {
		t.Errorf("default EscalationModel = %+v, want %+v", cfg.EscalationModel, want)
	}

	cfg, err = Parse([]string{"--on-fail", ""})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OnFail != nil {
		t.Errorf(`--on-fail "" OnFail = %v, want nil`, cfg.OnFail)
	}
	cfg, err = Parse([]string{"--on-fail", "block"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.OnFail, []string{"block"}) {
		t.Errorf("--on-fail block OnFail = %v", cfg.OnFail)
	}
	cfg, err = Parse([]string{"--on-fail", "escalate,split,block"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.OnFail, []string{"escalate", "split", "block"}) {
		t.Errorf("--on-fail escalate,split,block OnFail = %v", cfg.OnFail)
	}
}

func TestParseOnFailRejectsBadChains(t *testing.T) {
	_, err := Parse([]string{"--on-fail", "bogus"})
	if err == nil {
		t.Fatal("--on-fail bogus should be a startup error")
	}
	for _, want := range []string{"bogus", "escalate", "block"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, bad := range []string{"block,escalate", "escalate,escalate", "escalate,"} {
		if _, err := Parse([]string{"--on-fail", bad}); err == nil {
			t.Errorf("--on-fail %q should be a startup error", bad)
		}
	}
}

func TestParseEscalationModel(t *testing.T) {
	t.Setenv("DIADOCHE_ESCALATION_MODEL", "")
	os.Unsetenv("DIADOCHE_ESCALATION_MODEL")
	cfg, err := Parse([]string{"--escalation-model", "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (agents.ModelOverride{Value: "claude-opus-5-5", Source: agents.ModelSourceFlag}); cfg.EscalationModel != want {
		t.Errorf("EscalationModel = %+v, want %+v", cfg.EscalationModel, want)
	}

	_, err = Parse([]string{"--escalation-model", "nope"})
	if err == nil {
		t.Fatal("--escalation-model nope should be a startup error")
	}
	for _, want := range append([]string{"nope", "--escalation-model"}, agents.KnownModelNames()...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if _, err := Parse([]string{"--escalation-model", ""}); err == nil {
		t.Error("an empty --escalation-model should be a startup error")
	}
}

func TestParseEscalationModelEnv(t *testing.T) {
	t.Setenv("DIADOCHE_ESCALATION_MODEL", "claude-sonnet-5-5")
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (agents.ModelOverride{Value: "claude-sonnet-5-5", Source: agents.ModelSourceEnv}); cfg.EscalationModel != want {
		t.Errorf("env EscalationModel = %+v, want %+v", cfg.EscalationModel, want)
	}

	cfg, err = Parse([]string{"--escalation-model", "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EscalationModel.Value != "claude-opus-5-5" || cfg.EscalationModel.Source != agents.ModelSourceFlag {
		t.Errorf("flag should win over env, got %+v", cfg.EscalationModel)
	}

	t.Setenv("DIADOCHE_ESCALATION_MODEL", "")
	if _, err := Parse(nil); err == nil {
		t.Error("DIADOCHE_ESCALATION_MODEL set but empty should be a startup error")
	}

	t.Setenv("DIADOCHE_ESCALATION_MODEL", "nope")
	_, err = Parse(nil)
	if err == nil || !strings.Contains(err.Error(), "DIADOCHE_ESCALATION_MODEL") || !strings.Contains(err.Error(), "nope") ||
		!strings.Contains(err.Error(), "claude-fable-5-1") {
		t.Errorf("unknown env escalation model: error = %v", err)
	}
}

func TestParseHelpMentionsOnFail(t *testing.T) {
	out := captureStdout(t, func() {
		_, _ = Parse([]string{"--help"})
	})
	for _, want := range []string{"ON-FAIL POLICIES", "--on-fail", "--escalation-model", "DIADOCHE_ESCALATION_MODEL",
		"escalate", "block", "Depends-on:", "NNN_BLOCKED.md", "diadoche/blocked/", "on_fail_policy",
		"split", "Split-from", "diadoche/split/", "no_op", "--on-fail escalate,split,block"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestParseResumeWaiting(t *testing.T) {
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ResumeWaiting {
		t.Error("ResumeWaiting = true by default, want false")
	}
	cfg, err = Parse([]string{"--resume-waiting"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ResumeWaiting {
		t.Error("--resume-waiting not parsed")
	}
}

func TestParseHelpMentionsQuestionsAndWaiting(t *testing.T) {
	out := captureStdout(t, func() {
		_, _ = Parse([]string{"--help"})
	})
	for _, want := range []string{"--resume-waiting", "OPEN QUESTIONS AND WAITING", "QUESTIONS.md", "NEEDS_HUMAN", "WAITING", "NNN_WAITING.md", "diadoche/waiting/"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}

func TestParseMakesDirAbsolute(t *testing.T) {
	cfg, err := Parse([]string{"--dir", "."})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Dir != wd {
		t.Errorf("Dir = %q, want absolute %q", cfg.Dir, wd)
	}
}

// unsetADRModelEnv clears an ambient DIADOCHE_ADR_MODEL for the test (and
// restores it afterwards).
func unsetADRModelEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DIADOCHE_ADR_MODEL", "placeholder")
	if err := os.Unsetenv("DIADOCHE_ADR_MODEL"); err != nil {
		t.Fatal(err)
	}
}

func TestParseADRDefaultsOffAndCanBeEnabled(t *testing.T) {
	unsetADRModelEnv(t)
	cfg, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ADR || cfg.ADRModel.IsSet() {
		t.Fatalf("ADR = %v, ADRModel = %+v; want off and no override by default", cfg.ADR, cfg.ADRModel)
	}
	cfg, err = Parse([]string{"--adr"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ADR {
		t.Fatal("--adr: ADR = false, want true")
	}
}

func TestParseADRModelPrecedence(t *testing.T) {
	unsetADRModelEnv(t)
	cfg, err := Parse([]string{"--adr-model", "claude-haiku-4-5-20251001"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ADRModel != (agents.ModelOverride{Value: "claude-haiku-4-5-20251001", Source: agents.ModelSourceFlag}) {
		t.Fatalf("ADRModel = %+v, want the flag value", cfg.ADRModel)
	}

	t.Setenv("DIADOCHE_ADR_MODEL", "claude-opus-5-5")
	cfg, err = Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ADRModel != (agents.ModelOverride{Value: "claude-opus-5-5", Source: agents.ModelSourceEnv}) {
		t.Fatalf("ADRModel = %+v, want the env value", cfg.ADRModel)
	}

	cfg, err = Parse([]string{"--adr-model", "claude-haiku-4-5-20251001"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ADRModel.Value != "claude-haiku-4-5-20251001" || cfg.ADRModel.Source != agents.ModelSourceFlag {
		t.Fatalf("ADRModel = %+v, want the flag to beat the env", cfg.ADRModel)
	}
}

func TestParseADRModelEmptyIsError(t *testing.T) {
	unsetADRModelEnv(t)
	if _, err := Parse([]string{"--adr-model="}); err == nil || !strings.Contains(err.Error(), "--adr-model must not be empty") {
		t.Errorf("empty --adr-model: err = %v", err)
	}
	t.Setenv("DIADOCHE_ADR_MODEL", " ")
	if _, err := Parse(nil); err == nil || !strings.Contains(err.Error(), "DIADOCHE_ADR_MODEL") {
		t.Errorf("empty DIADOCHE_ADR_MODEL: err = %v", err)
	}
}

func TestParseHelpMentionsADR(t *testing.T) {
	out := captureStdout(t, func() {
		_, _ = Parse([]string{"--help"})
	})
	for _, want := range []string{"--adr", "--adr-model", "ADR STEP", "DIADOCHE_ADR_MODEL", "docs/adr/NNNN-<slug>.md", "docs/adr/README.md", "adr-writer", "iter-0-adr-writer.stdout"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output missing %q", want)
		}
	}
}
