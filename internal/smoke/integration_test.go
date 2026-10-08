//go:build integration

package smoke

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const integrationServer = `package main

import (
	"flag"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	flag.Parse()
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	panic(http.ListenAndServe(*addr, nil))
}
`

// buildServer compiles a tiny HTTP server into dir/server.
func buildServer(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	for name, body := range map[string]string{
		"main.go": integrationServer,
		"go.mod":  "module smokesrv\n\ngo 1.21\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "build", "-o", "server", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func runIntegration(t *testing.T, missingStatus int) (Result, string) {
	t.Helper()
	dir := t.TempDir()
	buildServer(t, dir)
	addr := freeAddr(t)
	src := fmt.Sprintf(`up: ./server -addr %[1]s >server.log 2>&1 & echo $! > server.pid
wait:
  url: http://%[1]s/healthz
  timeout: 30s
  interval: 100ms
checks:
  - url: http://%[1]s/healthz
    status: 200
  - url: http://%[1]s/missing
    status: %[2]d
down: kill $(cat server.pid)
`, addr, missingStatus)
	path := PathIn(dir)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	res := Run(context.Background(), Options{Config: cfg, Dir: dir})
	return res, addr
}

// assertStopped polls the health URL until the server is gone.
func assertStopped(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err != nil {
			return
		}
		resp.Body.Close()
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("server still answers after down")
}

func TestIntegrationRealServer(t *testing.T) {
	res, addr := runIntegration(t, 404)
	if !res.OK() || !res.DownRan || !res.DownOK {
		t.Fatalf("result:\n%s", res.Log())
	}
	if got := statuses(res); got != "up=ok wait=ok checks[1]=ok checks[2]=ok down=ok" {
		t.Errorf("steps = %s", got)
	}
	assertStopped(t, addr)
}

func TestIntegrationRealServerFailingCheck(t *testing.T) {
	res, addr := runIntegration(t, 200)
	if res.Failed != "checks[2]" || !res.DownRan || !res.DownOK {
		t.Fatalf("result:\n%s", res.Log())
	}
	if f := res.FailedStep(); f.Detail != "HTTP 404, want 200" {
		t.Errorf("detail = %q", f.Detail)
	}
	assertStopped(t, addr)
}
