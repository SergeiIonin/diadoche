package smoke

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fullExample = `up: docker compose up -d
wait:
  url: http://localhost:8080/healthz
  timeout: 60s
  interval: 1s
checks:
  - cmd: ./scripts/smoke.sh
    timeout: 30s
  - url: http://localhost:8080/api/ping
    status: 200
browser:
  - http://localhost:8080/
  - http://localhost:8080/login
down: docker compose down -v
`

func TestParseFullExample(t *testing.T) {
	cfg, err := Parse([]byte(fullExample))
	if err != nil {
		t.Fatal(err)
	}
	want := &Config{
		Up:   &Command{Cmd: "docker compose up -d", Timeout: DefaultUpTimeout, Line: 1},
		Wait: &Wait{URL: "http://localhost:8080/healthz", Timeout: 60 * time.Second, Interval: time.Second, Line: 2},
		Checks: []Check{
			{Cmd: "./scripts/smoke.sh", Timeout: 30 * time.Second, Line: 7},
			{URL: "http://localhost:8080/api/ping", Status: 200, Timeout: DefaultURLCheckTimeout, Line: 9},
		},
		Browser: []BrowserURL{{"http://localhost:8080/", 12}, {"http://localhost:8080/login", 13}},
		Down:    &Command{Cmd: "docker compose down -v", Timeout: DefaultDownTimeout, Line: 14},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Parse =\n%+v\nwant\n%+v", cfg, want)
	}
}

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte("wait: {cmd: curl -sf localhost}\nchecks:\n  - cmd: ./a\n  - url: http://x/\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Wait.Timeout != 60*time.Second || cfg.Wait.Interval != time.Second || cfg.Wait.Cmd != "curl -sf localhost" {
		t.Errorf("wait = %+v", cfg.Wait)
	}
	if c := cfg.Checks[0]; c.Timeout != 60*time.Second || c.Status != 0 {
		t.Errorf("cmd check = %+v", c)
	}
	if c := cfg.Checks[1]; c.Timeout != 30*time.Second || c.Status != 200 {
		t.Errorf("url check = %+v", c)
	}
	if cfg.Up != nil || cfg.Down != nil || cfg.Browser != nil {
		t.Errorf("unset steps = %+v", cfg)
	}
}

func TestParseCommandMapForm(t *testing.T) {
	cfg, err := Parse([]byte("up: {cmd: make up, timeout: 10m}\ndown:\n  cmd: 'make down'\n  timeout: 30s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if *cfg.Up != (Command{Cmd: "make up", Timeout: 10 * time.Minute, Line: 1}) {
		t.Errorf("up = %+v", cfg.Up)
	}
	if *cfg.Down != (Command{Cmd: "make down", Timeout: 30 * time.Second, Line: 2}) {
		t.Errorf("down = %+v", cfg.Down)
	}
}

func TestParseMinimalAndEmpty(t *testing.T) {
	cfg, err := Parse([]byte("checks:\n- cmd: go test ./...\n"))
	if err != nil || len(cfg.Checks) != 1 {
		t.Fatalf("minimal: %+v, %v", cfg, err)
	}
	for _, src := range []string{"", "# only a comment\n\n   \n# another\n"} {
		_, err := Parse([]byte(src))
		if err == nil || !strings.Contains(err.Error(), "no steps") {
			t.Errorf("Parse(%q) = %v, want a no-steps error", src, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, src string
		line      int
		key, msg  string
	}{
		{"unknown top-level key", "up: a\nservices: x\n", 2, "services", "unknown key"},
		{"unknown wait key", "wait:\n  url: http://x/\n  retries: 3\n", 3, "wait.retries", "unknown key"},
		{"unknown check key", "checks:\n  - cmd: a\n    retries: 3\n", 3, "checks[1].retries", "unknown key"},
		{"unknown up key", "up: {cmd: a, env: x}\n", 1, "up.env", "unknown key"},
		{"tab indentation", "wait:\n  url: http://x/\n\ttimeout: 5s\n", 3, "timeout", "tab"},
		{"wait both", "up: a\nwait:\n  url: http://x/\n  cmd: true\n", 2, "wait", "mutually exclusive"},
		{"wait neither", "wait:\n  timeout: 5s\n", 1, "wait", "one of url or cmd"},
		{"bad duration", "wait:\n  url: http://x/\n  timeout: 6x\n", 3, "wait.timeout", "invalid duration"},
		{"zero duration", "checks:\n  - cmd: a\n    timeout: 0s\n", 3, "checks[1].timeout", "positive"},
		{"non-numeric status", "checks:\n  - url: http://x/\n    status: abc\n", 3, "checks[1].status", "non-numeric status"},
		{"status on cmd", "checks:\n  - cmd: a\n    status: 200\n", 3, "checks[1].status", "only to a url"},
		{"browser ftp", "browser:\n  - ftp://x/\n", 2, "browser[1]", "not an http(s) URL"},
		{"browser no scheme", "browser:\n  - http://x/\n  - localhost:8080\n", 3, "browser[2]", "not an http(s) URL"},
		{"browser words", "browser:\n  - not a url\n", 2, "browser[1]", "not an http(s) URL"},
		{"browser map item", "browser:\n  - url: http://x/\n", 2, "browser[1]", "a map where a URL string is expected"},
		{"list where scalar expected", "up:\n  - docker compose up\n", 1, "up", "got a list"},
		{"list in nested map", "wait:\n  url:\n  - http://x/\n", 3, "wait.url", "nested too deep"},
		{"map where scalar expected", "checks:\n  - cmd: {a: b}\n", 2, "checks[1].cmd", "expected a scalar, got a map"},
		{"empty check item", "checks:\n  - {}\n", 2, "checks[1]", "empty item"},
		{"check both", "checks:\n  - {cmd: a, url: http://x/}\n", 2, "checks[1]", "mutually exclusive"},
		{"empty checks", "checks:\n", 1, "checks", "expected a list"},
		{"wait scalar", "wait: http://x/\n", 1, "wait", "expected a map"},
		{"up map without cmd", "up: {timeout: 5s}\n", 1, "up.cmd", "missing command"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if e.Line != c.line || e.Key != c.key || !strings.Contains(e.Msg, c.msg) {
				t.Errorf("err = %+v, want line %d key %q msg ~%q", e, c.line, c.key, c.msg)
			}
			if want := fmt.Sprintf(":%d: %s", c.line, c.key); !strings.Contains(err.Error(), want) {
				t.Errorf("Error() = %q, want it to contain %q", err, want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(PathIn(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load(missing) = %v, want fs.ErrNotExist", err)
	}
	if got := PathIn("/p/.claude/diadoche"); got != filepath.Join("/p/.claude/diadoche", "smoke.yaml") {
		t.Errorf("PathIn = %q", got)
	}
}
