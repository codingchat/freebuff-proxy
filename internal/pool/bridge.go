// bridge.go — bridge-mode acquire and maintenance.
//
// Bridge mode serves a single upstream account per client-supplied token
// (no AUTH_TOKENS configured). Each client token is lazily mapped to a
// bridgeEntry (upstream client + session manager + run manager) and
// cached for reuse with LRU eviction when the cache is full.
package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// tokenKey returns a 32-char hex string derived from the SHA-256 hash of the
// raw client token. Bridge map keys use this non-reversible form so raw tokens
// are never stored as map keys in memory (B3). The raw token is still held in
// bridgeEntry.token for upstream client creation.
func tokenKey(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:16])[:32]
}

// AcquireBridge acquires a lease for one client-supplied token in bridge
// mode (no AUTH_TOKENS configured). The entry — upstream client, session
// manager, and run manager — is created lazily on first use and cached for
// reuse across that client's later requests (least quota burn). There is no
// multi-token failover: a single token either yields a lease or its error
// is returned as-is. Registry misses pass through.
func (p *Pool) AcquireBridge(ctx context.Context, clientToken, model string) (*Lease, error) {
	clientToken = strings.TrimSpace(clientToken)
	cfg := p.cfg
	if clientToken == "" {
		return nil, errors.New("bridge: empty client token")
	}
	agentID, err := p.reg.AgentForModel(model)
	if err != nil {
		return nil, err
	}

	entry, err := p.bridgeEntryFor(clientToken)
	if err != nil {
		return nil, err
	}

	// B5: Global bridge daily limit check — before per-entry check, reject
	// if the total across ALL bridge entries exceeds BRIDGE_DAILY_LIMIT.
	if cfg.BridgeDailyLimit > 0 {
		// TOCTOU: snapshot read, then compare after unlock. Worst case: one
		// extra request past the limit. Acceptable for a best-effort cap.
		p.bridgeMu.Lock()
		total := p.bridgeDailyUsage
		p.bridgeMu.Unlock()
		if total >= cfg.BridgeDailyLimit {
			p.logger.Debug("pool: bridge global daily limit reached", "limit", cfg.BridgeDailyLimit, "used", total)
			return nil, fmt.Errorf("bridge: global daily limit %d reached (%d used)", cfg.BridgeDailyLimit, total)
		}
	}

	// Cooldown: skip the entry during its window; surface the remembered
	// ban/country-block/rate-limit error so the client keeps getting 403/429
	// instead of a generic failure (mirrors the fixed-token cooldown-skip
	// branch). The remembered errors are mutually exclusive in the run
	// manager; checked in pool precedence order.
	if until := entry.runs.CooldownUntil(); time.Now().Before(until) {
		if be := entry.runs.BanError(); be != nil {
			return nil, be
		}
		if cbe := entry.runs.CountryBlockedError(); cbe != nil {
			return nil, cbe
		}
		if rle := entry.runs.RateLimitError(); rle != nil {
			return nil, rle
		}
		if ice := entry.runs.IpCappedError(); ice != nil {
			return nil, ice
		}
		return nil, fmt.Errorf("bridge: token cooling down until %s", until.Format(time.RFC3339))
	}

	// Daily rolling cap, per client token (mirrors the fixed-token path).
	if cfg.MaxMessagesPerDay > 0 && p.bridgeUsageCount(entry) >= cfg.MaxMessagesPerDay {
		p.logger.Debug("pool: bridge entry daily message limit", "limit", cfg.MaxMessagesPerDay)
		return nil, p.bridgeDailyLimitError(entry)
	}

	// Session-create admission gate (issue #86), mirroring the fixed-token
	// path: concurrent session creates are bounded globally and per model.
	permit, err := p.gate.acquire(ctx, model)
	if err != nil {
		return nil, err
	}
	sessionStart := time.Now()
	instanceID, err := entry.session.EnsureSessionForModel(ctx, model)
	permit.Release()
	phasetiming.FromContext(ctx).Since(phasetiming.SessionRefreshMS, sessionStart)
	if err != nil {
		if errors.Is(err, upstream.ErrAuthRejected) {
			entry.runs.Cooldown(runs.DefaultCooldown)
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
			// B6: immediate eviction — the token is dead; do not let it
			// sit in the cache for 2h until the idle sweep catches it.
			p.bridgeEvictToken(clientToken)
		}
		if rle := asRateLimit(err); rle != nil {
			entry.runs.CooldownRateLimit(rle)
			// Issue #122: count admission-path spend_limited refusals on
			// the bridge entry's ledger (same counter as the chat-path
			// refusal in CooldownBridgeRateLimit).
			if rle.Status == "spend_limited" {
				p.bridgeMu.Lock()
				p.bridgeRecordSpendLimited(entry)
				p.bridgeMu.Unlock()
			}
		}
		if ice := asIpCapped(err); ice != nil {
			entry.runs.CooldownIpCapped(ice)
		}
		if be := asBan(err); be != nil {
			entry.runs.CooldownBan(be)
		}
		if cbe := asCountryBlocked(err); cbe != nil {
			entry.runs.CooldownCountryBlocked(cbe)
		}
		return nil, err
	}
	ss := entry.session.Snapshot()
	effectiveModel := model
	effectiveAgentID := agentID
	if ss.Model != "" && ss.Model != model {
		effectiveModel = ss.Model
		if p.reg != nil {
			if resolvedAgent, aerr := p.reg.AgentForModel(effectiveModel); aerr == nil {
				effectiveAgentID = resolvedAgent
			}
		}
	}
	// Issue #90a: pre-create the run at session admission (best-effort).
	_ = entry.runs.Precreate(ctx, effectiveAgentID)
	runStart := time.Now()
	run, err := entry.runs.Acquire(ctx, effectiveAgentID)
	phasetiming.FromContext(ctx).Since(phasetiming.RunAcquireMS, runStart)
	if err != nil {
		if errors.Is(err, upstream.ErrAuthRejected) {
			entry.runs.Cooldown(runs.DefaultCooldown)
			p.logger.Debug("pool: bridge entry cooling down", "duration", runs.DefaultCooldown.String())
			// B6: immediate eviction — the token is dead.
			p.bridgeEvictToken(clientToken)
		}
		if rle := asRateLimit(err); rle != nil {
			entry.runs.CooldownRateLimit(rle)
			// Issue #122: count run-start spend_limited refusals on the
			// bridge entry's ledger (same counter as the chat-path refusal).
			if rle.Status == "spend_limited" {
				p.bridgeMu.Lock()
				p.bridgeRecordSpendLimited(entry)
				p.bridgeMu.Unlock()
			}
		}
		if ice := asIpCapped(err); ice != nil {
			entry.runs.CooldownIpCapped(ice)
		}
		if be := asBan(err); be != nil {
			entry.runs.CooldownBan(be)
		}
		if cbe := asCountryBlocked(err); cbe != nil {
			entry.runs.CooldownCountryBlocked(cbe)
		}
		return nil, err
	}

	p.logger.Debug("pool: bridge lease acquired", "model", effectiveModel, "agent", effectiveAgentID, "instance_id", instanceID,
		"country", ss.CountryCode)
	// Track the activity and end any idle-maintenance pause, mirroring
	// Acquire: without this, IDLE_ROTATION_TIMEOUT was dead config in
	// bridge mode — lastActive stayed zero forever, so the pool never
	// idle-paused and bridge entries were maintained, polled, and
	// queued-advanced every pass indefinitely.
	p.lastActiveMu.Lock()
	p.lastActive = time.Now()
	p.idleFinished = false
	p.lastActiveMu.Unlock()
	return &Lease{Token: -1, Model: effectiveModel, AgentID: effectiveAgentID, Run: run, SessionInstanceID: instanceID,
		Bridge: entry, AcquiredAt: time.Now()}, nil
}

