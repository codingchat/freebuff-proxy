package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// quotaSummary renders the live per-model session quota from a probe's
// RateLimitsByModel map (models sorted for determinism), plus glmPromo when
// the response carried it (the
// x-freebuff-include-unused-rate-limits probe header asks upstream to
// include the unused limits); "" when the upstream response carried no quota
// data (compact responses omit it).
func quotaSummary(st *upstream.SessionState) string {
	if st == nil || (len(st.RateLimitsByModel) == 0 && st.GlmPromo == "") {
		return ""
	}
	var parts []string
	models := make([]string, 0, len(st.RateLimitsByModel))
	for id := range st.RateLimitsByModel {
		models = append(models, id)
	}
	sort.Strings(models)
	for _, id := range models {
		q := st.RateLimitsByModel[id]
		entry := fmt.Sprintf("%s %s/%s", id, strconv.FormatFloat(q.Limit, 'f', -1, 64), strconv.FormatFloat(q.RecentCount, 'f', -1, 64))
		if q.Period != "" {
			entry += " " + q.Period
		}
		if !q.ResetAt.IsZero() {
			entry += fmt.Sprintf(", resets %s", q.ResetAt.Format(time.RFC3339))
		}
		parts = append(parts, entry)
	}
	if st.GlmPromo != "" {
		parts = append(parts, "glmPromo "+st.GlmPromo)
	}
	return "quota: " + strings.Join(parts, "; ")
}

// openAIError is the OpenAI error body with an optional human-readable hint (#19).
// Per OpenAPI 3.1 specification (reference/openai-openapi/openapi.yaml), code,
// message, param, and type are standard; param is null when unset.
type openAIError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code,omitempty"`
	Hint    string  `json:"hint,omitempty"`
}

// isAnthropicRequest reports whether the incoming request is destined for the
// Anthropic Messages surface (/v1/messages) or carries Anthropic headers.
func isAnthropicRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		return true
	}
	if r.Header.Get("anthropic-version") != "" || r.Header.Get("anthropic-api-key") != "" {
		return true
	}
	return false
}

// anthropicErrorType maps HTTP status code and internal error code to standard
// Anthropic error types per reference/anthropic-sdk-typescript.
func anthropicErrorType(status int, code string) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusServiceUnavailable && (code == "waiting_room_queued" || code == "waiting_room_required" || code == "capacity_deferred"):
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// writeAnthropicError writes an Anthropic-formatted error response:
// {"type": "error", "error": {"type": "...", "message": "...", "code": "..."}}
func (s *Server) writeAnthropicError(w http.ResponseWriter, r *http.Request, status int, message, code string, retryAfter time.Duration) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	version := "2023-06-01"
	if r != nil {
		if reqVer := r.Header.Get("anthropic-version"); reqVer != "" {
			version = reqVer
		}
	}
	h.Set("anthropic-version", version)
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	}
	w.WriteHeader(status)
	errType := anthropicErrorType(status, code)
	errMap := map[string]any{
		"type":    errType,
		"message": message,
	}
	if code != "" {
		errMap["code"] = code
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": errMap,
	})
}

// writeJSONError writes an OpenAI-shaped error response. Retry-After is set
// (in ceil seconds) only when retryAfter > 0.
func (s *Server) writeJSONError(w http.ResponseWriter, status int, message, typ, code string, retryAfter time.Duration) {
	s.writeJSONErrorWithHint(w, status, message, typ, code, "", retryAfter)
}

func (s *Server) writeJSONErrorWithHint(w http.ResponseWriter, status int, message, typ, code, hint string, retryAfter time.Duration) {
	if hint == "" {
		hint = defaultHintForCode(code, message)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(retryAfter.Seconds()))))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": openAIError{Message: message, Type: typ, Code: code, Hint: hint},
	})
}

