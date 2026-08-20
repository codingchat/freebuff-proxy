package server

// Wave-2 observability tests (T16-T17): silent-endpoint coverage and
// access-log hygiene. Internal package so the tests can reach the unexported
// access gate and the Server struct directly.

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// newServer builds a test server over one mock token with a config mutation;
// returns the raw *Server for pool/option assertions.
func newServer(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config)) *Server {
	t.Helper()
	return newServerOpts(t, mock, mut)
}

// newServerOpts builds the full stack (one mock token, fallback registry)
// and returns the raw *Server, applying mut before construction.
func newServerOpts(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config)) *Server {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    "https://www.codebuff.com",
		LogAccess:          true,
	}
	if mut != nil {
		mut(cfg)
	}
	clientCfg := *cfg
	clientCfg.UpstreamBaseURL = mock.URL()
	client, err := upstream.New(cfg.AuthTokens[0], &clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	sess := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, p, reg, nil)
}

// newLoggingServer builds a full test server (one mock token, fallback
// registry) whose logger writes to a capture buffer at Debug level, so the
// T16-T17 log lines are assertable.
func newLoggingServer(t *testing.T, mock *testutil.MockUpstream, mut func(*config.Config)) (*Server, *bytes.Buffer) {
	t.Helper()
	srv := newServerOpts(t, mock, mut)
	var sink bytes.Buffer
	srv.logger = slog.New(slog.NewTextHandler(&sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return srv, &sink
}

// TestAccessLogGate pins the quiet-path gate (T17): the first request for a
// path logs, requests within the window are suppressed, a different path is
// its own gate, and a request after the window logs again.
func TestAccessLogGate(t *testing.T) {
	resetAccessLogGate()
	t.Cleanup(resetAccessLogGate)
	orig := accessQuietWindow
	accessQuietWindow = 60 * time.Second
	t.Cleanup(func() { accessQuietWindow = orig })

	t0 := time.Now()
	if !accessLogDue("/healthz", t0) {
		t.Fatal("first /healthz request must log")
	}
	if accessLogDue("/healthz", t0.Add(time.Second)) {
		t.Error("second /healthz within the window must be suppressed")
	}
	if !accessLogDue("/metrics", t0.Add(time.Second)) {
		t.Error("a different quiet path has its own gate")
	}
	if !accessLogDue("/healthz", t0.Add(61*time.Second)) {
		t.Error("/healthz after the window must log again (a new minute)")
	}
}

// TestAccessQuietEndpointsRateLimited verifies end-to-end that two /healthz
// requests in the same window produce one access line, and a request after
// the window produces a second (T17).
func TestAccessQuietEndpointsRateLimited(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	resetAccessLogGate()
	t.Cleanup(resetAccessLogGate)
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, nil)
	h := srv.Handler()

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	}
	if got := strings.Count(sink.String(), "msg=access"); got != 1 {
		t.Fatalf("access lines for two same-window /healthz = %d, want 1", got)
	}

	// Shrink the window to zero: the next request logs again (deterministic
	// stand-in for "two requests in different minutes").
	orig := accessQuietWindow
	accessQuietWindow = 0
	t.Cleanup(func() { accessQuietWindow = orig })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if got := strings.Count(sink.String(), "msg=access"); got != 2 {
		t.Fatalf("access lines after window expiry = %d, want 2", got)
	}
}

// TestAccessLogDisabledSuppressesLines verifies LOG_ACCESS=false turns the
// access lines off entirely (normal paths included), and flipping the
// effective config back on restores them (T17).
func TestAccessLogDisabledSuppressesLines(t *testing.T) {
	testutil.UnsetConfigEnv(t)
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, func(c *config.Config) { c.LogAccess = false })
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d, want 200", rec.Code)
	}
	if strings.Contains(sink.String(), "msg=access") {
		t.Fatal("access line logged with LOG_ACCESS=false")
	}

	// Runtime toggle back on (config reload semantics).
	cfg := *srv.cfg
	cfg.LogAccess = true
	srv.cfg = &cfg
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(sink.String(), "msg=access") {
		t.Fatal("no access line after LOG_ACCESS re-enabled")
	}
}

// TestEmbeddingsUnsupportedWarn verifies T16: the unsupported_endpoint 400
// logs a WARN with path, remote, and status.
func TestEmbeddingsUnsupportedWarn(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, nil)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"x"}`))
	req.RemoteAddr = "198.51.100.11:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("embeddings status = %d, want 400", rec.Code)
	}
	logs := sink.String()
	for _, want := range []string{"unsupported endpoint requested", "path=/v1/embeddings", "remote=198.51.100.11", "status=400"} {
		if !strings.Contains(logs, want) {
			t.Errorf("embeddings WARN missing %q", want)
		}
	}
}

// TestModelsEmptyRegistryWarn verifies T16: /v1/models with an empty
// registry logs a WARN (model_count 0) when requested — not at startup.
func TestModelsEmptyRegistryWarn(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	srv, sink := newLoggingServer(t, mock, nil)
	// Replace the fallback-populated registry with an empty one.
	srv.reg = registry.New(srv.cfg, nil)
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d, want 200", rec.Code)
	}
	logs := sink.String()
	for _, want := range []string{"model list requested with empty registry", "model_count=0"} {
		if !strings.Contains(logs, want) {
			t.Errorf("empty-registry WARN missing %q", want)
		}
	}
}