// ProbeNewToken validates a NOT-yet-added token against upstream with a
// zero-cost GET session probe (no session claim, no model needed). It builds
// the probe client from the pool's own config, so the base URL matches the
// one a freshly-built client would use (tests inject a mock URL here).
// Returns the session state on success, ErrNoActiveSession when the token is
// valid but idle, or the classified auth/network error (ErrBanned /
// ErrCountryBlocked / ErrAuthRejected / ErrRateLimited) otherwise.
func (p *Pool) ProbeNewToken(ctx context.Context, token string) (*upstream.SessionState, error) {
	if token == "" {
		return nil, errors.New("pool: empty token")
	}
	cfg := *p.cfg
	// Match the base URL of an existing pooled client when one exists: the
	// pool's fixed-token clients were built by the caller with the effective
	// upstream URL (tests inject a mock URL), while p.cfg.UpstreamBaseURL
	// may still hold the production default. A probe built from the wrong
	// URL would validate against a different host than the one the token
	// will actually use — silently false results.
	if len(p.toks) > 0 {
		if base := p.toks[0].client.BaseURL(); base != "" {
			cfg.UpstreamBaseURL = base
		}
	}
	client, err := upstream.New(token, &cfg)
	if err != nil {
		return nil, fmt.Errorf("pool: probe token: %w", err)
	}
	return client.ProbeAccount(ctx)
}

