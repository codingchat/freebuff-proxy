// pool_scarce_test.go — issue #178: quota-exhaustion cooldowns are isolated
// per model, so a token quota-capped for one scarce/promo model
// (z-ai/glm-5.2, gpt-5.6-luna) keeps serving its other models
// (deepseek/deepseek-v4-flash) instead of being locked token-wide.
package pool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"freebuff-proxy/internal/testutil"
	"freebuff-proxy/internal/upstream"
)

// TestQuotaCooldownIsolationAcrossModels pins issue #178 at the pool level:
// a token whose remembered cooldown is a quota exhaustion for one model is
// still eligible for a DIFFERENT model — the luna daily cap must not
// block flash on the same token — while the capped model itself keeps
// surfacing the remembered 429.
func TestQuotaCooldownIsolationAcrossModels(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()
	p := newTestPool(t, mock)

	// Seed token 1's cooldown exactly as an upstream quota-exhaustion 429
	// for gpt-5.6-luna lands: quota fields at/over the limit with a future reset.
	rle := &upstream.RateLimitError{
		Model:       "openai/gpt-5.6-luna",
		Status:      "rate_limited",
		RetryAfter:  time.Hour,
		Limit:       2,
		RecentCount: 2,
		Period:      "pacific_day",
		ResetAt:     time.Now().Add(time.Hour),
		Body:        "session quota exhausted for model",
	}
	p.CooldownTokenRateLimit(0, rle)

	// The capped model itself is refused: the token is cooling down for it.
	_, err := p.Acquire(context.Background(), "openai/gpt-5.6-luna")
	var got *upstream.RateLimitError
	if !errors.As(err, &got) {
		t.Fatalf("Acquire(openai/gpt-5.6-luna) on quota-capped token = %v, want 429", err)
	}

	// A different model on the SAME token is not blocked by that cooldown.
	lease, err := p.Acquire(context.Background(), "deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("Acquire(flash) blocked by luna quota cooldown: %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire(flash) = nil lease")
	}
	p.LeaseRelease(lease)
}

// TestAdmissionQuota429TagsModelAndIsolates pins issue #178 end-to-end: a
// 429 quota-exhaustion on session CREATE for luna whose body omits the
// model field is tagged with the requested model by the admission path,
// cools the token per-model, and the same token still admits flash after.
func TestAdmissionQuota429TagsModelAndIsolates(t *testing.T) {
	mock := testutil.NewMock()
	defer mock.Close()

	var creates atomic.Int32
	mock.SessionHandler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && creates.Add(1) == 1 {
			// First create (gpt-5.6-luna): quota-exhausted 429 with NO model
			// field — the proxy must tag the requested model itself.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"status":"rate_limited","limit":2,"recentCount":2,"period":"pacific_day","resetAt":"`+
				time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")+`","retryAfterMs":3600000}`)
			return
		}
		// Subsequent creates (flash) and probes/polls: active.
		expiresAt := time.Now().Add(30 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z07:00")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"status":"active","instanceId":"inst-abc-123","expiresAt":"`+expiresAt+`"}`)
	}

	p := newTestPool(t, mock)

	_, err := p.Acquire(context.Background(), "openai/gpt-5.6-luna")
	var rle *upstream.RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("Acquire(openai/gpt-5.6-luna) = %v, want 429", err)
	}
	if rle.Model != "openai/gpt-5.6-luna" {
		t.Errorf("RLE.Model = %q, want %q (tagged from the requested model)", rle.Model, "openai/gpt-5.6-luna")
	}
	if !isQuotaExhaustedError(rle) {
		t.Errorf("RLE not classified as quota exhaustion: %+v", rle)
	}

	// The same token now serves flash despite the luna quota cooldown.
	lease, err := p.Acquire(context.Background(), "deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("Acquire(flash) blocked by luna quota cooldown: %v", err)
	}
	if lease == nil {
		t.Fatal("Acquire(flash) = nil lease")
	}
	p.LeaseRelease(lease)
}