func defaultHintForCode(code, message string) string {
	lowerMsg := strings.ToLower(message)
	switch {
	case code == "free_mode_cli_required" || strings.Contains(lowerMsg, "free_mode_cli_required"):
		return "Upstream free tier gate requires official CLI traffic envelope. See FAQ: https://github.com/trefeon/freebuff-proxy#faq"
	case code == "account_banned" || strings.Contains(lowerMsg, "banned"):
		return "Account suspended upstream. Token is dead; create a fresh account with an established GitHub login."
	case code == "country_blocked" || strings.Contains(lowerMsg, "country blocked") || strings.Contains(lowerMsg, "country_blocked"):
		return "Your egress IP is in an unsupported region. Route traffic through an allowed country (e.g. US/EU/ID/SG)."
	case code == "out_of_credits" || strings.Contains(lowerMsg, "out of credits"):
		return "Upstream free-tier credits exhausted. Check COST_MODE=free in .env — a typo routes requests as PAID and fresh free accounts get 402."
	case code == "upstream_timeout":
		return "The upstream request exceeded its deadline. Retry, or raise REQUEST_TIMEOUT/SESSION_CALL_TIMEOUT in .env."
	case code == "upstream_auth_rejected" || code == "invalid_api_key" || strings.Contains(lowerMsg, "invalid api key"):
		return "Token invalid or expired. Get a fresh token by running scripts/gen-token.cmd (Windows) or scripts/gen-token.sh (Linux/macOS)"
	case code == "rate_limited":
		return "Daily message cap or rate limit reached. Wait for quota reset or add another token."
	case code == "missing_bearer_token":
		return "Bridge mode active: pass your FreeBuff token in Authorization: Bearer <token>"
	case code == "model_not_found":
		return "Check available models via GET /v1/models"
	default:
		return ""
	}
}

// rateLimitWarnDedupe gates identical (token, code, window) `request failed`
// WARNs (D6): the first + every 50th occurrence fire; the per-key counter
// always increments so a silent burst stays countable, and the client
// response is always written. Package-level = per-process, shared by every
// server instance.
var rateLimitWarnDedupe = struct {
	mu sync.Mutex
	m  map[string]int64
}{}

// resetRateLimitWarnDedupe clears the dedupe ledger (test hook).
func resetRateLimitWarnDedupe() {
	rateLimitWarnDedupe.mu.Lock()
	defer rateLimitWarnDedupe.mu.Unlock()
	rateLimitWarnDedupe.m = make(map[string]int64)
}

// rateLimitWarnShouldLog reports whether the (token, code, window) WARN
// should fire for this occurrence, always incrementing the occurrence count.
func rateLimitWarnShouldLog(key string) bool {
	rateLimitWarnDedupe.mu.Lock()
	defer rateLimitWarnDedupe.mu.Unlock()
	if rateLimitWarnDedupe.m == nil {
		rateLimitWarnDedupe.m = make(map[string]int64)
	}
	rateLimitWarnDedupe.m[key]++
	n := rateLimitWarnDedupe.m[key]
	return n == 1 || n%50 == 0
}

