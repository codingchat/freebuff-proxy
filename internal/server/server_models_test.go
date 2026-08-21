package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// flakyFirstRT fails the very first request with a transient transport error
// and delegates everything else to base (mirrors pool_test's helper; drives a
// real retry deterministically across platforms).
type flakyFirstRT struct {
	mu     sync.Mutex
	failed bool
	base   http.RoundTripper
}

func (f *flakyFirstRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	shouldFail := !f.failed
	if shouldFail {
		f.failed = true
	}
	f.mu.Unlock()
	if shouldFail {
		return nil, fmt.Errorf("read tcp 127.0.0.1:443: connection reset by peer")
	}
	return f.base.RoundTrip(req)
}

type quotaEntry struct {
	Limit       float64            `json:"limit"`
	RecentCount float64            `json:"recent_count"`
	Period      string             `json:"period"`
	ResetAt     string             `json:"reset_at"`
	Entitlement map[string]float64 `json:"entitlement"`
}

func TestUnknownModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	for _, tc := range []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"unknown_model", `{"model":"no/such-model","messages":[{"role":"user","content":"hi"}]}`, http.StatusNotFound},
		{"missing_model", `{"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(tc.body), nil)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.wantStatus, data)
			}
			if !strings.Contains(string(data), "model_not_found") {
				t.Errorf("body missing model_not_found: %s", data)
			}
		})
	}

	// Rejected before the pool: the upstream must be untouched.
	if mock.SessionCreates != 0 {
		t.Errorf("upstream session creates = %d, want 0", mock.SessionCreates)
	}
}

func TestModelsEndpoint(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID        string `json:"id"`
			Object    string `json:"object"`
			Created   int64  `json:"created"`
			OwnedBy   string `json:"owned_by"`
			Available bool   `json:"available"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, data)
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want list", out.Object)
	}
	// #121: the offline fallback pruned 5 dead model ids (laguna/ling/greg),
	// 15 -> 10 rows (registry fallbackAgents, free-agents.ts-verified).
	if len(out.Data) < 10 {
		t.Errorf("models = %d, want >= 10", len(out.Data))
	}
	for i, m := range out.Data {
		if m.ID == "" || m.Object != "model" || m.OwnedBy == "" {
			t.Errorf("model %d malformed: %+v", i, m)
		}
		if m.Created != out.Data[0].Created {
			t.Errorf("model %d created = %d, want %d (pinned to server start)", i, m.Created, out.Data[0].Created)
		}
		// Advisory annotation: never hide a working model, so available is
		// true and status "unknown" when no session has reported anything.
		if !m.Available {
			t.Errorf("model %s available = false, want true (advisory default)", m.ID)
		}
		if m.Status == "" {
			t.Errorf("model %s status empty, want a status string", m.ID)
		}
	}
	if out.Data[0].Created <= 0 || out.Data[0].Created > time.Now().Unix() {
		t.Errorf("created = %d, not a plausible server-start timestamp", out.Data[0].Created)
	}
}

// TestMaxVariantsBlocked pins issue #153: the -max ids are excluded from the
// ServedModels gate, so they are neither listed on /v1/models nor servable —
// upstream's session admission resolves them to mimo/mimo-v2.5 anyway.
func TestMaxVariantsBlocked(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServer(t, nil, mock)

	// /v1/models must not advertise any -max id.
	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	for _, m := range out.Data {
		if strings.HasSuffix(m.ID, "-max") {
			t.Errorf("/v1/models leaked blocked -max id %q", m.ID)
		}
	}

	// Chat requests for a -max id are rejected before touching upstream.
	for _, model := range []string{
		"deepseek/deepseek-v4-flash-max",
		"deepseek/deepseek-v4-pro-max",
		"openai/gpt-5.6-luna-max",
	} {
		resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(model), nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("chat %s status = %d, want 404: %s", model, resp.StatusCode, data)
		}
	}
	if len(mock.RecordedChatHeaders) != 0 {
		t.Error("upstream chat recorded for a blocked -max model, want none")
	}
}

