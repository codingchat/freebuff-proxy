// scarce.go — scarce-model allocation and session-quota protection (issue #155).
package pool

import (
	"fmt"
	"time"

	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// scarceSwitchLead is how long before a scarce session expires that a request
// for a DIFFERENT model is allowed to switch away from it. A request arriving
// earlier than this leaves the scarce session running so the irreplaceable
// 1-session/day allocation is not burned.
const scarceSwitchLead = time.Minute

// ScarceSessionError is returned when all eligible tokens are busy running
// active scarce-model sessions (deepseek-v4-pro, gpt-5.6-luna) for a different
// model that cannot be released without burning irreplaceable daily quota.
// Surfaced as 503 Service Unavailable with Retry-After matching the earliest
// expiry time.
type ScarceSessionError struct {
	Model     string
	ExpiresAt time.Time
}

func (e *ScarceSessionError) Error() string {
	return fmt.Sprintf("scarce session (%s) in use until %s", e.Model, e.ExpiresAt.Format(time.RFC3339))
}

// scarceHeld reports whether the token's cached session is an active scarce
// model with more than scarceSwitchLead remaining — a request for a different
// model must not evict or switch away from it.
func scarceHeld(snap session.SessionSnapshot, requested string, scarce map[string]bool) bool {
	if snap.Status != "active" || snap.Model == "" || snap.Model == requested {
		return false
	}
	if !scarce[snap.Model] {
		return false
	}
	return !snap.ExpiresAt.IsZero() && time.Until(snap.ExpiresAt) > scarceSwitchLead
}

// scarceActive reports whether the token's cached session is an active scarce
// model with any remaining lifetime (greater than 0). Used by bridge idle
// eviction and shutdown teardown to keep the session alive.
func scarceActive(snap session.SessionSnapshot, scarce map[string]bool) bool {
	if snap.Status != "active" || snap.Model == "" {
		return false
	}
	if !scarce[snap.Model] {
		return false
	}
	return !snap.ExpiresAt.IsZero() && time.Until(snap.ExpiresAt) > 0
}

// scarceModelSet builds a fast lookup map from the configured scarce models.
func scarceModelSet(models []string) map[string]bool {
	if len(models) == 0 {
		return nil
	}
	out := make(map[string]bool, len(models))
	for _, m := range models {
		if m != "" {
			out[m] = true
		}
	}
	return out
}

// bridgeQuotaRemaining reports the bridge entry's session-quota state for model
// from its last admission (mirrors quotaRemaining in quota.go).

// isQuotaExhaustedError reports whether rle represents a session quota exhaustion
// (recentCount >= limit or local session quota error) as opposed to a transient rate limit.
func isQuotaExhaustedError(rle *upstream.RateLimitError) bool {
	if rle == nil {
		return false
	}
	if rle.Limit > 0 && rle.RecentCount >= rle.Limit {
		return true
	}
	if rle.Body == "session quota exhausted for model" {
		return true
	}
	return false
}
func bridgeQuotaRemaining(entry *bridgeEntry, model string) (known bool, remaining float64, capped bool) {
	q, ok := entry.session.Snapshot().QuotaByModel[model]
	if !ok || q.Limit <= 0 {
		return false, 0, false
	}
	resetFuture := !q.ResetAt.IsZero() && q.ResetAt.After(time.Now())
	if resetFuture && q.RecentCount >= q.Limit {
		return false, 0, true
	}
	if q.RecentCount < q.Limit {
		return true, q.Limit - q.RecentCount, false
	}
	return false, 0, false
}

// bridgeQuotaCapped reports whether the bridge entry's session quota is capped.
func bridgeQuotaCapped(entry *bridgeEntry, model string) bool {
	_, _, capped := bridgeQuotaRemaining(entry, model)
	return capped
}

// bridgeQuotaLimitError builds the 429 RateLimitError for a quota-capped bridge entry.
func bridgeQuotaLimitError(entry *bridgeEntry, model string) *upstream.RateLimitError {
	q := entry.session.Snapshot().QuotaByModel[model]
	retryAfter := time.Duration(0)
	if !q.ResetAt.IsZero() && q.ResetAt.After(time.Now()) {
		retryAfter = time.Until(q.ResetAt)
	}
	return &upstream.RateLimitError{
		Status:      "rate_limited",
		RetryAfter:  retryAfter,
		Limit:       q.Limit,
		RecentCount: q.RecentCount,
		ResetAt:     q.ResetAt,
		Body:        "session quota exhausted for model",
	}
}
