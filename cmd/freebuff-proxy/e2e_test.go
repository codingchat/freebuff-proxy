// E2E subprocess suite for the freebuff-proxy CLI. The proxy is built as a
// REAL binary (go build, never `go test -c`) and exercised end to end:
// serve + graceful drain, -version, port conflicts, JSON config, and
// bridge mode. Every subprocess pins
// its environment (AUTO_DISCOVER_TOKEN=false, mock upstream, HOME/PATH)
// so ambient developer config cannot leak into the child.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
)

// --- shared E2E helpers ---

var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

// moduleRoot walks up from the test working directory to the module root
// (the directory containing go.mod), so `go build ./cmd/freebuff-proxy`
// resolves regardless of where the test binary was launched from.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test working directory")
		}
		dir = parent
	}
}

// proxyBinary builds the real freebuff-proxy binary once per test run and
// returns its path. The shared build is never mutated; tests run a copy
// inside their own temp dir (proxyInDir).
func proxyBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "freebuff-proxy-e2e-build-*")
		if err != nil {
			buildErr = err
			return
		}
		name := "freebuff-proxy"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		bin := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/freebuff-proxy")
		cmd.Dir = moduleRoot(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
			return
		}
		buildPath = bin
	})
	if buildErr != nil {
		t.Fatalf("build binary: %v", buildErr)
	}
	return buildPath
}

// proxyInDir copies the shared binary into dir (which becomes the subprocess
// working directory), so .env and the executable share one directory exactly
// like a real install. Returns the binary path.
func proxyInDir(t *testing.T, dir string) string {
	t.Helper()
	name := "freebuff-proxy"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(dir, name)
	data, err := os.ReadFile(proxyBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0755); err != nil {
		t.Fatal(err)
	}
	return dst
}

// e2eEnv returns the environment for a subprocess: the parent's environment
// minus every freebuff-proxy config variable (a developer's exported
// AUTH_TOKENS/ADMIN_TOKEN/... must not leak into the child), plus the given
// KEY=VALUE overrides. For duplicate keys the later entry wins.
func e2eEnv(t *testing.T, overrides ...string) []string {
	t.Helper()
	testutil.UnsetConfigEnv(t)
	return append(os.Environ(), overrides...)
}

// freePort reserves a free loopback port and returns it. The listener is
// closed immediately; the tiny reuse race is standard for tests.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// eventually polls fn until it returns true or the deadline elapses.
func eventually(t *testing.T, what string, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if fn() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (timeout %s)", what, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// e2eHTTPClient returns a client that closes connections after every request
// so the proxy's graceful HTTP shutdown never waits on a parked keep-alive.
func e2eHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   30 * time.Second,
	}
}

// startProcess launches cmd with captured stdout/stderr and registers a
// cleanup that kills the child if the test fails before a graceful exit.
func startProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	prepareChild(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
}

// waitProcess waits for cmd to exit and returns its exit code, or fails the
// test when the process is still alive after timeout.
func waitProcess(t *testing.T, cmd *exec.Cmd, what string, timeout time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return 0
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		t.Fatalf("wait %s: %v", what, err)
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("%s did not exit within %s", what, timeout)
	}
	return -1
}

// runSimple runs a one-shot subprocess (-version, or the default serve in
// port-conflict cases) to completion and returns exit code, stdout, stderr.
func runSimple(t *testing.T, dir, bin string, args []string, env []string, timeout time.Duration) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	code := waitProcess(t, cmd, strings.Join(args, " "), timeout)
	return code, stdout.String(), stderr.String()
}

func writeDotenv(t *testing.T, dir string, kv map[string]string) {
	t.Helper()
	var sb strings.Builder
	for k, v := range kv {
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(v)
		sb.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(sb.String()), 0644); err != nil {
		t.Fatal(err)
	}
}

func getBody(t *testing.T, client *http.Client, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read GET %s: %v", url, err)
	}
	return resp, body
}

func postBody(t *testing.T, client *http.Client, url, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read POST %s: %v", url, err)
	}
	return resp, data
}

// healthzMode polls /healthz until it answers 200 and returns the parsed
// mode field ("pooled" / "bridge").
func healthzMode(t *testing.T, base string) string {
	t.Helper()
	client := e2eHTTPClient()
	var mode string
	eventually(t, "healthz", 20*time.Second, func() bool {
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false
		}
		var hz struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(body, &hz); err != nil {
			return false
		}
		mode = hz.Mode
		return true
	})
	return mode
}

