package telemetry

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// closeLogFile closes the log file held by a NewLogger result so TempDir
// cleanup can delete it (Windows refuses to delete open files).
func closeLogFile(t *testing.T, logger *slog.Logger) {
	t.Helper()
	th, ok := logger.Handler().(*textHandler)
	if !ok {
		t.Fatalf("logger handler is %T, want *textHandler", logger.Handler())
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if th.file != nil {
		_ = th.file.Close()
		th.file = nil
	}
}

// captureStderr reroutes os.Stderr for the duration of fn and returns
// everything written to it. Not safe under t.Parallel.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	fn()
	_ = w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}

func TestNewLoggerLevelSelection(t *testing.T) {
	infoFile := filepath.Join(t.TempDir(), "info.log")
	infoLogger := NewLogger(false, infoFile)
	infoLogger.Debug("debug line")
	infoLogger.Info("info line")
	closeLogFile(t, infoLogger)

	data, err := os.ReadFile(infoFile)
	if err != nil {
		t.Fatalf("read %s: %v", infoFile, err)
	}
	got := string(data)
	if !strings.Contains(got, `msg="info line"`) {
		t.Errorf("Info line missing from log file: %q", got)
	}
	if strings.Contains(got, "debug line") {
		t.Errorf("Debug line logged at Info level: %q", got)
	}

	debugFile := filepath.Join(t.TempDir(), "debug.log")
	debugLogger := NewLogger(true, debugFile)
	debugLogger.Debug("debug line")
	closeLogFile(t, debugLogger)

	data, err = os.ReadFile(debugFile)
	if err != nil {
		t.Fatalf("read %s: %v", debugFile, err)
	}
	if !strings.Contains(string(data), `msg="debug line"`) {
		t.Errorf("Debug line missing at Debug level: %q", data)
	}
}

func TestNewLoggerAppendsToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	first := NewLogger(true, path)
	first.Info("first")
	second := NewLogger(true, path)
	second.Info("second")
	closeLogFile(t, first)
	closeLogFile(t, second)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	got := string(data)
	if !strings.Contains(got, "msg=first") || !strings.Contains(got, "msg=second") {
		t.Errorf("expected both lines appended, got: %q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("log file contains ANSI color escapes: %q", got)
	}
}

func TestNewLoggerColors(t *testing.T) {
	out := captureStderr(t, func() {
		logger := NewLogger(true, "")
		logger.Debug("m-debug")
		logger.Info("m-info")
		logger.Warn("m-warn")
		logger.Error("m-error")
	})
	for _, want := range []struct{ msg, token string }{
		{"m-debug", "\x1b[90mDEBUG\x1b[0m"},
		{"m-info", "\x1b[32mINFO\x1b[0m"},
		{"m-warn", "\x1b[33mWARN\x1b[0m"},
		{"m-error", "\x1b[31mERROR\x1b[0m"},
	} {
		if !strings.Contains(out, want.msg) {
			t.Errorf("stderr missing message %q", want.msg)
		}
		if !strings.Contains(out, want.token) {
			t.Errorf("stderr missing color token %q (for %s): %q", want.token, want.msg, out)
		}
	}
}

func TestRedactHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer topsecret")
	h.Add("x-api-key", "key-1")
	h.Add("x-api-key", "key-2")
	h.Set("Cookie", "session=abc")
	h.Set("Set-Cookie", "sid=xyz")
	h.Set("Content-Type", "application/json")
	h.Set("X-Custom", "keepme")

	got := RedactHeaders(h)

	if h.Get("Authorization") != "Bearer topsecret" {
		t.Error("RedactHeaders modified the input header")
	}
	for _, k := range []string{"Authorization", "X-Api-Key", "Cookie", "Set-Cookie"} {
		if v := got[k][0]; v != "[redacted]" {
			t.Errorf("RedactHeaders[%q] = %q, want [redacted]", k, v)
		}
	}
	if v := got["Content-Type"][0]; v != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", v)
	}
	if v := got["X-Custom"][0]; v != "keepme" {
		t.Errorf("X-Custom = %q, want keepme", v)
	}
	if vs := got["X-Api-Key"]; len(vs) != 2 || vs[0] != "[redacted]" || vs[1] != "[redacted]" {
		t.Errorf("X-Api-Key values = %v, want two [redacted]", vs)
	}
}

func TestRedactHeadersNonCanonicalKey(t *testing.T) {
	h := http.Header{"x-api-key": {"k"}}
	got := RedactHeaders(h)
	if v := got["x-api-key"][0]; v != "[redacted]" {
		t.Errorf("lowercase raw key value = %q, want [redacted]", v)
	}
}

func TestDumpRequest(t *testing.T) {
	_ = os.RemoveAll("dump")
	t.Cleanup(func() { _ = os.RemoveAll("dump") })

	req, err := http.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	body := strings.Repeat("x", 21000)

	DumpRequest("chat", req, 200, body, true)

	entries, err := filepath.Glob(filepath.Join("dump", "chat-*.dump"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one dump file, got %v", entries)
	}
	if !strings.Contains(filepath.Base(entries[0]), "_v1_chat_completions") {
		t.Errorf("dump name %q missing sanitized path", filepath.Base(entries[0]))
	}

	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "POST http://example.com/v1/chat/completions") {
		t.Errorf("dump missing request line: %q", content)
	}
	if !strings.Contains(content, "Authorization: [redacted]") {
		t.Errorf("dump missing redacted Authorization: %q", content)
	}
	if !strings.Contains(content, "Content-Type: application/json") {
		t.Errorf("dump lost non-sensitive header: %q", content)
	}
	if !strings.Contains(content, "[status 200]") {
		t.Errorf("dump missing status block: %q", content)
	}
	if !strings.Contains(content, strings.Repeat("x", 20000)) {
		t.Error("dump missing truncated body")
	}
	if strings.Contains(content, strings.Repeat("x", 20001)) {
		t.Error("dump body not truncated at 20000")
	}

	// Permission bits are only meaningful off Windows (Go synthesizes 0666
	// there); CI runs the exact 0600 check on Linux.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(entries[0])
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("dump mode = %o, want 600", got)
		}
	}
}

func TestDumpRequestDisabled(t *testing.T) {
	_ = os.RemoveAll("dump")
	t.Cleanup(func() { _ = os.RemoveAll("dump") })

	req, err := http.NewRequest(http.MethodGet, "http://example.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	DumpRequest("chat-disabled", req, 200, "x", false)

	entries, err := filepath.Glob(filepath.Join("dump", "chat-disabled-*.dump"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("dump written while disabled: %v", entries)
	}
}

func TestParseLevel(t *testing.T) {
	if _, ok := ParseLevel(""); ok {
		t.Error(`ParseLevel("") ok=true, want false`)
	}
	if lv, ok := ParseLevel("debug"); !ok || lv != slog.LevelDebug {
		t.Errorf("ParseLevel(debug) = %v, ok=%v; want %v, true", lv, ok, slog.LevelDebug)
	}
	if lv, ok := ParseLevel("INFO"); !ok || lv != slog.LevelInfo {
		t.Errorf("ParseLevel(INFO) = %v, ok=%v; want %v, true", lv, ok, slog.LevelInfo)
	}
	if _, ok := ParseLevel("bogus"); ok {
		t.Error("ParseLevel(bogus) ok=true, want false")
	}
}
