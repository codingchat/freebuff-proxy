package session

import (
	"log/slog"
	"time"

	"freebuff-proxy/internal/telemetry"
	"freebuff-proxy/internal/upstream"
)

// modelUnavailableEntry is one cached model_unavailable refusal (issue
// #158): until is when the proxy stops skipping admissions for the model
// (the earlier of the parsed availability window's next opening and
// now+MODEL_UNAVAILABLE_CACHE_TTL); window is the raw availableHours
// string for diagnostics.
type modelUnavailableEntry struct {
	until  time.Time
	window string
}

// SetModelUnavailableCacheTTL configures how long a model_unavailable
// refusal is remembered per model (issue #158, MODEL_UNAVAILABLE_CACHE_TTL
// default 1h): within the TTL (or until the parsed availability window
// re-opens, whichever is sooner) admissions for that model short-circuit to
// the fallback without the 409 roundtrip. d <= 0 disables the cache.
// Wired by the pool; safe to call at runtime.
func (m *Manager) SetModelUnavailableCacheTTL(d time.Duration) {
	m.mu.Lock()
	m.unavailableTTL = d
	m.mu.Unlock()
}

// recordModelUnavailable caches a model_unavailable admission refusal for
// model (issue #158). The skip window ends at the earlier of the parsed
// availability window's next opening and now+unavailableTTL, so a model
// known to re-open shortly stops being skipped at the right instant;
// unparseable windows (or a refusal inside the parsed window — a transient
// or upstream-side state) fall back to the plain TTL. Expired entries are
// pruned opportunistically so the map stays bounded. Caller need not hold
// mu.
func (m *Manager) recordModelUnavailable(model string, w *upstream.AvailabilityWindow, raw string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailableTTL <= 0 || model == "" {
		return
	}
	now := m.now()
	until := now.Add(m.unavailableTTL)
	if w != nil && !w.AvailableAt(now) {
		if next := w.NextStart(now); next.After(now) && next.Before(until) {
			until = next
		}
	}
	if m.modelUnavailable == nil {
		m.modelUnavailable = make(map[string]modelUnavailableEntry)
	}
	m.modelUnavailable[model] = modelUnavailableEntry{until: until, window: raw}
	for k, e := range m.modelUnavailable {
		if !e.until.After(now) && k != model {
			delete(m.modelUnavailable, k)
		}
	}
}

// modelUnavailableUntil returns when the skip window for model ends (issue
// #158); ok is false when the model is not cached as unavailable or the
// entry has expired. Never skips the guaranteed-available fallback model.
func (m *Manager) modelUnavailableUntil(model string, now time.Time) (time.Time, bool) {
	if model == "" || model == DefaultFallbackModel {
		return time.Time{}, false
	}
	m.mu.Lock()
	e, ok := m.modelUnavailable[model]
	m.mu.Unlock()
	if !ok || !e.until.After(now) {
		return time.Time{}, false
	}
	return e.until, true
}

// modelUnavailableShortCircuit applies the issue #158 skip in place: when
// *target is cached as unavailable it is rewritten to the fallback model and
// true is returned when the caller must stop immediately (a usable fallback
// session was reused with zero upstream calls). Without reuse the cached row
// is dropped (mirroring the 409 path) so the fallback admission runs fresh.
// Every skip is counted on /metrics as
// freebuff_proxy_model_unavailable_skips_total. Logs at DEBUG — the
// frequent path; a real 409 is now rare (once per TTL per model).
func (m *Manager) modelUnavailableShortCircuit(target *string) bool {
	skipUntil, ok := m.modelUnavailableUntil(*target, m.now())
	if !ok {
		return false
	}
	m.mu.Lock()
	cached := m.state
	reuse := cached != nil && cached.model == DefaultFallbackModel && sessionUsable(cached)
	if !reuse {
		// Mirror the 409 path: the cached row belongs to the unavailable
		// model (or is unusable) — drop it so the fallback admission runs
		// fresh.
		m.commit(nil)
	}
	m.mu.Unlock()
	telemetry.RecordModelUnavailableSkip()
	if reuse {
		slog.Debug("session: model cached unavailable, reusing fallback session",
			"requested", *target, "model", cached.model, "skip_until", skipUntil.Format(time.RFC3339))
	} else {
		slog.Debug("session: model cached unavailable, falling back without admission",
			"requested", *target, "fallback", DefaultFallbackModel, "skip_until", skipUntil.Format(time.RFC3339))
	}
	*target = DefaultFallbackModel
	return reuse
}
