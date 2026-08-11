// Package session implements the FreeBuff free-session lifecycle for a
// single token: create, poll, cache, invalidate, and end — with a
// single-flight refresh so concurrent callers share one upstream request.
//
// Semantics ported from proxy-freebuff lib/sessions.js and
// freebuff2api-quorinex free_session.go:
//   - active: ready until expiresAt-5s
//   - disabled: no instance id needed; proceed without one
//   - queued: waiting room; callers get WaitingRoomError until pollAt
//   - ended/superseded/none: transparently re-created
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"freebuff-proxy/internal/upstream"
)

const (
	// expiryMargin is subtracted from expiresAt before a session is
	// considered ready (mirrors the references' 5s safety margin).
	expiryMargin = 5 * time.Second
	// maxRefreshIterations bounds the create/poll status loop.
	maxRefreshIterations = 5
	// maxOuterIterations bounds EnsureSession's refresh attempts per call so
	// a pathological upstream (always-expired or never-advancing queue)
	// cannot spin forever.
	maxOuterIterations = 5
)

// WaitingRoomError is returned when the session is queued and pollAt has not
// passed. Callers should surface it as 503 with Retry-After.
type WaitingRoomError struct {
	Position   int
	QueueDepth int
	RetryAfter time.Duration
}

func (e *WaitingRoomError) Error() string {
	return fmt.Sprintf("waiting room: position %d of %d, retry after %s", e.Position, e.QueueDepth, e.RetryAfter)
}

// Manager owns the cached session state for one token.
type Manager struct {
	client *upstream.Client

	mu         sync.Mutex
	state      *cachedState
	refreshCh  chan struct{} // closed by the in-flight refresher when done
	refreshing bool
}

type cachedState struct {
	status             string
	instanceID         string
	expiresAt          time.Time
	position           int
	queueDepth         int
	pollAt             time.Time
	accessTier         string
	countryCode        string
	countryBlockReason string
}

// NewManager builds a session manager for the given upstream client.
func NewManager(client *upstream.Client) *Manager {
	return &Manager{client: client}
}

