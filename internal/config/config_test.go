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
