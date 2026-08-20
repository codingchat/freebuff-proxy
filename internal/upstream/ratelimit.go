// Package upstream implements the codebuff.com wire protocol for a single token.
package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// countRateLimitEvent increments the per-code rate-limit ledger (T7). The
// map entry is created lazily so clients built without the constructor
// (tests, bridge entries) still record safely.
func (c *Client) countRateLimitEvent(code string) {
	c.rateLimitMu.Lock()
	ctr := c.rateLimitEvents[code]
	if ctr == nil {
		ctr = &atomic.Int64{}
		if c.rateLimitEvents == nil {
			c.rateLimitEvents = make(map[string]*atomic.Int64)
		}
		c.rateLimitEvents[code] = ctr
	}
	c.rateLimitMu.Unlock()
	ctr.Add(1)
}

// RateLimitEvents returns a copy of this client's per-code rate-limit
// classification counters (pool snapshot /metrics aggregation).
func (c *Client) RateLimitEvents() map[string]int64 {
	c.rateLimitMu.Lock()
	defer c.rateLimitMu.Unlock()
	out := make(map[string]int64, len(c.rateLimitEvents))
	for code, ctr := range c.rateLimitEvents {
		out[code] = ctr.Load()
	}
	return out
}

// classify maps an upstream error response through the shared matrix and
// records the 428 waiting_room_required flag (issue #94) so the pool can
// fire the gated pre-session chain before the next session create. All
// in-client error paths must use this wrapper; the free classifyError stays
// pure for tests.
func (c *Client) classify(status int, body string, hdr http.Header) error {
	err := classifyError(status, body, hdr)
	// classifyError returns a concrete typed error in the interface, never a
	// nil interface — so err is always non-nil; test the sentinel directly.
	if errors.Is(err, ErrWaitingRoomRequired) {
		c.waitingRoomRequired.Store(true)
	}
	// T7 ledger: count every rate-limit-family classification by its
	// upstream body code and surface one Debug line carrying the FULL
	// (redacted) body, so the distinct refusal codes (free_mode_rate_limited,
	// insufficient_quota, limit_burst_rate, ip_capped, spend_limited,
	// rate_limited, ...) are distinguishable in logs before the #133
	// behavior fix lands.
	if code, window := rateLimitInfo(body, err); code != "" {
		c.countRateLimitEvent(code)
		logRateLimitClassified(status, body, code, window, err)
	}
	return err
}

