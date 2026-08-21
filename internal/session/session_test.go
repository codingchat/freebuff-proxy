package session

import (
	"bytes"
	"log/slog"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func newTestManager(t *testing.T, mock *testutil.MockUpstream) *Manager {
	t.Helper()
	client, err := upstream.New("tok", &config.Config{
		UpstreamBaseURL:    mock.URL(),
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(client)
}

// — T9/T10/T11: session lifecycle telemetry (wave 2). —

// captureLogs swaps slog's default handler for a buffer-backed text handler
// at Debug level and returns the restore function. Session tests run
// sequentially (no t.Parallel), so swapping the process default is safe.
func captureLogs(buf *bytes.Buffer) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }
}
