package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// --- bridge mode ---

// newBridgeTestServer wires the server in bridge mode (no AUTH_TOKENS): the
// pool has no fixed tokens and lazily-created per-client-token clients talk
// to the given mock upstream.
func newBridgeTestServer(t *testing.T, mock *testutil.MockUpstream) (*httptest.Server, *pool.Pool) {
	t.Helper()
	cfg := &config.Config{
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		DashboardEnabled:   true,
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, p
}

func TestBridgeModeRequiresClientToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	// No Authorization header: 401 missing_bearer_token, nothing upstream.
	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "missing_bearer_token") {
		t.Errorf("body missing missing_bearer_token: %s", data)
	}
	if mock.SessionCreates != 0 || len(mock.StartedRuns) != 0 {
		t.Errorf("upstream contact = %d creates / %d runs, want 0/0 (missing token rejected before pool)",
			mock.SessionCreates, len(mock.StartedRuns))
	}

	// An empty Bearer value is also rejected.
	resp, data = doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer  "})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty bearer status = %d, want 401: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "missing_bearer_token") {
		t.Errorf("empty bearer body missing missing_bearer_token: %s", data)
	}
}

func TestBridgeModeRelaysClientToken(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b1", 1, `"choices":[{"index":0,"delta":{"content":"bridged"},"finish_reason":null}]`))
	ts, _ := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-abc"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "bridged") {
		t.Errorf("stream missing content: %s", data)
	}

	// The upstream saw the CLIENT's token, not a proxy-configured one.
	if len(mock.RecordedChatHeaders) != 1 {
		t.Fatalf("upstream chat calls = %d, want 1", len(mock.RecordedChatHeaders))
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer client-tok-abc" {
		t.Errorf("upstream Authorization = %q, want %q", got, "Bearer client-tok-abc")
	}

	// A second request with the same token reuses the entry: no new run.
	resp2, data2 := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-abc"})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d, want 200: %s", resp2.StatusCode, data2)
	}
	if got := len(mock.StartedRuns); got != 1 {
		t.Errorf("started runs = %d, want 1 (entry reused across requests)", got)
	}
}

func TestBridgeModeXAPIKey(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b2", 2, `"choices":[{"index":0,"delta":{"content":"keyed"},"finish_reason":null}]`))
	ts, _ := newBridgeTestServer(t, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), map[string]string{"x-api-key": "client-tok-xyz"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "keyed") {
		t.Errorf("stream missing content: %s", data)
	}
	if got := mock.RecordedChatHeaders[0].Get("Authorization"); got != "Bearer client-tok-xyz" {
		t.Errorf("upstream Authorization = %q, want %q (x-api-key relayed as bearer)", got, "Bearer client-tok-xyz")
	}
}

func TestBridgeModeModelsAndHealthz(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)

	// /v1/models and /healthz need no header in bridge mode.
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		BridgeTokens int `json:"bridge_tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.BridgeTokens != 0 {
		t.Errorf("bridge_tokens = %d, want 0 (no chat requests yet)", out.BridgeTokens)
	}

	// After a bridged chat the healthz counter reflects the cached entry.
	doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-h"})
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.BridgeTokens != 1 {
		t.Errorf("bridge_tokens = %d, want 1 after a chat", out.BridgeTokens)
	}
}

func TestBridgeModeChat401Cooldown(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatStatus = 401
	mock.ChatErrorBody = `{"error":{"message":"unauthorized","type":"authentication_error"}}`
	ts, _ := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"
	hdr := map[string]string{"Authorization": "Bearer client-tok-401"}

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), hdr)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "upstream_auth_rejected") {
		t.Errorf("body missing upstream_auth_rejected: %s", data)
	}

	// The entry's token went on cooldown; the next request surfaces the
	// cooldown without re-hitting upstream.
	mock.ChatStatus = 200
	resp2, data2 := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), hdr)
	if resp2.StatusCode != http.StatusBadGateway {
		t.Fatalf("second request status = %d, want 502 (cooldown): %s", resp2.StatusCode, data2)
	}
	if !strings.Contains(string(data2), "cooling down") {
		t.Errorf("second request body = %q, want cooldown error", data2)
	}
	if got := len(mock.RecordedChatHeaders); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (cooldown skipped upstream)", got)
	}
}

// TestBridgeModeHealthzReportsMode pins the healthz "mode" field in pure
// bridge mode.
func TestBridgeModeHealthzReportsMode(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newBridgeTestServer(t, mock)
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.Mode != "bridge" {
		t.Errorf("mode = %q, want bridge", out.Mode)
	}
}

// RequestsServed counts successful upstream chats in bridge mode too (the
// metrics page reads it, so it must not stay 0 when no AUTH_TOKENS exist).
func TestBridgeRequestsServedCounter(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-b2", 1, `"choices":[{"index":0,"delta":{"content":"x"},"finish_reason":null}]`))
	ts, p := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"
	for range 3 {
		resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, data)
		}
	}
	if got := p.PoolSnapshot().RequestsServed; got != 3 {
		t.Fatalf("RequestsServed = %d, want 3 (bridge chats must count)", got)
	}
}

// TestBridgeModelUnfitNotGated pins the bridge exemption: bridge clients
// relay their own token (their account may serve the model on this egress
// and their session slots are theirs to spend), so the registry never gates
// them even when (egress, model) is marked unfit.
func TestBridgeModelUnfitNotGated(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-bg1", 1, `"choices":[{"index":0,"delta":{"content":"bridged"},"finish_reason":null}]`))
	ts, p := newBridgeTestServer(t, mock)
	chatURL := ts.URL + "/v1/chat/completions"

	p.MarkModelUnfit(modelA, &upstream.LimitedIpError{Body: "pre-marked unfit"})
	if until, _ := p.ModelUnfit(modelA); until.IsZero() {
		t.Fatal("pre-mark not set")
	}

	resp, data := doJSON(t, http.MethodPost, chatURL, chatBody(modelA), map[string]string{"Authorization": "Bearer client-tok-abc"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge status = %d, want 200 (bridge never gated): %s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "bridged") {
		t.Errorf("stream missing bridged content: %s", data)
	}
	if got := len(mock.RecordedChatHeaders); got != 1 {
		t.Errorf("upstream chat calls = %d, want 1 (bridge ignored the unfit mark)", got)
	}
}