// classifyError maps an upstream error response to the recovery matrix.
func classifyError(status int, body string, hdr http.Header) error {
	lower := strings.ToLower(body)
	retryAfter := parseRetryAfter(hdr)

	switch {
	case status == http.StatusForbidden && strings.Contains(lower, `"status":"banned"`):
		// The canonical ban body is {"status":"banned"} (the free-session
		// status wire shape, reference/freebuff freebuff-session.ts). Match
		// the marker exactly: any 403 whose body merely mentions the word
		// "banned" (e.g. {"error":"model temporarily banned..."}) must stay
		// a generic 403, not trigger the long ban cooldown. (Audit B5.)
		return parseBan(body)
	case strings.Contains(lower, "deployment_outside_hours"):
		// Free tier is outside its operating hours: temporarily unavailable
		// but worth a later retry. Checked before the status-driven 503/429
		// cases because upstream can attach it to any status (reference:
		// freebuff-reverse adapter.go classifies it Retryable by body first).
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter, Retryable: true}
	case containsAny(lower, "free_mode_capacity_deferred"):
		// Free-tier transient capacity queue: upstream says "your request
		// will be retried automatically" and a same-session retry recovers
		// immediately. Retryable transport-level condition handled under the
		// TRANSIENT_RETRIES budget in ChatCompletions against the SAME
		// lease/session — never a token cooldown, never a session
		// invalidation (reference/freebuff-proxy-hengxin proxy.js:652-668).
		return &CapacityDeferredError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter}
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %d %s", ErrAuthRejected, status, truncate(body, 200))
	case status == http.StatusServiceUnavailable:
		return &WaitingRoomError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case status == http.StatusPaymentRequired:
		return &CreditsError{Status: status, Body: truncate(body, 200)}
	case status == http.StatusConflict && containsAny(lower, "session_limit_reached"):
		// 409 session_limit_reached: the ACCOUNT is over its concurrent-tab
		// budget, but this session's row is fine (endsTheSession:false).
		// Distinct non-invalid error: the server surfaces 409 and never
		// refreshes/recreates the session
		// (reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		return &SessionLimitError{Status: status, Body: truncate(body, 200)}
	case status == http.StatusForbidden && strings.Contains(lower, "free_mode_cli_required"):
		return fmt.Errorf("%w: %d %s", ErrFreeModeCLIRequired, status, truncate(body, 200))
	case status == http.StatusForbidden && strings.Contains(lower, "country_blocked"):
		return parseCountryBlock(body)
	case containsAny(lower, "ip_capped"):
		// 429 ip_capped: too many DISTINCT users on the egress IP.
		// Admission-only — existing sessions keep running, so unlike
		// rate_limited this is NOT tied to a quota reset. Cooldown is
		// bounded by the proxy to retryAfterMs + jitter, with a per-token
		// daily re-admission cap (3rd hit in a rolling window locks until
		// Pacific midnight — #118) (reference/freebuff freebuff-session.ts).
		return parseIpCapped(body, retryAfter)
	case containsAny(lower, "waiting_room_queued"):
		// 429 waiting_room_queued: transient admission race — the session
		// row was caught mid-admit (endsTheSession:false). NOT session
		// invalid: the row is fine, so the cached session must not be
		// invalidated or refreshed. Surfaced as 503 waiting_room_queued +
		// Retry-After via the shared WaitingRoomError
		// (reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		return &WaitingRoomError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case containsAny(lower, "waiting_room_required"):
		// 428 waiting_room_required (issue #94): the account must walk the
		// reference pre-session ad-chain + streak flow before the next
		// session create. Own retryable signal (Retry-After honored, no
		// cooldown) — deliberately NOT ErrSessionInvalid: the session row is
		// fine, so nothing must be invalidated (reference
		// freebuff2api-optimized codebuff.py:1048-1074). The body marker is
		// the discriminator (upstream can attach it to 428/429 alike); the
		// Client.classify wrapper records the flag so the pool can fire the
		// gated WAITING_ROOM_CHAIN before the next create.
		return &WaitingRoomRequiredError{RetryAfter: retryAfter, Detail: truncate(body, 200)}
	case containsAny(lower, "session_model_mismatch") && containsAny(lower, "limited"):
		// The egress IP cannot serve the requested model (e.g. "Limited free
		// access is only available with DeepSeek V4 Flash or MiMo 2.5." or
		// "model <id> is limited on this IP"). The session row is fine — it
		// stays bound to its admitted model — so this is NOT session-invalid:
		// invalidating would re-admit and burn a daily session slot. The server
		// marks the refusal and the pool registry cools the (egress, model)
		// pairing instead.
		return &LimitedIpError{RetryAfter: retryAfter, Body: truncate(body, 200)}
	case containsAny(lower, "session_superseded"):
		// #119: 409 session_superseded is a TERMINAL gate rejection
		// (endsTheSession:true — another instance took over the account;
		// reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
		// Deliberately NOT ErrSessionInvalid: the server must never
		// auto-reacquire in-request (auto-takeover risks ping-pong) — it
		// surfaces 409 session_superseded and lets the NEXT request re-join
		// fresh (send-message.ts handleFreebuffGateError marks the session
		// superseded and stops polling; use-freebuff-session.ts
		// nextDelayMs returns null).
		return &SessionSupersededError{Status: status, Body: truncate(body, 200)}
	case containsAny(lower, "freebuff_update_required",
		"session_expired", "session_model_mismatch", "model_locked"):
		return fmt.Errorf("%w: %s%s", ErrSessionInvalid, truncate(body, 200), retryDetail(retryAfter))
	case status == http.StatusBadRequest && containsAny(lower, "runid not found", "runid not running"):
		return fmt.Errorf("%w: %s", ErrRunInvalid, truncate(body, 200))
	case status == http.StatusTooManyRequests && containsAny(lower, "insufficient_quota", "limit_burst_rate"):
		// #133: upstream load saturation ("The current group's upstream
		// load is saturated, please try again later"). No Retry-After in
		// the body — parseRateLimit would lock the token until Pacific
		// midnight on what is a minutes-scale transient. Bounded cooldown,
		// distinct code, no midnight lock.
		return &RateLimitError{
			Status:     "load_shedding",
			RetryAfter: LoadShedCooldown,
			Body:       truncate(body, 200),
		}
	case status == http.StatusTooManyRequests && containsAny(lower, "peak hours"):
		// #133: "Usage is temporarily limited during peak hours, when
		// upstream model prices double…". The peak end is unknowable from
		// the body: bounded conservative cooldown instead of locking the
		// token until Pacific midnight (the peak is hours, not a day).
		return &RateLimitError{
			Status:     "peak_hours",
			RetryAfter: PeakHoursCooldown,
			Body:       truncate(body, 200),
		}
	case status == http.StatusTooManyRequests || containsAny(lower, "rate_limited", "spend_limited"):
		return parseRateLimit(body, parseRetryAfter(hdr))
	default:
		return &UpstreamError{Status: status, Body: truncate(body, 500), RetryAfter: retryAfter}
	}
}

