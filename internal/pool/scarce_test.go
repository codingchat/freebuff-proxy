// scarce_test.go — tests for scarce-model session protection and quota-exhaustion fallback (issue #155).
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

func newTestScarcePool(t *testing.T, mock *testutil.MockUpstream, cfgFn func(*config.Config)) (*Pool, *registry.Registry) {
	t.Helper()
	cfg := &config.Config{
		AuthTokens:         []string{"tok-1"},
		UpstreamBaseURL:    mock.URL(),
		SessionCallTimeout: 5 * time.Second,
		RequestTimeout:     15 * time.Minute,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
		ScarceSessionModels: []string{
			"deepseek/deepseek-v4-pro",
			"openai/gpt-5.6-luna",
		},
		QuotaFallbackModels: map[string]string{
			"deepseek/deepseek-v4-flash": "mimo/mimo-v2.5",
		},
	}
	if cfgFn != nil {
		cfgFn(cfg)
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()

	client, err := upstream.New(cfg.AuthTokens[0], cfg)
	if err != nil {
		t.Fatal(err)
	}
	sess := session.NewManager(client)
	p, err := New(cfg, []*upstream.Client{client}, []*session.Manager{sess}, reg)
	if err != nil {
		t.Fatal(err)
	}
	return p, reg
}

func TestScarceSessionErrorFormatting(t *testing.T) {
	exp := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	err := &ScarceSessionError{Model: "openai/gpt-5.6-luna", ExpiresAt: exp}
	want := "scarce session (openai/gpt-5.6-luna) in use until 2026-08-21T15:00:00Z"
	if got := err.Error(); got != want {
		t.Errorf("ScarceSessionError.Error() = %q, want %q", got, want)
	}
}

func TestAcquireScarceSessionProtection(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fmt.Sprintf(`{
				"status": "active",
				"instanceId": "inst-luna",
				"model": "openai/gpt-5.6-luna",
				"expiresAt": "%s"
			}`, time.Now().Add(45*time.Minute).Format(time.RFC3339)))
			return
		}
		http.NotFound(w, r)
	}
	p, _ := newTestScarcePool(t, mock, nil)

	// Pre-seed the token with the active scarce session
	toks := *p.toks.Load()
	_, err := toks[0].session.EnsureSessionForModel(context.Background(), "openai/gpt-5.6-luna")
	if err != nil {
		t.Fatalf("setup EnsureSessionForModel: %v", err)
	}

	// Requesting the SAME scarce model succeeds and reuses
	lease, err := p.Acquire(context.Background(), "openai/gpt-5.6-luna")
	if err != nil {
		t.Fatalf("Acquire(gpt-5.6-luna): %v", err)
	}
	p.LeaseRelease(lease)

	// Requesting a DIFFERENT model (e.g. flash) when the token is busy with a scarce session
	// returns ScarceSessionError instead of burning the scarce session.
	_, err = p.Acquire(context.Background(), "deepseek/deepseek-v4-flash")
	if err == nil {
		t.Fatal("expected ScarceSessionError for flash request while luna is active, got nil")
	}
	var scse *ScarceSessionError
	if !errors.As(err, &scse) {
		t.Fatalf("expected errors.As ScarceSessionError, got %T: %v", err, err)
	}
	if scse.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("scse.Model = %q, want deepseek/deepseek-v4-flash", scse.Model)
	}
}

func TestAcquireQuotaExhaustionFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	// Setup admission returning quota exhausted for flash (recentCount >= limit, future resetAt)
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		modelReq := r.Header.Get("x-freebuff-model")
		if r.Method == http.MethodPost {
			if modelReq == "deepseek/deepseek-v4-flash" {
				// 429 quota exhausted
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, fmt.Sprintf(`{
					"status": "rate_limited",
					"model": "deepseek/deepseek-v4-flash",
					"limit": 5,
					"recentCount": 5,
					"period": "pacific_day",
					"resetAt": "%s",
					"retryAfterMs": 3600000
				}`, time.Now().Add(time.Hour).Format(time.RFC3339)))
				return
			}
			// mimo-v2.5 succeeds
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, fmt.Sprintf(`{
				"status": "active",
				"instanceId": "inst-mimo-fallback",
				"model": "%s",
				"expiresAt": "%s"
			}`, modelReq, time.Now().Add(time.Hour).Format(time.RFC3339)))
			return
		}
		http.NotFound(w, r)
	}

	p, _ := newTestScarcePool(t, mock, nil)

	// Acquire flash -> should fall back to mimo-v2.5
	lease, err := p.Acquire(context.Background(), "deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("Acquire flash with fallback: %v", err)
	}
	defer p.LeaseRelease(lease)

	if lease.Model != "mimo/mimo-v2.5" {
		t.Errorf("lease.Model = %q, want mimo/mimo-v2.5 (fallback)", lease.Model)
	}
	// Issue #164: the fallback lease must report why it serves a different
	// model so the server surfaces X-FreeBuff-Fallback: quota_exhausted.
	if lease.FallbackReason != "quota_exhausted" {
		t.Errorf("lease.FallbackReason = %q, want quota_exhausted", lease.FallbackReason)
	}
}

