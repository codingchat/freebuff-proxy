package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/upstream"
)

// requestFailedFields returns the newest `request failed` log line, or ""
// when absent. The line carries every structured field as a "key=value"
// attribute, so callers assert on substrings.
func requestFailedFields(buf *bytes.Buffer) string {
	lines := strings.Split(buf.String(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "request failed") {
			return lines[i]
		}
	}
	return ""
}

// countRequestFailedCode counts `request failed` records carrying the exact
// code= field (e.g. "code=rate_limited").
func countRequestFailedCode(buf *bytes.Buffer, codeField string) int {
	n := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "request failed") && strings.Contains(line, codeField) {
			n++
		}
	}
	return n
}

// TestRequestFailedWarnDedupe pins D6: 100 identical rate_limited errors
// produce <=4 `request failed` WARNs (1st + every 50th) while the per-key
// ledger always counts every occurrence; non-rate-limit codes log every
// time. The client response is written on every call regardless.
func TestRequestFailedWarnDedupe(t *testing.T) {
	resetRateLimitWarnDedupe()
	t.Cleanup(resetRateLimitWarnDedupe)

	t.Run("rate_limited burst fires <=4 WARNs", func(t *testing.T) {
		var sink bytes.Buffer
		s := &Server{logger: slog.New(slog.NewTextHandler(&sink, nil))}
		rle := &upstream.RateLimitError{Status: "", RetryAfter: time.Minute, Window: "reset", Body: "daily quota exhausted"}
		var gotStatus int
		for i := 0; i < 100; i++ {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			s.writeError(w, r, rle, "deepseek/deepseek-v4-flash", nil)
			gotStatus = w.Code
		}
		if gotStatus != http.StatusTooManyRequests {
			t.Errorf("response status = %d, want 429 even on suppressed WARNs", gotStatus)
		}
		if n := countRequestFailedCode(&sink, "code=rate_limited"); n > 4 {
			t.Errorf("`request failed` WARNs = %d, want <= 4 for 100 identical rate_limited errors", n)
		}
		rateLimitWarnDedupe.mu.Lock()
		n := rateLimitWarnDedupe.m["bridge|rate_limited|reset"]
		rateLimitWarnDedupe.mu.Unlock()
		if n != 100 {
			t.Errorf("dedupe ledger count = %d, want 100 (counter always increments)", n)
		}
	})

	t.Run("non-rate-limit codes log every time", func(t *testing.T) {
		var sink bytes.Buffer
		s := &Server{logger: slog.New(slog.NewTextHandler(&sink, nil))}
		be := &upstream.BanError{ResumesAt: time.Now().Add(time.Hour), Body: `{"status":"banned"}`}
		for i := 0; i < 25; i++ {
			w := httptest.NewRecorder()
			s.writeError(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), be, "", nil)
		}
		if n := countRequestFailedCode(&sink, "code=account_banned"); n != 25 {
			t.Errorf("`request failed` WARNs for banned = %d, want 25 (every time)", n)
		}
	})
}

// TestRequestFailedStructuredFields pins T6: the `request failed` WARN
// carries req_id, retry_after, reset_at, token and model when the caller and
// the error provide them.
func TestRequestFailedStructuredFields(t *testing.T) {
	resetRateLimitWarnDedupe()
	t.Cleanup(resetRateLimitWarnDedupe)
	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	t.Run("req_id token model retry_after", func(t *testing.T) {
		var sink bytes.Buffer
		s := &Server{logger: slog.New(slog.NewTextHandler(&sink, nil))}
		rle := &upstream.RateLimitError{RetryAfter: 90 * time.Second, Window: "retry-after", Body: "quota"}
		ctx := context.WithValue(context.Background(), reqIDKey{}, "req-test-123")
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		s.writeError(w, r, rle, "deepseek/deepseek-v4-flash", &pool.Lease{Token: 0})
		line := requestFailedFields(&sink)
		if line == "" {
			t.Fatal("no `request failed` WARN captured")
		}
		for _, want := range []string{
			"req_id=req-test-123",
			"retry_after=90",
			"token=1",
			"model=deepseek/deepseek-v4-flash",
			"code=rate_limited",
			"status=429",
		} {
			if !strings.Contains(line, want) {
				t.Errorf("`request failed` missing %q in %s", want, line)
			}
		}
		if strings.Contains(line, "reset_at=") {
			t.Errorf("unexpected reset_at when the error carries none: %s", line)
		}
	})

	t.Run("reset_at when the error carries it", func(t *testing.T) {
		var sink bytes.Buffer
		s := &Server{logger: slog.New(slog.NewTextHandler(&sink, nil))}
		rle := &upstream.RateLimitError{ResetAt: future, Window: "reset", Body: "quota"}
		w := httptest.NewRecorder()
		s.writeError(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), rle, "", &pool.Lease{Token: 0})
		line := requestFailedFields(&sink)
		if line == "" {
			t.Fatal("no `request failed` WARN captured")
		}
		if want := "reset_at=" + future.Format(time.RFC3339); !strings.Contains(line, want) {
			t.Errorf("`request failed` missing %q in %s", want, line)
		}
		if !strings.Contains(line, "retry_after=") {
			t.Errorf("`request failed` missing derived retry_after in %s", line)
		}
	})
}
