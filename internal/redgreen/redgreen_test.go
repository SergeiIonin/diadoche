package redgreen

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestIsTestFile(t *testing.T) {
	cases := []struct {
		p     string
		globs []string
		want  bool
	}{
		{"foo_test.go", nil, true},
		{"internal/x/foo_test.go", nil, true},
		{"foo.go", nil, false},
		{"test_x.py", []string{"test_*.py"}, true},
		{"pkg/test_x.py", []string{"test_*.py"}, true},
		{"pkg/x.py", []string{"test_*.py"}, false},
		{"src/a.spec.ts", []string{"*.test.ts", "*.spec.ts"}, true},
		{"crates/x/tests/a.rs", []string{"**/tests/*.rs"}, true},
		{"tests/a.rs", []string{"**/tests/*.rs"}, true},
		{"tests/a.rs", []string{"tests/*.rs"}, true},
		{"x/tests/a.rs", []string{"tests/*.rs"}, false},
		{"src/a.rs", []string{"**/tests/*.rs"}, false},
	}
	for _, c := range cases {
		if got := IsTestFile(c.p, c.globs); got != c.want {
			t.Errorf("IsTestFile(%q, %q) = %v, want %v", c.p, c.globs, got, c.want)
		}
	}
}

func TestValidateGlobs(t *testing.T) {
	if err := ValidateGlobs([]string{"test_*.py", "**/tests/*.rs"}); err != nil {
		t.Errorf("valid globs rejected: %v", err)
	}
	err := ValidateGlobs([]string{"ok_*.py", "["})
	if err == nil || !strings.Contains(err.Error(), `"["`) {
		t.Errorf("ValidateGlobs([) = %v, want an error naming the glob", err)
	}
}

func TestTestNames(t *testing.T) {
	goSrc := "package x\n\nfunc TestA(t *testing.T) {}\nfunc helper() {}\nfunc FuzzB(f *testing.F) {}\nfunc ExampleC() {}\nfunc TestA(t *testing.T) {}\n"
	if got, want := TestNames("x_test.go", goSrc), []string{"TestA", "FuzzB", "ExampleC"}; !reflect.DeepEqual(got, want) {
		t.Errorf("go: %q, want %q", got, want)
	}
	pySrc := "def test_one():\n    pass\n\nclass T:\n    async def test_two(self):\n        pass\n    def helper(self):\n        pass\n"
	if got, want := TestNames("test_x.py", pySrc), []string{"test_one", "test_two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("py: %q, want %q", got, want)
	}
	jsSrc := "describe('suite', () => {\n  it('does a', () => {});\n  test(\"does b\", () => {});\n  it(`does c`, () => {});\n});\n"
	if got, want := TestNames("a.spec.ts", jsSrc), []string{"does a", "does b", "does c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("js: %q, want %q", got, want)
	}
	rbSrc := "describe X do\n  it 'works' do\n  end\nend\n"
	if got, want := TestNames("x_spec.rb", rbSrc), []string{"works"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rb: %q, want %q", got, want)
	}
	if got := TestNames("x.txt", "func TestA("); got != nil {
		t.Errorf("unknown extension: %q, want nil", got)
	}
}

func TestNewNames(t *testing.T) {
	base := "func TestOld(t *testing.T) {}\n"
	cur := base + "func TestNew(t *testing.T) {}\n"
	if got := NewNames("x_test.go", base, cur); !reflect.DeepEqual(got, []string{"TestNew"}) {
		t.Errorf("NewNames = %q, want [TestNew]", got)
	}
}

func TestRunCommand(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	if r := RunCommand(ctx, dir, "echo hello; exit 0", 0); !r.Passed() || !strings.Contains(r.Output, "hello") {
		t.Errorf("exit 0: %+v", r)
	}
	r := RunCommand(ctx, dir, "echo oops >&2; exit 3", 0)
	if r.Passed() || r.ExitCode != 3 || !strings.Contains(r.Output, "oops") {
		t.Errorf("exit 3: %+v", r)
	}

	r = RunCommand(ctx, dir, "pwd", 0)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(r.Output))
	want, _ := filepath.EvalSymlinks(dir)
	if got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}

	start := time.Now()
	r = RunCommand(ctx, dir, "sleep 5", 100*time.Millisecond)
	if r.Passed() || !r.TimedOut {
		t.Errorf("timeout: %+v", r)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("timed-out command took %v", time.Since(start))
	}

	if r := RunCommand(ctx, filepath.Join(dir, "missing"), "true", 0); r.Passed() || r.Err == nil {
		t.Errorf("missing dir: %+v, want a start error", r)
	}
}

func TestTail(t *testing.T) {
	if got := Tail("short", 10); got != "short" {
		t.Errorf("Tail(short) = %q", got)
	}
	if got := Tail("abcdef", 3); got != "…def" {
		t.Errorf("Tail = %q, want …def", got)
	}
	if got := Tail("aéb", 2); got != "…b" { // "é" is 2 bytes; cut lands mid-rune
		t.Errorf("Tail mid-rune = %q, want …b", got)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{limit: 10}
	for _, chunk := range []string{"0123456789", "abcdefghij", "ABCDEFGHIJ", "xyz"} {
		if n, err := b.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	want := "[output truncated: kept last 10 bytes]\nDEFGHIJxyz"
	if got := b.String(); got != want || !b.truncated {
		t.Errorf("String() = %q (truncated %v), want %q", got, b.truncated, want)
	}

	u := &tailBuffer{}
	u.Write([]byte(strings.Repeat("a", 100)))
	if got := u.String(); got != strings.Repeat("a", 100) || u.truncated {
		t.Errorf("unlimited buffer = %q (truncated %v)", got, u.truncated)
	}
}

func TestRunCommandLimit(t *testing.T) {
	r := RunCommandLimit(context.Background(), t.TempDir(), "i=0; while [ $i -lt 200 ]; do echo line$i; i=$((i+1)); done", 0, 50)
	if !r.Passed() {
		t.Fatalf("run: %+v", r)
	}
	if !strings.HasPrefix(r.Output, "[output truncated: kept last 50 bytes]\n") || !strings.HasSuffix(r.Output, "line199\n") {
		t.Errorf("Output = %q, want a truncation note and the last bytes", r.Output)
	}
	if body := strings.TrimPrefix(r.Output, "[output truncated: kept last 50 bytes]\n"); len(body) != 50 {
		t.Errorf("kept %d bytes, want 50", len(body))
	}
}