// shutdownAndExpectExit sends the graceful shutdown signal and requires the
// process to exit 0 within the drain budget.
func shutdownAndExpectExit(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := sendShutdownSignal(cmd); err != nil {
		t.Skipf("cannot generate shutdown signal (no console?): %v", err)
	}
	if code := waitProcess(t, cmd, "graceful shutdown", 40*time.Second); code != 0 {
		t.Fatalf("proxy exited %d after shutdown signal, want 0", code)
	}
}

// --- 1. flagship: serve + chat + graceful drain ---

func TestE2EServeAndDrain(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	// Prewarm starts a run for every registry agent (~13) plus the chat's
	// run: give the mock enough run ids so every START succeeds.
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%04d", i)
	}
	mock.RunIDs = ids
	mock.ChatBody = testutil.SSEEvent(`{"id":"chatcmpl-e2e","object":"chat.completion.chunk","created":1,"model":"deepseek/deepseek-v4-flash","choices":[{"index":0,"delta":{"content":"hello-e2e"},"finish_reason":null}]}`) +
		testutil.SSEEvent(`{"id":"chatcmpl-e2e","object":"chat.completion.chunk","created":1,"model":"deepseek/deepseek-v4-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)

	dir := t.TempDir()
	port := freePort(t)
	writeDotenv(t, dir, map[string]string{
		"LISTEN_ADDR":       fmt.Sprintf("127.0.0.1:%d", port),
		"AUTH_TOKENS":       "cb_e2e",
		"UPSTREAM_BASE_URL": mock.URL(),
	})
	bin := proxyInDir(t, dir)
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = e2eEnv(t, "AUTO_DISCOVER_TOKEN=false")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	startProcess(t, cmd)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := e2eHTTPClient()

	// healthz up, pooled mode.
	eventually(t, "healthz", 20*time.Second, func() bool {
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	})
	resp, body := getBody(t, client, base+"/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d: %s", resp.StatusCode, body)
	}
	var hz struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(body, &hz); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, body)
	}
	if hz.Mode != "pooled" {
		t.Errorf("healthz mode = %q, want pooled", hz.Mode)
	}

	// /v1/models carries the deepseek models from the offline fallback
	// (or the live refresh, which contains the same ids).
	resp, body = getBody(t, client, base+"/v1/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("deepseek/deepseek-v4-flash")) {
		t.Errorf("models response missing deepseek/deepseek-v4-flash: %s", body)
	}

	// Streaming chat through the mock upstream: admission + session + SSE.
	chat := `{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"ping"}],"stream":true}`
	resp, body = postBody(t, client, base+"/v1/chat/completions", chat)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d: %s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("hello-e2e")) {
		t.Errorf("chat stream missing mock content; body:\n%s", body)
	}

	// Graceful shutdown: SIGINT (Ctrl+Break on Windows) → drain → exit 0.
	// The drain FINISHes the chat's run exactly once (runs.Shutdown), so the
	// single-FINISH assertion below runs after exit.
	if err := sendShutdownSignal(cmd); err != nil {
		t.Skipf("cannot generate shutdown signal (no console?): %v", err)
	}
	if code := waitProcess(t, cmd, "graceful shutdown", 40*time.Second); code != 0 {
		t.Fatalf("proxy exited %d after shutdown signal, want 0", code)
	}

	stderrText := stderr.String()
	stdoutText := stdout.String()
	// Log ordering: startup summary → shutting down → shutdown complete.
	iStart := strings.Index(stderrText, `msg="freebuff-proxy starting"`)
	iShut := strings.Index(stderrText, `msg="shutting down"`)
	iDone := strings.Index(stderrText, `msg="shutdown complete"`)
	if iStart < 0 || iShut < 0 || iDone < 0 || iStart >= iShut || iShut >= iDone {
		t.Errorf("stderr missing or misordered lifecycle logs (start=%d shut=%d done=%d); stderr:\n%s", iStart, iShut, iDone, stderrText)
	}
	// Startup summary carries counts, never token values.
	if !strings.Contains(stderrText, " auth_tokens=1 ") {
		t.Errorf("startup summary missing auth_tokens=1; stderr:\n%s", stderrText)
	}
	if !strings.Contains(stderrText, " bridge_mode=false ") {
		t.Errorf("startup summary missing bridge_mode=false; stderr:\n%s", stderrText)
	}
	// The interactive banner must be suppressed: stderr is piped, so
	// stderrIsCharDevice() is false and the banner must not be emitted.
	for _, out := range []string{stderrText, stdoutText} {
		if strings.Contains(out, "is running!") || strings.Contains(out, "Press Ctrl+C") {
			t.Errorf("interactive startup banner leaked into piped output:\n%s", out)
		}
	}
	// Every prewarmed/chat run must be FINISHed exactly once by the drain:
	// no run id may appear twice (the double-FINISH regression class), and
	// every started run must be finished.
	finished := mock.FinishedRunsSnapshot()
	started := len(mock.StartedRunsSnapshot())
	if len(finished) == 0 {
		t.Error("drain finished no runs, want every started run finished exactly once")
	}
	seen := make(map[string]bool, len(finished))
	for _, f := range finished {
		if seen[f.RunID] {
			t.Errorf("run %s was FINISHed more than once", f.RunID)
		}
		seen[f.RunID] = true
	}
	if len(finished) < started {
		t.Errorf("drain finished %d runs, want >= %d started runs finished", len(finished), started)
	}
}

// --- 2. -version ---

func TestE2EVersionFlag(t *testing.T) {
	dir := t.TempDir()
	bin := proxyInDir(t, dir)
	code, stdout, stderr := runSimple(t, dir, bin, []string{"-version"}, e2eEnv(t, "AUTO_DISCOVER_TOKEN=false"), 30*time.Second)
	if code != 0 {
		t.Fatalf("-version exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got := strings.TrimSpace(stdout); got != "freebuff-proxy dev" {
		t.Errorf("-version output = %q, want %q", got, "freebuff-proxy dev")
	}
}

// --- 3. port conflict ---

func TestE2EPortConflict(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	writeDotenv(t, dir, map[string]string{
		"LISTEN_ADDR":       fmt.Sprintf("127.0.0.1:%d", port),
		"AUTH_TOKENS":       "cb_e2e",
		"UPSTREAM_BASE_URL": mock.URL(),
	})
	bin := proxyInDir(t, dir)
	code, _, stderr := runSimple(t, dir, bin, nil, e2eEnv(t, "AUTO_DISCOVER_TOKEN=false"), 40*time.Second)
	if code != 1 {
		t.Fatalf("port-conflict exit = %d, want 1; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "cannot listen on") {
		t.Errorf("port-conflict stderr missing 'cannot listen on':\n%s", stderr)
	}
	if !strings.Contains(stderr, "already in use") {
		t.Errorf("port-conflict stderr missing 'already in use' hint:\n%s", stderr)
	}
}

// --- 4. -config JSON ---

func TestE2EConfigJSON(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	port := freePort(t)
	dir := t.TempDir()
	cfg := map[string]any{
		"LISTEN_ADDR":       fmt.Sprintf("127.0.0.1:%d", port),
		"AUTH_TOKENS":       []string{"cb_e2e"},
		"UPSTREAM_BASE_URL": mock.URL(),
	}
	cfgPath := filepath.Join(dir, "cfg.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	bin := proxyInDir(t, dir)
	cmd := exec.Command(bin, "-config", cfgPath)
	cmd.Dir = dir
	cmd.Env = e2eEnv(t, "AUTO_DISCOVER_TOKEN=false")
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	startProcess(t, cmd)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	if mode := healthzMode(t, base); mode != "pooled" {
		t.Errorf("-config JSON healthz mode = %q, want pooled", mode)
	}
	shutdownAndExpectExit(t, cmd)
	if !strings.Contains(stderr.String(), `msg="shutdown complete"`) {
		t.Errorf("-config JSON stderr missing 'shutdown complete':\n%s", stderr.String())
	}
}

// --- 5. bridge mode (explicit empty AUTH_TOKENS) ---

func TestE2EBridgeMode(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	port := freePort(t)
	dir := t.TempDir()
	writeDotenv(t, dir, map[string]string{
		"LISTEN_ADDR":       fmt.Sprintf("127.0.0.1:%d", port),
		"AUTH_TOKENS":       "", // explicit empty = bridge mode
		"UPSTREAM_BASE_URL": mock.URL(),
	})
	bin := proxyInDir(t, dir)
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = e2eEnv(t, "AUTO_DISCOVER_TOKEN=false")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	startProcess(t, cmd)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	if mode := healthzMode(t, base); mode != "bridge" {
		t.Errorf("bridge-mode healthz mode = %q, want bridge", mode)
	}
	shutdownAndExpectExit(t, cmd)
}