// writeError maps any error from the pool/upstream to the PRD §6 matrix and
// logs it once. Canceled client contexts are logged at debug and dropped (no
// response written). model and lease come from the call site: model is the
// request's effective model, lease the acquired token lease (nil when the
// error fired before acquisition — e.g. an unfit-egress refusal).
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error, model string, lease *pool.Lease) {
	if errors.Is(err, context.Canceled) {
		s.logger.Debug("request canceled by client", "err", err)
		return
	}
	if r != nil && r.Context().Err() != nil {
		s.logger.Debug("client context canceled; not writing error", "err", err)
		return
	}

	status := http.StatusBadGateway
	code := "upstream_unavailable"
	message := err.Error()
	var retryAfter time.Duration
	var resetAt time.Time
	window := "" // T7 ledger window; set for rate-limit errors (dedupe key)

	var wr *session.WaitingRoomError
	var uwr *upstream.WaitingRoomError
	var wrr *upstream.WaitingRoomRequiredError
	var sse *upstream.SessionSupersededError
	var ue *upstream.UpstreamError
	var rle *upstream.RateLimitError
	var ice *upstream.IpCappedError
	var sle *upstream.SessionLimitError
	var lie *upstream.LimitedIpError
	var be *upstream.BanError
	var cbe *upstream.CountryBlockedError
	var ce *upstream.CreditsError
	var cde *upstream.CapacityDeferredError
	switch {
	case errors.As(err, &be):
		status, code = http.StatusForbidden, "account_banned"
		message, retryAfter = be.Error(), time.Until(be.ResumesAt)
		resetAt = be.ResumesAt
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &rle):
		status, code = http.StatusTooManyRequests, "rate_limited"
		switch rle.Status {
		case "load_shedding":
			// #133: upstream load saturation — minutes-scale transient with
			// a bounded cooldown; surfaced honestly instead of the daily-cap
			// "rate_limited" hint.
			code = "load_shedding"
		case "peak_hours":
			// #133: upstream peak-hours pricing window — bounded cooldown,
			// not a quota lock.
			code = "peak_hours"
		}
		message, retryAfter = rle.Error(), rle.RetryAfter
		resetAt, window = rle.ResetAt, rle.Window
		if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
			retryAfter = time.Until(rle.ResetAt)
		}
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &ice):
		// ip_capped: admission-only (too many distinct users on the egress
		// IP) — 429, not the quota 429, with the body's retryAfterMs only.
		status, code = http.StatusTooManyRequests, "ip_capped"
		message, retryAfter = ice.Error(), ice.RetryAfter
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &sle):
		// session_limit_reached (409): the ACCOUNT is over its concurrent-tab
		// budget; this session's row is fine. Never session-invalid.
		status, code = http.StatusConflict, "session_limit_reached"
		message = sle.Body
		if message == "" {
			message = "session limit reached"
		}
	case errors.As(err, &lie):
		// Issue #74 P2: the egress IP cannot serve the requested model.
		// 409 (not a quota lock): a different egress or token may still
		// serve the model. The body's retryAfterMs is surfaced
		// as Retry-After but does not set the unfit window.
		status, code = http.StatusConflict, "model_ip_limited"
		message, retryAfter = lie.Error(), lie.RetryAfter
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.Is(err, upstream.ErrModelIPLimited):
		// Bare sentinel (registry entry stored without refusal detail):
		// same 409 contract, no Retry-After to surface.
		status, code = http.StatusConflict, "model_ip_limited"
		message = err.Error()
		retryAfter = 0
	case errors.As(err, &wr):
		status, code = http.StatusServiceUnavailable, "waiting_room_queued"
		message, retryAfter = wr.Error(), wr.RetryAfter
	case errors.As(err, &uwr):
		status, code = http.StatusServiceUnavailable, "waiting_room_queued"
		message, retryAfter = uwr.Error(), uwr.RetryAfter
	case errors.As(err, &wrr):
		// #116: 428 waiting_room_required (endsTheSession:true — the seat
		// is gone; chatAttempt already dropped the cached session and
		// re-admitted once). 503 + the refusal's Retry-After — NEVER a bare
		// 502. MUST precede the generic UpstreamError branch.
		status, code = http.StatusServiceUnavailable, "waiting_room_required"
		message, retryAfter = wrr.Error(), wrr.RetryAfter
		if retryAfter < 0 {
			retryAfter = 0
		}
	case errors.As(err, &sse):
		// #119: 503 session_superseded — another instance took over the
		// account. Return 503 + Retry-After (not 409) so 9router retries
		// immediately instead of locking the model for 30s. The session is
		// already invalidated in chatAttempt so the next request re-joins fresh.
		status, code = http.StatusServiceUnavailable, "session_superseded"
		message = sse.Body
		if message == "" {
			message = "session superseded"
		}
		retryAfter = 1 // retry in 1s
	case errors.As(err, &cde):
		// #105 (server half): the client's capacity-deferred retry budget
		// (TRANSIENT_RETRIES) is exhausted, so the free tier's transient
		// capacity queue is surfaced to downstream clients as 429 +
		// Retry-After — they must honor the window, not hammer a 502/503.
		// MUST precede the generic errors.As(err, &ue) branch: the error
		// unwraps to a Retryable UpstreamError, which would otherwise be
		// swallowed as 503 upstream_retryable.
		status, code = http.StatusTooManyRequests, "free_mode_capacity_deferred"
		message = cde.Body
		if message == "" {
			message = cde.Error()
		}
		retryAfter = cde.RetryAfter
		if retryAfter <= 0 {
			retryAfter = 10 * time.Second
		}
	case errors.As(err, &ue):
		if ue.Retryable {
			// deployment_outside_hours etc.: temporarily unavailable, worth
			// a later retry — 503 lets clients/9router back off instead of
			// treating it as a hard failure.
			status, code = http.StatusServiceUnavailable, "upstream_retryable"
		} else {
			status = ue.Status
			if status != http.StatusPaymentRequired && status != http.StatusConflict && status != http.StatusTooManyRequests {
				status = http.StatusBadGateway
			}
		}
		message = ue.Body
		if message == "" {
			message = "upstream error"
		}
		retryAfter = ue.RetryAfter
	case errors.Is(err, registry.ErrModelNotFound):
		status, code = http.StatusBadRequest, "model_not_found"
		message = err.Error() + "; available: " + strings.Join(s.reg.Models(), ", ")
	case errors.Is(err, upstream.ErrAuthRejected):
		status, code = http.StatusBadGateway, "upstream_auth_rejected"
		message = err.Error()
	case errors.Is(err, upstream.ErrWaitingRoom):
		status, code = http.StatusServiceUnavailable, "waiting_room_queued"
		message = err.Error()
	case errors.As(err, &cbe):
		status, code = http.StatusForbidden, "country_blocked"
		message = cbe.Error()
	case errors.Is(err, upstream.ErrFreeModeCLIRequired):
		status, code = http.StatusForbidden, "free_mode_cli_required"
		message = err.Error()
	case errors.As(err, &ce):
		// 402 "Out of credits": surfacing the upstream body verbatim keeps
		// the quota detail (limit/recent/reset) for the client.
		status, code = http.StatusPaymentRequired, "out_of_credits"
		message = ce.Body
		if message == "" {
			message = "out of credits"
		}
	case errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusGatewayTimeout, "upstream_timeout"
		message = "upstream request timed out: " + err.Error()
	}

	attrs := []any{"status", status, "code", code, "err", err}
	if r != nil {
		if reqID := reqIDFrom(r.Context()); reqID != "" {
			attrs = append(attrs, "req_id", reqID)
		}
	}
	if retryAfter > 0 {
		attrs = append(attrs, "retry_after", int(retryAfter.Seconds()))
	}
	if !resetAt.IsZero() {
		attrs = append(attrs, "reset_at", resetAt.UTC().Format(time.RFC3339))
	}
	if lease != nil {
		attrs = append(attrs, "token", tokenLabel(lease))
	}
	if model != "" {
		attrs = append(attrs, "model", model)
	}

	if code == "rate_limited" {
		// D6 dedupe: identical (token, code, window) WARNs fire on the 1st +
		// every 50th; the counter always increments and the response is
		// always written.
		key := tokenLabel(lease) + "|" + code + "|" + window
		if !rateLimitWarnShouldLog(key) {
			if isAnthropicRequest(r) {
				s.writeAnthropicError(w, r, status, message, code, retryAfter)
			} else {
				s.writeJSONError(w, status, message, "upstream_error", code, retryAfter)
			}
			return
		}
	}
	s.logger.Warn("request failed", attrs...)
	if isAnthropicRequest(r) {
		s.writeAnthropicError(w, r, status, message, code, retryAfter)
	} else {
		s.writeJSONError(w, status, message, "upstream_error", code, retryAfter)
	}
}