// rateLimitInfo derives the T7 ledger code and window for a rate-limit
// classification. The classification must be in the rate-limit error family
// (RateLimitError/IpCappedError/CapacityDeferredError) — 403 bans, 401 auth
// refusals, waiting rooms and other gates never count; code is empty then
// and nothing is logged.
func rateLimitInfo(body string, err error) (code, window string) {
	switch err.(type) {
	case *RateLimitError, *IpCappedError, *CapacityDeferredError:
	default:
		return "", ""
	}
	code = rateLimitCode(body, err)
	if code == "" {
		return "", ""
	}
	return code, rateLimitWindow(body, err)
}

// rateLimitCode extracts the upstream refusal code from the body's
// "error"/"type" field (free_mode_rate_limited, insufficient_quota,
// limit_burst_rate, ip_capped, spend_limited, rate_limited, ...), falling
// back to the classified error type when the body carries no code key.
func rateLimitCode(body string, err error) string {
	if code := bodyCode(body); code != "" {
		return code
	}
	switch e := err.(type) {
	case *CapacityDeferredError:
		return "free_mode_capacity_deferred"
	case *IpCappedError:
		return "ip_capped"
	case *RateLimitError:
		if e.Status != "" {
			return e.Status // load_shedding | peak_hours
		}
		return "rate_limited"
	}
	return ""
}

// bodyCode reads the first non-empty "error":"X" or "type":"X" string from a
// JSON error body (the ledger's code source).
func bodyCode(body string) string {
	var raw struct {
		Error string `json:"error"`
		Type  string `json:"type"`
	}
	if json.Unmarshal([]byte(body), &raw) != nil {
		return ""
	}
	if raw.Error != "" {
		return raw.Error
	}
	return raw.Type
}

// rateLimitWindow maps a rate-limit classification to the shared window
// table: the body's own "1 minute"/"30 minutes" text when present, else
// "reset" when the error carries a reset timestamp, else "retry-after" when
// it carries a retry delay, else "none".
func rateLimitWindow(body string, err error) string {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "1 minute") {
		return "1 minute"
	}
	if strings.Contains(lower, "30 minutes") {
		return "30 minutes"
	}
	switch e := err.(type) {
	case *RateLimitError:
		if !e.ResetAt.IsZero() {
			return "reset"
		}
		if e.RetryAfter > 0 {
			return "retry-after"
		}
	case *IpCappedError:
		if e.RetryAfter > 0 {
			return "retry-after"
		}
	case *CapacityDeferredError:
		if e.RetryAfter > 0 {
			return "retry-after"
		}
	}
	return "none"
}