// BridgeCount returns the number of cached bridge entries (healthz).
func (p *Pool) BridgeCount() int {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return len(p.bridge)
}

// bridgeEntryFor returns the cached bridge entry for clientToken, creating
// it on first use (upstream client + session manager + run manager) and
// recording the use for LRU order. A token that cannot build an upstream
// client yields an error and is never cached.
//
// B1+B2: The upstream client is created OUTSIDE bridgeMu to avoid blocking
// other bridge operations during the network-heavy New call. A buffered
// creation-rate gate (bridgeCreateGate, capacity 4) limits concurrent
// client creations to prevent thundering-herd creation. A double-check
// after acquiring the gate ensures a concurrent creator did not already
// populate the cache.
//
// B3: Map keys use tokenKey (SHA-256 truncated to 32 hex chars) so raw
// client tokens are never stored as map keys in memory.
func (p *Pool) bridgeEntryFor(clientToken string) (*bridgeEntry, error) {
	key := tokenKey(clientToken)

	// Fast path: entry already cached.
	p.bridgeMu.Lock()
	if entry, ok := p.bridge[key]; ok {
		entry.lastUsed = time.Now()
		p.bridgeTouch(key)
		p.bridgeMu.Unlock()
		return entry, nil
	}
	p.bridgeMu.Unlock()

	// Slow path: create client OUTSIDE bridgeMu (B2). upstream.New may
	// involve DNS + TLS handshake; holding bridgeMu here would block every
	// other bridge operation for the full creation duration.
	//
	// B1: Acquire a creation-rate gate slot to cap concurrent New calls.
	select {
	case p.bridgeCreateGate <- struct{}{}:
		// Acquired.
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("bridge: creation rate limit exceeded (too many concurrent client builds)")
	}
	defer func() { <-p.bridgeCreateGate }()

	// Double-check: another goroutine may have created the entry while we
	// were waiting for the gate. Release bridgeMu before calling upstream.New
	// (DNS + TLS handshake can be slow; holding bridgeMu blocks every other
	// bridge operation).
	p.bridgeMu.Lock()
	if entry, ok := p.bridge[key]; ok {
		entry.lastUsed = time.Now()
		p.bridgeTouch(key)
		p.bridgeMu.Unlock()
		return entry, nil
	}
	p.bridgeMu.Unlock()

	// Create the client outside bridgeMu (B2).
	client, err := upstream.New(clientToken, p.cfg)
	if err != nil {
		return nil, fmt.Errorf("bridge: %w", err)
	}

	// Re-acquire lock and insert; another creator may have raced us.
	p.bridgeMu.Lock()
	if entry, ok := p.bridge[key]; ok {
		p.bridgeMu.Unlock()
		return entry, nil
	}
	entry := &bridgeEntry{token: clientToken, client: client, spend: newSpendLedger()}
	cfg := p.cfg
	entry.session = session.NewManagerWithStore(client, p.store)
	entry.session.SetReAdmitLead(cfg.SessionReAdmitLead)
	entry.session.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
	entry.runs = runs.NewRunManagerOpts(client, entry.session, runOptions(cfg))
	entry.lastUsed = time.Now()

	p.bridge[key] = entry
	p.bridgeOrder = append(p.bridgeOrder, key)
	// Drop the LRU victims under the lock, then FINISH their runs after
	// releasing it: FinishAllRuns is a sequential upstream call bounded by
	// the session-call timeout, so running it under bridgeMu would stall
	// every other bridge operation (AcquireBridge, bridgeRecordChat,
	// BridgeCount, bridgeMaintain) for the full eviction duration.
	victims := p.bridgeEvictLocked(entry)
	p.bridgeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	for _, victim := range victims {
		victim.runs.FinishAllRuns(ctx)
	}
	return entry, nil
}

