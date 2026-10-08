package smoke

import (
	"errors"
	"strings"
	"testing"
)

func TestParseYAMLSubset(t *testing.T) {
	src := `# leading comment
up: "echo \"hi\" # not a comment\n" # trailing comment
down: 'it''s # kept'
wait: {url: http://x/, timeout: '5s'}
checks:
- cmd: echo a: b
  timeout: 3s
- {url: "http://x/a, b", status: 201}
browser:
  - http://x/
  - http://y/ # comment
`
	root, err := parseYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	get := func(key string) *node {
		for _, e := range root.entries {
			if e.key == key {
				return e.value
			}
		}
		t.Fatalf("no key %q", key)
		return nil
	}
	if v := get("up").value; v != "echo \"hi\" # not a comment\n" {
		t.Errorf("up = %q", v)
	}
	if v := get("down").value; v != "it's # kept" {
		t.Errorf("down = %q", v)
	}
	wait := get("wait")
	if wait.kind != mapNode || len(wait.entries) != 2 || wait.entries[1].value.value != "5s" {
		t.Errorf("wait = %+v", wait)
	}
	checks := get("checks")
	if checks.kind != listNode || len(checks.items) != 2 {
		t.Fatalf("checks = %+v", checks)
	}
	first := checks.items[0]
	if first.kind != mapNode || first.entries[0].value.value != "echo a: b" || first.entries[1].value.value != "3s" || first.entries[1].line != 7 {
		t.Errorf("checks[1] = %+v", first)
	}
	if second := checks.items[1]; second.entries[0].value.value != "http://x/a, b" || second.entries[1].value.value != "201" {
		t.Errorf("checks[2] = %+v", second)
	}
	browser := get("browser")
	if len(browser.items) != 2 || browser.items[1].value != "http://y/" || browser.items[1].line != 11 {
		t.Errorf("browser = %+v", browser)
	}
}

func TestParseYAMLRejects(t *testing.T) {
	cases := []struct {
		name, src string
		line      int
		key, msg  string
	}{
		{"tab indent", "wait:\n\turl: http://x/\n", 2, "url", "tab"},
		{"doc marker", "up: a\n---\ndown: b\n", 2, "", "document markers"},
		{"anchor", "up: &a echo\n", 1, "up", "anchors"},
		{"alias", "down: *a\n", 1, "down", "aliases"},
		{"block scalar", "up: |\n  echo\n", 1, "up", "block scalars"},
		{"flow sequence", "browser: [http://x/]\n", 1, "browser", "flow sequences"},
		{"unterminated quote", "up: \"echo\n", 1, "up", "unterminated"},
		{"text after quote", "up: 'a' b\n", 1, "up", "after a quoted"},
		{"duplicate key", "up: a\ndown: b\nup: c\n", 3, "up", "duplicate"},
		{"too deep under nested map", "wait:\n  url:\n    - x\n", 3, "wait.url", "nested too deep"},
		{"too deep under list item", "checks:\n  - cmd:\n      x: y\n", 3, "checks[1].cmd", "nested too deep"},
		{"top-level list", "- a\n", 1, "", "list item"},
		{"empty item", "checks:\n  -\n", 2, "checks[1]", "empty list item"},
		{"bad indentation", "up: a\n  down: b\n", 2, "", "indentation"},
		{"not a key", "up a\n", 1, "", "key: value"},
		{"unclosed flow map", "up: {cmd: a\n", 1, "up", "close"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseYAML([]byte(c.src))
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if e.Line != c.line || e.Key != c.key || !strings.Contains(e.Msg, c.msg) {
				t.Errorf("err = %+v, want line %d key %q msg ~%q", e, c.line, c.key, c.msg)
			}
			if !strings.Contains(err.Error(), "smoke.yaml:") {
				t.Errorf("Error() = %q", err)
			}
		})
	}
}