// rateLimitFields extracts the retry-after delay and reset timestamp a
// rate-limit-family error carries, for the classification Debug line.
func rateLimitFields(err error) (time.Duration, time.Time) {
	switch e := err.(type) {
	case *RateLimitError:
		return e.RetryAfter, e.ResetAt
	case *IpCappedError:
		return e.RetryAfter, time.Time{}
	case *CapacityDeferredError:
		return e.RetryAfter, time.Time{}
	}
	return 0, time.Time{}
}

// logRateLimitClassified emits the T7 ledger Debug line. The body is logged
// in FULL (the 200-rune truncation applies to the HTTP error response only)
// and must already be redacted by the caller.
func logRateLimitClassified(status int, body, code, window string, err error) {
	attrs := []any{
		"status", status,
		"code", code,
		"window", window,
		"body", body,
	}
	if retryAfter, resetAt := rateLimitFields(err); retryAfter > 0 {
		attrs = append(attrs, "retry_after", int(retryAfter.Seconds()))
		if !resetAt.IsZero() {
			attrs = append(attrs, "reset_at", resetAt.UTC().Format(time.RFC3339))
		}
	}
	slog.Debug("upstream rate limit classified", attrs...)
}

// errClassName names the classified error type for the `upstream response`
// debug line (T5). Wrapped sentinel errors (auth/session/run refusals built
// with fmt.Errorf) fall back to the generic upstream error class.
func errClassName(err error) string {
	switch err.(type) {
	case *RateLimitError:
		return "RateLimitError"
	case *IpCappedError:
		return "IpCappedError"
	case *BanError:
		return "BanError"
	case *CountryBlockedError:
		return "CountryBlockedError"
	case *CreditsError:
		return "CreditsError"
	case *SessionLimitError:
		return "SessionLimitError"
	case *SessionSupersededError:
		return "SessionSupersededError"
	case *LimitedIpError:
		return "LimitedIpError"
	case *CapacityDeferredError:
		return "CapacityDeferredError"
	case *WaitingRoomError:
		return "WaitingRoomError"
	case *WaitingRoomRequiredError:
		return "WaitingRoomRequiredError"
	case *UpstreamError:
		return "UpstreamError"
	}
	if err == nil {
		return ""
	}
	return "UpstreamError"
}