func TestAcquireBridgeScarceAndFallback(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	cfg := &config.Config{
		AuthTokens:         nil, // Bridge mode
		UpstreamBaseURL:    mock.URL(),
		SessionCallTimeout: 5 * time.Second,
		RequestTimeout:     15 * time.Minute,
		RotationInterval:   6 * time.Hour,
		RegistryRefresh:    6 * time.Hour,
		ScarceSessionModels: []string{
			"deepseek/deepseek-v4-pro",
		},
		QuotaFallbackModels: map[string]string{
			"deepseek/deepseek-v4-flash": "mimo/mimo-v2.5",
		},
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fmt.Sprintf(`{
				"status": "active",
				"instanceId": "inst-bridge-pro",
				"model": "deepseek/deepseek-v4-pro",
				"expiresAt": "%s"
			}`, time.Now().Add(30*time.Minute).Format(time.RFC3339)))
			return
		}
		http.NotFound(w, r)
	}
	p, err := New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	leasePro, err := p.AcquireBridge(context.Background(), "cb_test_client", "deepseek/deepseek-v4-pro")
	if err != nil {
		t.Fatalf("AcquireBridge pro: %v", err)
	}
	p.LeaseRelease(leasePro)

	// Now ask for flash on the same token -> scarce protected
	_, err = p.AcquireBridge(context.Background(), "cb_test_client", "deepseek/deepseek-v4-flash")
	if err == nil {
		t.Fatal("expected ScarceSessionError in bridge mode, got nil")
	}
	var scse *ScarceSessionError
	if !errors.As(err, &scse) {
		t.Fatalf("expected ScarceSessionError, got %v", err)
	}
}

func TestBridgeMaintainSkipsActiveScarceEviction(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	cfg := &config.Config{
		AuthTokens:          nil, // Bridge mode
		UpstreamBaseURL:     mock.URL(),
		SessionCallTimeout:  5 * time.Second,
		RequestTimeout:      15 * time.Minute,
		RotationInterval:    6 * time.Hour,
		RegistryRefresh:     6 * time.Hour,
		IdleRotationTimeout: 0,
		ScarceSessionModels: []string{
			"openai/gpt-5.6-luna",
		},
	}
	reg := registry.New(cfg, nil)
	reg.LoadFallback()

	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fmt.Sprintf(`{
				"status": "active",
				"instanceId": "inst-bridge-scarce",
				"model": "openai/gpt-5.6-luna",
				"expiresAt": "%s"
			}`, time.Now().Add(50*time.Minute).Format(time.RFC3339)))
			return
		}
		http.NotFound(w, r)
	}
	p, err := New(cfg, nil, nil, reg)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := p.AcquireBridge(context.Background(), "cb_token_scarce", "openai/gpt-5.6-luna")
	if err != nil {
		t.Fatal(err)
	}
	p.LeaseRelease(lease)

	// Age the entry past bridgeIdleEvict (2 hours)
	key := tokenKey("cb_token_scarce")
	p.bridgeMu.Lock()
	entry := p.bridge[key]
	entry.lastUsed = time.Now().Add(-3 * time.Hour)
	p.bridgeMu.Unlock()

	// Run bridgeMaintain
	p.bridgeMaintain(context.Background(), false)

	// Entry must STILL be cached because it holds an active scarce session
	p.bridgeMu.Lock()
	_, stillCached := p.bridge[key]
	p.bridgeMu.Unlock()

	if !stillCached {
		t.Error("bridgeMaintain evicted entry with active scarce session; want kept alive")
	}
}