func TestHealthz(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	mock1 := testutil.NewMock()
	defer mock1.Close()
	ts, _ := newTestServer(t, nil, mock0, mock1)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		UptimeSeconds float64 `json:"uptime_seconds"`
		Models        int     `json:"models"`
		Tokens        []any   `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, data)
	}
	if out.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %v, want >= 0", out.UptimeSeconds)
	}
	// #121: fallback registry 15 -> 10 rows after pruning dead model ids.
	if out.Models < 10 {
		t.Errorf("models = %d, want >= 10", out.Models)
	}
	if len(out.Tokens) != 2 {
		t.Errorf("tokens = %d, want 2", len(out.Tokens))
	}
}

func TestMetricsEndpoint(t *testing.T) {
	mock0 := testutil.NewMock()
	defer mock0.Close()
	ts, _ := newTestServer(t, nil, mock0)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	if !strings.Contains(body, "freebuff_proxy_uptime_seconds") || !strings.Contains(body, "freebuff_proxy_models_total") {
		t.Errorf("metrics missing expected keys: %s", body)
	}
}

// TestHealthzSpend pins the /healthz spend surface (issue #122): the ledger
// buckets fed by the chat feeder, the advisory MAX_SPEND_PER_DAY ceiling
// (SpendLimit), the capped SpendPct, and the SpendLimited refusal counter.
func TestHealthzSpend(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-s1", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-s1", 1, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}`))
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) { c.MaxSpendPerDay = 100 }, mock)

	req := `{"model":"` + modelA + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", []byte(req), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200: %s", resp.StatusCode, data)
	}

	// The spend feeder runs after the relay flushes the response, so the
	// first healthz read can race it (see waitSpend); poll briefly.
	deadline := time.Now().Add(waitSpendTimeout)
	var tok struct {
		Spend24h     int64 `json:"Spend24h"`
		SpendDay     int64 `json:"SpendDay"`
		SpendLimit   int64 `json:"SpendLimit"`
		SpendPct     int   `json:"SpendPct"`
		SpendLimited int   `json:"SpendLimited"`
	}
	for {
		resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
		}
		var out struct {
			Tokens []struct {
				Spend24h     int64 `json:"Spend24h"`
				SpendDay     int64 `json:"SpendDay"`
				SpendLimit   int64 `json:"SpendLimit"`
				SpendPct     int   `json:"SpendPct"`
				SpendLimited int   `json:"SpendLimited"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("healthz is not JSON: %v: %s", err, data)
		}
		if len(out.Tokens) != 1 {
			t.Fatalf("tokens = %d, want 1", len(out.Tokens))
		}
		tok = out.Tokens[0]
		if tok.Spend24h == 13 && tok.SpendDay == 13 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("healthz spend did not reach 13/13 within %s (last: %d/%d)", waitSpendTimeout, tok.SpendDay, tok.Spend24h)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if tok.SpendLimit != 100 {
		t.Errorf("SpendLimit = %d, want 100 (MAX_SPEND_PER_DAY)", tok.SpendLimit)
	}
	if tok.SpendPct != 13 {
		t.Errorf("SpendPct = %d, want 13 (13 of 100)", tok.SpendPct)
	}
	if tok.SpendLimited != 0 {
		t.Errorf("SpendLimited = %d, want 0 (no upstream spend_limited refusals)", tok.SpendLimited)
	}
}

func TestHealthzQuota(t *testing.T) {
	mock := quotaMock(t)
	ts, _ := newTestServer(t, nil, mock)

	// A chat admits the session (which carries rateLimitsByModel).
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Tokens []struct {
			Quota       map[string]quotaEntry `json:"quota"`
			Entitlement map[string]float64    `json:"entitlement"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(out.Tokens))
	}
	q, ok := out.Tokens[0].Quota["z-ai/glm-5.2"]
	if !ok {
		t.Fatalf("healthz quota missing z-ai/glm-5.2: %+v", out.Tokens[0].Quota)
	}
	if q.Limit != 5 || q.RecentCount != 4 || q.Period != "pacific_day" {
		t.Errorf("quota = %+v, want limit=5 recent_count=4 period=pacific_day", q)
	}
	if q.ResetAt == "" {
		t.Error("reset_at missing from healthz quota entry")
	}
	if q.Entitlement["referral"] != 1 || q.Entitlement["streak"] != 3 {
		t.Errorf("entitlement = %+v, want referral=1 streak=3", q.Entitlement)
	}
	if len(out.Tokens[0].Entitlement) != 0 {
		t.Errorf("top-level entitlement = %+v, want omitted (empty)", out.Tokens[0].Entitlement)
	}
}