// parseCountryBlock builds a CountryBlockedError from a 403 country_blocked
// body, extracting countryCode/countryBlockReason/ipPrivacySignals
// best-effort (absent fields are tolerated).
func parseCountryBlock(body string) error {
	cbe := &CountryBlockedError{}
	var parsed struct {
		CountryCode        string   `json:"countryCode"`
		CountryBlockReason string   `json:"countryBlockReason"`
		IpPrivacySignals   []string `json:"ipPrivacySignals"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		cbe.CountryCode = parsed.CountryCode
		cbe.CountryBlockReason = parsed.CountryBlockReason
		cbe.IpPrivacySignals = parsed.IpPrivacySignals
	}
	return cbe
}

// NextPacificMidnight returns the upcoming 00:00 Pacific Time in UTC
// (which is 07:00 UTC during PDT / 08:00 UTC during PST).
func NextPacificMidnight() time.Time {
	loc, err := time.LoadLocation("America/Los_Angeles")
	now := time.Now()
	if err != nil {
		return pacificMidnightFallback(now)
	}
	nowLoc := now.In(loc)
	nextDay := time.Date(nowLoc.Year(), nowLoc.Month(), nowLoc.Day()+1, 0, 0, 0, 0, loc)
	return nextDay.UTC()
}

// pacificMidnightFallback approximates the upcoming Pacific midnight without
// the IANA tzdata database: America/Los_Angeles is UTC-7 during PDT
// (roughly March-November) and UTC-8 during PST (roughly November-March).
// The month range is the documented approximation; the exact DST transition
// dates require tzdata.
func pacificMidnightFallback(now time.Time) time.Time {
	hour := 7 // PDT
	if m := now.UTC().Month(); m < time.March || m > time.November {
		hour = 8 // PST: December, January, February
	}
	t := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), hour, 0, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

// parseIpCapped builds an IpCappedError from a 429 ip_capped body,
// extracting retryAfterMs/activeUsersForIp/limit best-effort (absent fields
// are tolerated). The ERROR's retryAfter stays bounded to the body's
// retryAfterMs (1m default) — ip_capped is admission-only and not a quota
// reset upstream, so the parse never fabricates a Pacific-midnight window;
// the proxy's bounded re-admission policy (full retryAfter + jitter, daily
// cap — #118) is applied by runs.CooldownIpCapped at cooldown time.
func parseIpCapped(body string, headerRetryAfter time.Duration) error {
	ice := &IpCappedError{Body: truncate(body, 200), RetryAfter: headerRetryAfter}

	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err == nil {
		target := raw
		if errObj, ok := raw["error"].(map[string]any); ok {
			target = errObj
		}

		if ms, ok := getNumber(target, "retryAfterMs", "retry_after_ms"); ok && ms > 0 {
			ice.RetryAfter = time.Duration(ms) * time.Millisecond
		} else if sec, ok := getNumber(target, "retryAfter", "retry_after"); ok && sec > 0 {
			ice.RetryAfter = time.Duration(sec * float64(time.Second))
		}

		if n, ok := getNumber(target, "activeUsersForIp", "active_users_for_ip"); ok {
			ice.ActiveUsersForIP = int(n)
		}
		if lim, ok := getNumber(target, "limit"); ok {
			ice.Limit = lim
		}
	}

	if ice.RetryAfter <= 0 {
		ice.RetryAfter = time.Minute
	}
	return ice
}

// isCapacityDeferred reports whether err is a free_mode_capacity_deferred
// response (the free tier's transient capacity queue).
func isCapacityDeferred(err error) bool {
	var cde *CapacityDeferredError
	return errors.As(err, &cde)
}

// LoadShedCooldown bounds a 429 load-saturation refusal (issue #133): the
// upstream sheds load for minutes, not a day, so the token re-probes after
// ~90s instead of being locked until Pacific midnight by the no-timestamp
// parseRateLimit default.
const LoadShedCooldown = 90 * time.Second

// PeakHoursCooldown bounds a 429 peak-hours refusal (issue #133): the peak
// window lasts hours and its end is not in the body; 30 minutes is a
// conservative floor that re-probes long before the daily-cap lock would
// have lifted.
const PeakHoursCooldown = 30 * time.Minute

// opaqueRateLimitBackoff bounds a 429 with no timestamp, no daily-reset
// signal, and no Retry-After header (issue #140 P2): a fully opaque body
// must never lock the token until Pacific midnight over a minutes-scale
// transient, so it gets the same bounded cooldown the other no-timestamp
// refusals get.
const opaqueRateLimitBackoff = 60 * time.Second

// parseRateLimit builds a RateLimitError from a 429 body, extracting
// retryAfterMs/resetAt/limit/recentCount/period best-effort across multiple
// JSON schemas. Falls back to the Retry-After header; a body with no
// timestamp/period and no header delay is bounded to opaqueRateLimitBackoff,
// except a genuine daily-cap body (resetAt or an at-cap daily/weekly period)
// which locks until the upcoming Pacific midnight (07:00 UTC).
func parseRateLimit(body string, headerRetryAfter time.Duration) error {
	rle := &RateLimitError{Body: truncate(body, 200), RetryAfter: headerRetryAfter}

	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err == nil {
		target := raw
		if errObj, ok := raw["error"].(map[string]any); ok {
			target = errObj
		}

		if ms, ok := getNumber(target, "retryAfterMs", "retry_after_ms"); ok && ms > 0 {
			rle.RetryAfter = time.Duration(ms) * time.Millisecond
		} else if sec, ok := getNumber(target, "retryAfter", "retry_after"); ok && sec > 0 {
			rle.RetryAfter = time.Duration(sec * float64(time.Second))
		}

		if t, ok := getTime(target, "resetAt", "reset_at", "resets_at", "resumes_at", "reset"); ok && !t.IsZero() {
			rle.ResetAt = t
		}

		if lim, ok := getNumber(target, "limit"); ok {
			rle.Limit = lim
		}
		if cnt, ok := getNumber(target, "recentCount", "recent_count"); ok {
			rle.RecentCount = cnt
		}
		if st, ok := target["status"].(string); ok {
			rle.Status = st
		}
		if period, ok := target["period"].(string); ok {
			rle.Period = period
		}
	}

	if !rle.ResetAt.IsZero() && rle.ResetAt.After(time.Now()) {
		if rle.RetryAfter <= 0 {
			rle.RetryAfter = time.Until(rle.ResetAt)
		}
	} else if rle.RetryAfter <= 0 {
		// No timestamp and no header delay: lock until the next Pacific
		// reset window (07:00 UTC) ONLY when the body signals a genuine
		// daily reset — a parsed resetAt (handled above) or a daily/weekly
		// quota period whose counter is at/over the limit (the daily-cap
		// bodies). Every other opaque 429 gets a bounded backoff so a
		// minutes-scale transient is never treated as a full-day lock
		// (issue #140 P2).
		if isDailyCapReset(rle) {
			nextReset := NextPacificMidnight()
			rle.ResetAt = nextReset
			rle.RetryAfter = time.Until(nextReset)
		} else {
			rle.RetryAfter = opaqueRateLimitBackoff
		}
	}

	if rle.RetryAfter <= 0 {
		rle.RetryAfter = 60 * time.Second
	}
	// T7 ledger window, computed after ResetAt/RetryAfter are finalized
	// (the daily-cap fallback above sets ResetAt so the window is "reset"
	// for daily-cap timestamp-less 429s; opaque ones carry just
	// RetryAfter → "retry-after").
	rle.Window = rateLimitWindow(body, rle)
	return rle
}

// isDailyCapReset reports whether a no-timestamp 429 body signals a genuine
// daily-cap reset: the quota period is pacific_day/pacific_week AND the
// recent counter is at/over the limit (the session-quota bodies the CLI
// serves on daily-cap refusals). Only these lock until the next Pacific
// midnight; truly opaque bodies get opaqueRateLimitBackoff.
func isDailyCapReset(rle *RateLimitError) bool {
	if rle.Period != "pacific_day" && rle.Period != "pacific_week" {
		return false
	}
	return rle.Limit > 0 && rle.RecentCount >= rle.Limit
}

// parseBan builds a BanError from a 403 banned body, extracting the
// resumes_at timestamp best-effort. resumes_at may be RFC3339, unix seconds,
// or unix milliseconds (parseFlexTime).
func parseBan(body string) error {
	be := &BanError{Body: truncate(body, 200)}
	var parsed struct {
		ResumesAt any    `json:"resumes_at"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil {
		if t, perr := parseFlexTime(parsed.ResumesAt); perr == nil {
			be.ResumesAt = t
		}
	}
	return be
}

// parseRetryAfter reads the Retry-After header (seconds or HTTP date).
func parseRetryAfter(hdr http.Header) time.Duration {
	raw := hdr.Get("Retry-After")
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		return time.Until(t)
	}
	return 0
}

// parseFlexTime accepts RFC3339, unix seconds, or unix milliseconds.
func parseFlexTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case nil:
		return time.Time{}, errors.New("nil time")
	case string:
		if t == "" {
			return time.Time{}, errors.New("empty time")
		}
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return parsed, nil
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed, nil
		}
		if secs, err := strconv.ParseInt(t, 10, 64); err == nil {
			return unixFrom(secs), nil
		}
		return time.Time{}, fmt.Errorf("unparseable time %q", t)
	case float64:
		return unixFrom(int64(t)), nil
	default:
		return time.Time{}, fmt.Errorf("unexpected time type %T", v)
	}
}