// bridgeTouch moves clientToken to the newest end of the LRU order.
func (p *Pool) bridgeTouch(clientToken string) {
	for i, tok := range p.bridgeOrder {
		if tok == clientToken {
			if i < len(p.bridgeOrder)-1 {
				copy(p.bridgeOrder[i:], p.bridgeOrder[i+1:])
				p.bridgeOrder[len(p.bridgeOrder)-1] = clientToken
			}
			return
		}
	}
	p.bridgeOrder = append(p.bridgeOrder, clientToken)
}

// bridgeEvictLocked evicts the oldest bridge entries while the cache is
// over maxBridgeEntries (LRU): the victims are removed from the cache and
// LRU order and returned so the caller can FINISH their runs best-effort
// (bounded by the client's session-call timeout) AFTER releasing bridgeMu —
// the upstream FINISH calls must not run under the lock, or a full cache
// would stall every other bridge operation for the whole eviction. keep is
// the entry that was just created by the caller; it is excluded from the
// victim scan (like busy entries) because bridgeEntryFor hands it back for
// immediate use — evicting it here would leave its run and admitted session
// outside the cache, where neither bridgeMaintain nor Pool.Shutdown would
// ever sweep them. Caller holds bridgeMu.
func (p *Pool) bridgeEvictLocked(keep *bridgeEntry) []*bridgeEntry {
	var victims []*bridgeEntry
	for len(p.bridgeOrder) > maxBridgeEntries {
		// Scan from the LRU end for an entry WITHOUT outstanding leases:
		// FINISHing the run of an entry that still serves a request would
		// kill the in-flight chat. Busy entries are left in the cache for
		// the idle sweep (bridgeMaintain) once their leases drain; when
		// every entry is busy, nothing is evicted this pass.
		evicted := false
		for i := 0; i < len(p.bridgeOrder); {
			oldest := p.bridgeOrder[i]
			entry, ok := p.bridge[oldest]
			if !ok {
				// Stale LRU token (cache entry dropped elsewhere): trim it
				// and keep scanning.
				p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, oldest)
				continue
			}
			// The just-created entry is never its own eviction victim: the
			// caller will admit a session and START a run on it, and an
			// entry outside the cache is invisible to bridgeMaintain and
			// Pool.Shutdown — a leaked upstream run + admitted session
			// burning a daily slot per new client under saturation. Skip it
			// like a busy entry; the cache may briefly sit one over the cap
			// until an older entry's lease drains.
			if entry == keep {
				i++
				continue
			}
			if entry.runs.InflightCount() > 0 {
				i++
				continue
			}
			victims = append(victims, entry)
			delete(p.bridge, oldest)
			p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, oldest)
			p.logger.Debug("pool: bridge entry evicted (cache full)", "bridge_entries", len(p.bridge))
			evicted = true
			break
		}
		if !evicted {
			break
		}
	}
	return victims
}

// bridgeLen returns the number of cached bridge entries (test accessor).
func (p *Pool) bridgeLen() int {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return len(p.bridge)
}