// TestModelsAnnotationWithQuota verifies /v1/models reflects a session that
// admitted a model: status becomes "available" once quotaByModel mentions it.
func TestModelsAnnotationWithQuota(t *testing.T) {
	mock := quotaMock(t)
	mock.CountryCode = "US"
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID        string `json:"id"`
			Available bool   `json:"available"`
			Status    string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	var found *struct {
		ID        string `json:"id"`
		Available bool   `json:"available"`
		Status    string `json:"status"`
	}
	for i := range out.Data {
		if out.Data[i].ID == modelA {
			found = &out.Data[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("model %q not in /v1/models", modelA)
	}
	if !found.Available {
		t.Errorf("available = false, want true")
	}
	if found.Status != "available" {
		t.Errorf("status = %q, want available (session admitted the model)", found.Status)
	}
}

// TestModelsAllowList verifies MODELS_ALLOW prunes /v1/models to exactly the
// allowlisted ids so picker clients never auto-select a model that 404s.
func TestModelsAllowList(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelsAllow = []string{"deepseek/deepseek-v4-flash", modelB}
	}, mock)

	resp, data := doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	seen := map[string]bool{}
	for _, m := range out.Data {
		if m.ID != "deepseek/deepseek-v4-flash" && m.ID != modelB {
			t.Errorf("model %q listed outside MODELS_ALLOW", m.ID)
		}
		seen[m.ID] = true
	}
	if !seen["deepseek/deepseek-v4-flash"] || !seen[modelB] {
		t.Errorf("allowlisted models missing from /v1/models: %v", seen)
	}
	if len(out.Data) != 2 {
		t.Errorf("model count = %d, want 2 (allowlisted only)", len(out.Data))
	}
}

// TestModelsAllowRejectsChat pins the chat 404: a request whose resolved
// model is outside MODELS_ALLOW is rejected before any upstream call.
func TestModelsAllowRejectsChat(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelsAllow = []string{"deepseek/deepseek-v4-flash"}
	}, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelB), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat status = %d, want 404: %s", resp.StatusCode, data)
	}
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, data)
	}
	if out.Error.Code != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", out.Error.Code)
	}
	if !strings.Contains(out.Error.Message, "MODELS_ALLOW") {
		t.Errorf("error.message = %q, want a mention of MODELS_ALLOW", out.Error.Message)
	}
	if len(mock.RecordedChatHeaders) != 0 {
		t.Error("upstream chat recorded for a rejected model, want none")
	}
}

// TestModelsAllowResolvedAlias pins the allowlist contract: it compares
// against the RESOLVED model id (after registry alias resolution), so a
// client alias that resolves outside the list is rejected too.
func TestModelsAllowResolvedAlias(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelAliases = map[string]string{"fable-alias": modelB}
		c.ModelsAllow = []string{"deepseek/deepseek-v4-flash"}
	}, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("fable-alias"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat (alias) status = %d, want 404: %s", resp.StatusCode, data)
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, data)
	}
	if out.Error.Code != "model_not_found" {
		t.Errorf("error.code = %q, want model_not_found", out.Error.Code)
	}
	if len(mock.RecordedChatHeaders) != 0 {
		t.Error("upstream chat recorded for a rejected alias, want none")
	}
}

