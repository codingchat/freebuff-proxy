package main

import (
	"flag"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"freebuff-proxy/internal/telemetry"
)

// TestHoldForExitIfConsolePipedStderrNoHang guards the console hold: with
// piped stderr (containers, log files, Task Scheduler, CI) holdForExitIfConsole
// must return immediately — a hang here would freeze every non-interactive
// startup error path.
func TestHoldForExitIfConsolePipedStderrNoHang(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan struct{})
	go func() {
		holdForExitIfConsole()
		close(done)
	}()
	select {
	case <-done:
		// Returned without waiting for input: an anonymous pipe is not a
		// character device, so the hold must be a no-op.
	case <-time.After(2 * time.Second):
		t.Fatal("holdForExitIfConsole blocked on piped stderr")
	}
}

// TestShutdownSignals guards the graceful-drain notify set: os.Interrupt and
// SIGTERM must always be registered. Go has no syscall.SIGBREAK constant on
// any platform — on Windows the runtime delivers both Ctrl+C and Ctrl+Break
// as os.Interrupt (runtime/os_windows.go ctrlHandler) — so the Ctrl+Break
// drain behavior itself is pinned by TestCtrlBreakDrainsGracefully
// (main_windows_test.go).
func TestShutdownSignals(t *testing.T) {
	got := shutdownSignals()
	has := func(want os.Signal) bool {
		for _, s := range got {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(os.Interrupt) {
		t.Error("shutdownSignals missing os.Interrupt (covers Ctrl+C and Ctrl+Break on Windows)")
	}
	if !has(syscall.SIGTERM) {
		t.Error("shutdownSignals missing syscall.SIGTERM")
	}
}

// TestVersionFlagPrintsVersion re-executes the test binary with -version
// (main() os.Exit's, so it cannot run in-process) and pins the output:
// "freebuff-proxy <version>" on stdout, exit 0.
func TestVersionFlagPrintsVersion(t *testing.T) {
	if os.Getenv("GO_WANT_VERSION_HELPER") == "1" {
		// Re-executed: the test framework already consumed -test.* flags on
		// the global flag set, so swap in a fresh set before running main.
		flag.CommandLine = flag.NewFlagSet("freebuff-proxy", flag.ExitOnError)
		os.Args = []string{"freebuff-proxy", "-version"}
		main()
		return // unreachable: main os.Exit(0)s
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestVersionFlagPrintsVersion$")
	cmd.Env = append(os.Environ(), "GO_WANT_VERSION_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper exited with error: %v\n%s", err, out)
	}
	if want := "freebuff-proxy " + version; !strings.Contains(string(out), want) {
		t.Errorf("output %q missing %q", out, want)
	}
}

// TestResolveLogLevel pins the effective log-level precedence: LOG_LEVEL
// config wins when set and parseable (even over -v), -v → debug, else
// info, and an unparseable LOG_LEVEL silently falls back to info.
func TestResolveLogLevel(t *testing.T) {
	cases := []struct {
		name     string
		logLevel string
		verbose  bool
		want     slog.Level
	}{
		{"empty not verbose", "", false, slog.LevelInfo},
		{"empty verbose", "", true, slog.LevelDebug},
		{"config wins", "warn", false, slog.LevelWarn},
		{"config beats verbose", "error", true, slog.LevelError},
		{"config case-insensitive", "DEBUG", false, slog.LevelDebug},
		{"trace level", "trace", false, telemetry.LevelTrace},
		{"trace case-insensitive", "TRACE", true, telemetry.LevelTrace},
		{"unparseable falls back to info", "bogus", true, slog.LevelInfo},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveLogLevel(tc.logLevel, tc.verbose); got != tc.want {
				t.Errorf("resolveLogLevel(%q, %v) = %v, want %v", tc.logLevel, tc.verbose, got, tc.want)
			}
		})
	}
}

// TestLogLevelDisplay pins the startup-summary level rendering: trace shows
// as TRACE (not slog's "DEBUG-4"), every other level keeps slog's name.
func TestLogLevelDisplay(t *testing.T) {
	cases := []struct {
		level slog.Level
		want  string
	}{
		{telemetry.LevelTrace, "TRACE"},
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelWarn, "WARN"},
		{slog.LevelError, "ERROR"},
	}
	for _, tc := range cases {
		if got := logLevelDisplay(tc.level); got != tc.want {
			t.Errorf("logLevelDisplay(%v) = %q, want %q", tc.level, got, tc.want)
		}
	}
}

// TestIgnoredExeAdjacentEnv pins the exe-adjacent .env warning branch: a
// .env next to the executable is flagged ONLY when the working directory
// differs from the executable's directory — the usual reason config "seems
// to vanish" under a launcher. Same directory, a missing exe-adjacent
// .env, or empty inputs must not warn.
func TestIgnoredExeAdjacentEnv(t *testing.T) {
	dir := t.TempDir()
	exeDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(exeDir, "freebuff-proxy.exe")
	envPath := filepath.Join(exeDir, ".env")
	if err := os.WriteFile(envPath, []byte("A=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("same working directory", func(t *testing.T) {
		if got := ignoredExeAdjacentEnv(exeDir, exe); got != "" {
			t.Errorf("same dir returned %q, want empty", got)
		}
	})
	t.Run("different cwd with exe-adjacent env", func(t *testing.T) {
		work := filepath.Join(dir, "work")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ignoredExeAdjacentEnv(work, exe); got != envPath {
			t.Errorf("cross-dir returned %q, want %q", got, envPath)
		}
	})
	t.Run("no env next to exe", func(t *testing.T) {
		cleanDir := filepath.Join(dir, "clean-bin")
		if err := os.MkdirAll(cleanDir, 0o755); err != nil {
			t.Fatal(err)
		}
		work := filepath.Join(dir, "work2")
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := ignoredExeAdjacentEnv(work, filepath.Join(cleanDir, "other.exe")); got != "" {
			t.Errorf("missing exe-adjacent env returned %q, want empty", got)
		}
	})
	t.Run("empty inputs", func(t *testing.T) {
		if got := ignoredExeAdjacentEnv("", exe); got != "" {
			t.Errorf("empty cwd returned %q, want empty", got)
		}
		if got := ignoredExeAdjacentEnv(exeDir, ""); got != "" {
			t.Errorf("empty exe path returned %q, want empty", got)
		}
	})
}