// EnsureSession returns the session instance id, or "" when the upstream
// session is disabled (requests proceed without an instance header). It
// returns WaitingRoomError while the session is queued and pollAt has not
// passed. Concurrent callers share a single in-flight refresh. The fast path
// reuses cached state while it is fresh; stale state (or none) triggers one
// refresh cycle, after which the freshly returned state is trusted — the
// expiry margin exists to avoid refreshing on every call, not to gate usage.
func (m *Manager) EnsureSession(ctx context.Context) (string, error) {
	for attempts := 0; attempts < maxOuterIterations; attempts++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		m.mu.Lock()
		s := m.state
		if s != nil && !m.refreshing {
			switch s.status {
			case "active":
				if time.Now().Before(s.expiresAt.Add(-expiryMargin)) {
					instance := s.instanceID
					m.mu.Unlock()
					slog.Debug("session reused", "instance_id", instance, "expires_at", s.expiresAt.Format(time.RFC3339))
					return instance, nil
				}
				// Freshness exceeded — fall through to refresh.
			case "disabled":
				m.mu.Unlock()
				return "", nil
			case "queued":
				if now := time.Now(); now.Before(s.pollAt) {
					wa := WaitingRoomError{
						Position:   s.position,
						QueueDepth: s.queueDepth,
						RetryAfter: s.pollAt.Sub(now),
					}
					m.mu.Unlock()
					return "", &wa
				}
				// pollAt passed — fall through to refresh and advance.
			}
		}
		singleFlight := m.refreshing
		// Capture the channel under the lock: the refresher clears
		// m.refreshCh (sets it to nil) when it finishes, and an unlocked
		// read here would race with that write — and could read nil, which
		// would block the select forever.
		var refreshCh chan struct{}
		if singleFlight {
			refreshCh = m.refreshCh
		} else {
			m.refreshing = true
			refreshCh = make(chan struct{})
			m.refreshCh = refreshCh
		}
		m.mu.Unlock()

		if singleFlight {
			select {
			case <-refreshCh:
				continue // loop re-evaluates cached state
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		// We are the refresher. Run the loop outside the lock.
		err := m.refresh(ctx)
		m.mu.Lock()
		m.refreshing = false
		close(m.refreshCh)
		m.refreshCh = nil
		m.mu.Unlock()
		if err != nil {
			return "", err
		}

		// Freshly refreshed: trust the new state (the expiry margin gates
		// only the cached fast path).
		m.mu.Lock()
		s = m.state
		m.mu.Unlock()
		if s == nil {
			continue // ended/superseded cleared it; refresh again
		}
		switch s.status {
		case "active":
			return s.instanceID, nil
		case "disabled":
			return "", nil
		case "queued":
			if now := time.Now(); now.Before(s.pollAt) {
				return "", &WaitingRoomError{
					Position:   s.position,
					QueueDepth: s.queueDepth,
					RetryAfter: s.pollAt.Sub(now),
				}
			}
			// Still queued with pollAt passed — bounded retry via the loop.
		}
	}
	return "", errors.New("session: not ready after repeated refreshes")
}

// refresh runs the create/poll status loop, updating cached state, until the
// session is active, disabled, or the iteration budget is exhausted.
func (m *Manager) refresh(ctx context.Context) error {
	for i := 0; i < maxRefreshIterations; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		m.mu.Lock()
		cached := m.state
		m.mu.Unlock()

		var (
			st  *upstream.SessionState
			err error
		)
		if cached != nil && cached.status == "queued" && cached.instanceID != "" {
			st, err = m.client.GetSession(ctx, cached.instanceID)
		} else {
			st, err = m.client.CreateSession(ctx)
		}
		if err != nil {
			return err
		}

		status := st.Status
		switch status {
		case "active":
			m.mu.Lock()
			m.state = &cachedState{
				status:             "active",
				instanceID:         st.InstanceID,
				expiresAt:          st.ExpiresAt,
				accessTier:         st.AccessTier,
				countryCode:        st.CountryCode,
				countryBlockReason: st.CountryBlockReason,
			}
			m.mu.Unlock()
			slog.Debug("session created", "status", "active", "instance_id", st.InstanceID,
				"expires_at", st.ExpiresAt.Format(time.RFC3339))
			return nil
		case "disabled":
			m.mu.Lock()
			m.state = &cachedState{status: "disabled"}
			m.mu.Unlock()
			slog.Debug("session created", "status", "disabled", "instance_id", "")
			return nil
		case "queued":
			pollAt := st.PollAt
			if pollAt.IsZero() {
				wait := time.Duration(st.EstimatedWaitMs) * time.Millisecond
				if wait < time.Second {
					wait = time.Second
				}
				if wait > 5*time.Second {
					wait = 5 * time.Second
				}
				pollAt = time.Now().Add(wait)
			}
			m.mu.Lock()
			m.state = &cachedState{
				status:     "queued",
				instanceID: st.InstanceID,
				position:   st.Position,
				queueDepth: st.QueueDepth,
				pollAt:     pollAt,
			}
			m.mu.Unlock()
			slog.Debug("session queued", "instance_id", st.InstanceID,
				"position", st.Position, "queue_depth", st.QueueDepth, "poll_at", pollAt.Format(time.RFC3339))
			// Surface the queue to the caller: EnsureSession re-evaluates the
			// cached state and returns WaitingRoomError until pollAt passes.
			// Polling resumes on the next call, mirroring the references.
			return nil
		case "ended", "superseded", "none":
			// Recreate on the next iteration.
			m.mu.Lock()
			m.state = nil
			m.mu.Unlock()
			slog.Debug("session recreated", "reason", status, "instance_id", st.InstanceID)
		case "banned":
			return fmt.Errorf("session: account banned upstream")
		case "country_blocked":
			return fmt.Errorf("session: country blocked upstream")
		case "rate_limited":
			return fmt.Errorf("session: rate limited upstream")
		case "model_locked":
			// Session is bound to a different model; recreate for ours.
			m.mu.Lock()
			m.state = nil
			m.mu.Unlock()
			slog.Debug("session recreated", "reason", "model_locked")
		default:
			return fmt.Errorf("session: unknown upstream status %q", status)
		}
	}
	return errors.New("session: refresh iteration budget exhausted")
}

// SessionSnapshot is a lock-free best-effort view of the cached session
// state, for healthz-style reporting (pool.TokenSnapshot).
type SessionSnapshot struct {
	Status             string
	InstanceID         string
	QueuePosition      int
	QueueDepth         int
	TierAccess         string
	TierCountry        string
	CountryBlockReason string
}

// Snapshot returns a best-effort view of the cached session state. All
// fields may be zero when no session has been created yet. Added for
// internal/pool snapshotting; no upstream calls are made.
func (m *Manager) Snapshot() SessionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		return SessionSnapshot{}
	}
	return SessionSnapshot{
		Status:             m.state.status,
		InstanceID:         m.state.instanceID,
		QueuePosition:      m.state.position,
		QueueDepth:         m.state.queueDepth,
		TierAccess:         m.state.accessTier,
		TierCountry:        m.state.countryCode,
		CountryBlockReason: m.state.countryBlockReason,
	}
}

// Invalidate drops the cached session so the next EnsureSession re-creates
// it. Used when a chat request reports a session-level error.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	instanceID := ""
	if m.state != nil {
		instanceID = m.state.instanceID
	}
	m.state = nil
	m.mu.Unlock()
	slog.Debug("session invalidated", "instance_id", instanceID)
}

// EndSession deletes the upstream session (if any) and clears the cache.
func (m *Manager) EndSession(ctx context.Context) error {
	m.mu.Lock()
	instanceID := ""
	if s := m.state; s != nil {
		instanceID = s.instanceID
	}
	m.state = nil
	m.mu.Unlock()

	if instanceID == "" {
		return nil
	}
	slog.Debug("session ended", "instance_id", instanceID)
	if err := m.client.EndSession(ctx, instanceID); err != nil && !errors.Is(err, upstream.ErrSessionInvalid) {
		return err
	}
	return nil
}
