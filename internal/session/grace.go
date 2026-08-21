package session

import "time"

// Grace-window handover helpers (issue #163): when a session is being
// ridden through its 30-minute grace drain (gap #13) — a long stream that
// crossed the expiresAt boundary — the next request should hand over to a
// fresh session without paying a synchronous admission (waiting room) at
// grace end. These helpers decide WHEN that background re-admit fires;
// the riding/refusal semantics live in refresh (preemptive) and
// EnsureSessionForModel (once-per-expiry guard).

// graceEndsAt returns the cached session's grace-window end: the upstream
// gracePeriodEndsAt when the response carried it, else expiresAt +
// graceWindow (the 30-minute drain fallback, gap #13). Zero when neither
// is known — the row has no computable grace window.
func graceEndsAt(s *cachedState) time.Time {
	if s == nil {
		return time.Time{}
	}
	graceEnd := s.gracePeriodEndsAt
	if graceEnd.IsZero() && !s.expiresAt.IsZero() {
		graceEnd = s.expiresAt.Add(graceWindow)
	}
	return graceEnd
}

// reAdmitDue reports whether a pre-emptive re-admit should fire for the
// cached session right now (issue #99/#163). It is the union of two
// windows, each gated once-per-expiry by the caller:
//
//   - the pre-expiry lead window: an active session within lead of
//     expiresAt-5s — the session is about to close, so a background
//     admission lands a fresh instance for the next request;
//   - the grace drain (issue #163): the session is usable ONLY through
//     its grace window (the active window has closed, grace still open) —
//     a long stream that crossed the expiry boundary must hand over to a
//     fresh session without the next request paying a synchronous
//     admission (waiting room) at grace end.
//
// The caller keeps the old row authoritative while the refresh runs
// preemptively: a refusal or queue (the upstream still considers the old
// session active) rides on without invalidating the cache.
func reAdmitDue(s *cachedState, lead time.Duration, now time.Time) bool {
	if s == nil || s.instanceID == "" {
		return false
	}
	leadEnd := s.expiresAt.Add(-expiryMargin)
	if s.status == "active" && lead > 0 && now.Before(leadEnd) && leadEnd.Sub(now) <= lead {
		return true
	}
	graceEnd := graceEndsAt(s)
	return !graceEnd.IsZero() && !now.Before(leadEnd) && now.Before(graceEnd)
}