// TestModelsAllowPassthrough pins the simplified allowlist interaction: no
// auto-upgrade exists, so an allowlisted BASE id serves exactly the base
// model, an allowlisted -max id serves the -max model, and the catalog
// always stays exactly the MODELS_ALLOW list.
func TestModelsAllowPassthrough(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-max", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts, _ := newTestServerCfg(t, nil, func(c *config.Config) {
		c.ModelsAllow = []string{"deepseek/deepseek-v4-pro"}
	}, mock)

	// The allowlisted base id is served as-is (no -max upgrade).
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("deepseek/deepseek-v4-pro"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat (allowlisted base) status = %d, want 200: %s", resp.StatusCode, data)
	}
	// A -max id NOT in the allowlist stays rejected (no implicit expansion).
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody("deepseek/deepseek-v4-pro-max"), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat (unlisted -max) status = %d, want 404: %s", resp.StatusCode, data)
	}
	// A model outside the list stays rejected.
	resp, data = doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat (disallowed) status = %d, want 404: %s", resp.StatusCode, data)
	}

	// /v1/models lists exactly the allowlist, nothing else.
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	listed := map[string]bool{}
	for _, m := range out.Data {
		listed[m.ID] = true
	}
	if !listed["deepseek/deepseek-v4-pro"] {
		t.Errorf("/v1/models missing allowlisted base id deepseek/deepseek-v4-pro: %v", listed)
	}
	if listed["deepseek/deepseek-v4-pro-max"] {
		t.Errorf("/v1/models leaked -max variant under base-only MODELS_ALLOW: %v", listed)
	}
	if len(out.Data) != 1 {
		t.Errorf("model count = %d, want 1 (base allowlist only)", len(out.Data))
	}
}

// TestModelsAllowEmptyIsOpen verifies an empty allowlist keeps current
// behavior: every model is served and listed.
func TestModelsAllowEmptyIsOpen(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-open", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}
	resp, data = doJSON(t, http.MethodGet, ts.URL+"/v1/models", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("models is not JSON: %v: %s", err, data)
	}
	if len(out.Data) < 2 {
		t.Fatalf("model count = %d, want full catalog (no allowlist)", len(out.Data))
	}
	var hasModelA, hasFlash bool
	for _, m := range out.Data {
		hasModelA = hasModelA || m.ID == modelA
		hasFlash = hasFlash || m.ID == "deepseek/deepseek-v4-flash"
	}
	if !hasModelA || !hasFlash {
		t.Errorf("full catalog missing models: modelA=%v flash=%v", hasModelA, hasFlash)
	}
}

// TestSmokeDefaultsToFallbackModel verifies the smoke test with no explicit
// model probes the guaranteed fallback (deepseek-v4-flash), not the
// alphabetical-first catalog model (anthropic/claude-fable-5, a gated offer).
func TestSmokeDefaultsToFallbackModel(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-sm", 1, `"choices":[{"index":0,"delta":{"content":"ping"},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	// Smoke with no model field: server picks the fallback.
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/admin/smoke", []byte(`{"prompt":"ping"}`), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("smoke status = %d, want 200: %s", resp.StatusCode, data)
	}
	if len(mock.RecordedChatHeaders) == 0 {
		t.Fatal("no upstream chat recorded")
	}
	// #106: the smoke probe is a chat POST — the model rides in the body,
	// not an x-freebuff-model header.
	if got := mock.RecordedChatHeaders[0].Get("x-freebuff-model"); got != "" {
		t.Errorf("smoke probe chat POST carries x-freebuff-model %q, want absent (#106)", got)
	}
	if len(mock.RecordedChatBodies) == 0 {
		t.Fatal("no upstream chat body recorded")
	}
	if !strings.Contains(mock.RecordedChatBodies[0], `"model":"deepseek/deepseek-v4-flash"`) {
		t.Errorf("smoke probe body missing model deepseek/deepseek-v4-flash: %s", mock.RecordedChatBodies[0])
	}
}