// bridgeToken returns the cached entry for clientToken (test accessor).
// Accepts the raw client token and hashes it (B3) for map lookup.
func (p *Pool) bridgeToken(clientToken string) *bridgeEntry {
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	return p.bridge[tokenKey(clientToken)]
}

// bridgeTokenLabel returns a short non-reversible label for a bridge
// entry's client token, safe for logs: the sha256 of the token, hex,
// truncated to 8 chars. The raw client token must never reach logs (logring
// retains them for /admin/logs), so shutdown and diagnostics use the label,
// not the token.
func bridgeTokenLabel(entry *bridgeEntry) string {
	if entry == nil || entry.client == nil {
		return "bridge"
	}
	return "token-" + entry.client.TokenKey()[:8]
}

// bridgeSessionPollTick polls the bridge cache's active sessions on the same
// jittered schedule as the fixed tokens (gap #2). The sweep/eviction half
// stays in bridgeMaintain; only the per-entry session poll runs here so its
// timing is not quantized onto the 60s rotation grid.
func (p *Pool) bridgeSessionPollTick(ctx context.Context, cfg *config.Config) {
	p.bridgeMu.Lock()
	entries := make([]*bridgeEntry, 0, len(p.bridge))
	for _, entry := range p.bridge {
		entries = append(entries, entry)
	}
	p.bridgeMu.Unlock()

	for _, entry := range entries {
		if time.Now().Before(entry.runs.CooldownUntil()) {
			continue
		}
		if entry.runs.InflightCount() > 0 {
			continue
		}
		now := time.Now()
		if !entry.nextPollAt.IsZero() && now.Before(entry.nextPollAt) {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		err := entry.session.Poll(mCtx)
		cancel()
		var delay time.Duration
		if err != nil {
			entry.pollFailures++
			delay = sessionPollBackoffDelay(entry.pollFailures, sessionPollRetryAfter(err))
			p.logger.Debug("pool: bridge session poll failed", "err", err, "retry_in", delay)
		} else {
			entry.pollFailures = 0
			snap := entry.session.Snapshot()
			delay = sessionPollSuccessDelay(snap)
		}
		entry.nextPollAt = time.Now().Add(delay)
	}
}

// bridgeMaintain sweeps the bridge cache: entries idle past bridgeIdleEvict
// are dropped (runs FINISHed and the upstream session ended, best-effort);
// entries with in-flight leases are NEVER evicted — FINISHing their runs
// would kill the in-flight chat, so busy entries always get the per-token
// maintain work below and are only swept once their leases drain and they
// stay idle. The remaining entries get the per-token maintain work — rotate
// aged runs and advance queued sessions, bounded by the same RequestTimeout
// ctx as the fixed-token loop. On idle passes (idle=true) only the sweep
// runs: the per-entry queued-advance pauses with the fixed tokens, and the
// idle-sweep keeps bridge entries from staying admitted upstream past
// bridgeIdleEvict while the pool stays idle. Active-session liveness polls
// are NOT part of this pass — they run on the jittered
// bridgeSessionPollTick schedule (gap #2).
func (p *Pool) bridgeMaintain(ctx context.Context, idle bool) {
	cfg := p.cfg
	var toEvict []*bridgeEntry
	var toMaintain []*bridgeEntry

	p.bridgeMu.Lock()
	now := time.Now()
	for token, entry := range p.bridge {
		// Busy entry: leave it for the maintain pass (same rule as
		// bridgeEvictLocked's busy skip — the idle sweep only handles
		// entries once their leases drain).
		if entry.runs.InflightCount() > 0 {
			toMaintain = append(toMaintain, entry)
			continue
		}
		if now.Sub(entry.lastUsed) > bridgeIdleEvict {
			toEvict = append(toEvict, entry)
			delete(p.bridge, token)
			p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, token)
			p.logger.Debug("pool: bridge entry evicted (idle)", "bridge_entries", len(p.bridge))
		} else {
			toMaintain = append(toMaintain, entry)
		}
	}

	// Recompute bridgeDailyUsage from live entries: each entry's usage
	// slice is pruned to the 24h window, so summing their lengths gives
	// the correct rolling total (mirrors bridgeUsageCount per-entry).
	total := 0
	for _, entry := range p.bridge {
		cutoff := now.Add(-usageWindow)
		history := entry.usage
		first := 0
		for first < len(history) && history[first].Before(cutoff) {
			first++
		}
		entry.usage = history[first:]
		total += len(entry.usage)
	}
	p.bridgeDailyUsage = total

	p.bridgeMu.Unlock()

	for _, entry := range toEvict {
		// Mirror the shutdown drain: FINISH the runs AND end the entry's
		// upstream session, so a dropped idle entry does not leak its
		// session upstream. Bounded by the same RequestTimeout ctx as the
		// per-token maintain work so a hung upstream cannot stall the loop.
		eCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		entry.runs.FinishAllRuns(eCtx)
		if endErr := entry.session.EndSession(eCtx); endErr != nil {
			// B4: log EndSession errors at WARN so failed session teardowns
			// are visible in diagnostics instead of silently swallowed.
			p.logger.Warn("pool: bridge EndSession failed during idle eviction",
				"err", endErr, "token_label", bridgeTokenLabel(entry))
		}
		cancel()
	}
	for _, entry := range toMaintain {
		if idle {
			// Idle pass: the per-entry maintain work pauses with the fixed
			// tokens; only the idle-eviction sweep above runs.
			continue
		}
		// Same cooldown skip as the fixed-token loop: no queued-session
		// EnsureSession, no rotation while cooling down.
		if time.Now().Before(entry.runs.CooldownUntil()) {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		entry.runs.Maintain(mCtx)
		// Same in-flight gate as the fixed-token loop: skip the queued-
		// session GET while a chat is in flight so it cannot kick the active
		// session (reference/freebuff-proxy-hengxin session-manager.js:37-49,
		// 259-260). Active-session liveness polls run on the jittered
		// bridgeSessionPollTick schedule instead.
		if entry.runs.InflightCount() == 0 {
			snap := entry.session.Snapshot()
			if snap.Status == "queued" {
				if _, err := entry.session.EnsureSession(mCtx); err != nil {
					p.logger.Debug("pool: bridge maintain session not ready", "err", err)
				} else {
					// Issue #90a: pre-create the run for the session's model
					// agent so the first request on this session does not pay
					// the START latency (mirrors the fixed-token path).
					after := entry.session.Snapshot()
					if agentID, err := p.reg.AgentForModel(after.Model); err == nil && agentID != "" {
						_ = entry.runs.Precreate(mCtx, agentID)
					}
				}
			}
		}
		cancel()
	}
}

// bridgeEvictToken immediately removes a token from the bridge cache (B6):
// used when a token is confirmed dead (ErrAuthRejected) so it does not sit
// in the cache for the full bridgeIdleEvict window. The removed entry's
// runs are FINISHed best-effort after releasing the lock.
func (p *Pool) bridgeEvictToken(rawToken string) {
	key := tokenKey(rawToken)
	p.bridgeMu.Lock()
	entry, ok := p.bridge[key]
	if !ok {
		p.bridgeMu.Unlock()
		return
	}
	delete(p.bridge, key)
	p.bridgeOrder = removeBridgeOrder(p.bridgeOrder, key)
	p.logger.Debug("pool: bridge entry evicted (dead token)", "token_label", bridgeTokenLabel(entry))
	p.bridgeMu.Unlock()

	// Best-effort cleanup: FINISH runs and end session, bounded by context.
	// A hung upstream should not stall the caller.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entry.runs.FinishAllRuns(ctx)
	if endErr := entry.session.EndSession(ctx); endErr != nil {
		p.logger.Warn("pool: bridge EndSession failed during dead-token eviction",
			"err", endErr, "token_label", bridgeTokenLabel(entry))
	}
}

// removeBridgeOrder drops token from the LRU order slice.
func removeBridgeOrder(order []string, token string) []string {
	for i, tok := range order {
		if tok == token {
			return append(order[:i], order[i+1:]...)
		}
	}
	return order
}