// TestHealthzModeCountry verifies healthz surfaces the effective routing
// mode plus the per-token country from the session admission.
func TestHealthzModeCountry(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.CountryCode = "US"
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200: %s", resp.StatusCode, data)
	}
	var out struct {
		Mode   string `json:"mode"`
		Tokens []struct {
			Country string `json:"country"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("healthz is not JSON: %v: %s", err, data)
	}
	if out.Mode != "pooled" {
		t.Errorf("mode = %q, want pooled", out.Mode)
	}
	if len(out.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(out.Tokens))
	}
	if out.Tokens[0].Country != "US" {
		t.Errorf("country = %q, want US", out.Tokens[0].Country)
	}
}

func TestMetricsQuotaLines(t *testing.T) {
	mock := quotaMock(t)
	ts, _ := newTestServer(t, nil, mock)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	for _, want := range []string{
		`freebuff_proxy_quota_recent{token="1",model="z-ai/glm-5.2",period="pacific_day"} 4`,
		`freebuff_proxy_quota_limit{token="1",model="z-ai/glm-5.2",period="pacific_day"} 5`,
		`freebuff_proxy_quota_remaining{token="1",model="z-ai/glm-5.2",period="pacific_day"} 1`,
		fmt.Sprintf(`freebuff_proxy_session_remaining_seconds{token="1",model="%s"}`, modelA),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

// TestMetricsLabelEscaping verifies Prometheus label values (model id and
// period, both upstream-derived) are escaped: quotes become \" so the text
// format stays parseable.
func TestMetricsLabelEscaping(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.RateLimitsByModel = map[string]any{
		`weird"model`: map[string]any{
			"model":       `weird"model`,
			"limit":       5,
			"recentCount": 4,
			"period":      `p"d`,
			"resetAt":     "2026-08-16T07:00:00.000Z",
		},
	}
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-qe1", 100,
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]`)) +
		testutil.SSEEvent(chunk("chatcmpl-qe1", 100,
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`))
	ts, _ := newTestServer(t, nil, mock)

	// A chat admits the session (which carries rateLimitsByModel).
	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	for _, want := range []string{
		`freebuff_proxy_quota_recent{token="1",model="weird\"model",period="p\"d"} 4`,
		`freebuff_proxy_quota_limit{token="1",model="weird\"model",period="p\"d"} 5`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s in:\n%s", want, body)
		}
	}
}

func TestMetricsTransientRetryCounters(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.ChatBody = testutil.SSEEvent(chunk("chatcmpl-r", 1, `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]`)) + "data: [DONE]\n\n"

	cfg := &config.Config{
		AuthTokens:         []string{"tok-0"},
		RotationInterval:   time.Hour,
		RequestTimeout:     15 * time.Minute,
		SessionCallTimeout: 5 * time.Second,
		RegistryRefresh:    6 * time.Hour,
		UpstreamBaseURL:    mock.URL(),
		DashboardEnabled:   true,
		TransientRetries:   1,
	}
	client, err := upstream.New("tok-0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The first upstream call (agent-runs START during lease acquisition)
	// fails once at the transport level; TRANSIENT_RETRIES replays it.
	client.SetTransport(&flakyFirstRT{base: http.DefaultTransport})

	sess := session.NewManager(client)
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	p, err := pool.New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(cfg, p, reg, nil, nil, "")
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, data := doJSON(t, http.MethodPost, ts.URL+"/v1/chat/completions", chatBody(modelA), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", resp.StatusCode, data)
	}

	resp, data = doJSON(t, http.MethodGet, ts.URL+"/metrics", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200: %s", resp.StatusCode, data)
	}
	body := string(data)
	if !strings.Contains(body, `freebuff_proxy_transient_retries_total{token="1"} 1`) {
		t.Errorf("metrics missing transient retry line: %s", body)
	}
	// No TLS fingerprint is pinned in this setup, so no rotation happened
	// and the fingerprint value line must not be emitted (only when > 0).
	if strings.Contains(body, "freebuff_proxy_fingerprint_rotations_total{token=\"1\"}") {
		t.Errorf("metrics emitted a fingerprint rotation value with no rotation: %s", body)
	}
}
