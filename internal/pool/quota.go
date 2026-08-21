package pool

import (
	"time"

	"freebuff-proxy/internal/upstream"
)

// quotaRemaining reports the token's session-quota state for model from the
// last admission (issue #85): known reports whether the quota is known with
// a positive remaining allowance; remaining is the positive delta; capped
// reports RecentCount >= Limit with a future ResetAt (the token must be
// skipped this pass — it cannot serve the model right now). Quotas with a
// past/absent ResetAt are treated as fresh (the window rolled) and never
// capped.
func quotaRemaining(tok *tokenEntry, model string) (known bool, remaining float64, capped bool) {
	q, ok := tok.session.Snapshot().QuotaByModel[model]
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
	// RecentCount >= Limit but the window already rolled: unknown until the
	// next admission reports a fresh count.
	return false, 0, false
}

// quotaLimitError builds the 429 surfaced when token is excluded for the
// model's exhausted session quota (issue #85): RetryAfter is the time until
// the window reset, mirroring the upstream RateLimitError contract.
func quotaLimitError(tok *tokenEntry, model string) *upstream.RateLimitError {
	q := tok.session.Snapshot().QuotaByModel[model]
	retryAfter := time.Duration(0)
	if !q.ResetAt.IsZero() && q.ResetAt.After(time.Now()) {
		retryAfter = time.Until(q.ResetAt)
	}
	return &upstream.RateLimitError{
		Status:      "rate_limited",
		Model:       model,
		RetryAfter:  retryAfter,
		Limit:       q.Limit,
		RecentCount: q.RecentCount,
		ResetAt:     q.ResetAt,
		Body:        "session quota exhausted for model",
	}
}

// --- pool quota/snapshot ---

// recordChat appends one successful upstream chat for token and prunes the
// token's usage history outside the 24h window.
func (p *Pool) recordChat(token int) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	// Authoritative bound under the lock: p.msgsPerToken is the index space
	// AddToken/RemoveLastToken/RemoveAllTokens keep consistent, so a
	// removal that raced the snapshot check above (or a lease issued from a
	// snapshot that already went stale) is caught here instead of indexing
	// past the usage slice.
	if token < 0 || token >= len(p.msgsPerToken) {
		return
	}
	cutoff := time.Now().Add(-usageWindow)
	history := p.msgsPerToken[token]
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	p.msgsPerToken[token] = append(history[first:], time.Now())
}

// recordChatEntry appends one successful upstream chat for the lease's
// backing entry and prunes its usage history outside the 24h window. The
// entry is located by pointer in the CURRENT token list so the usage lands
// on the right token: after a concurrent RemoveLastToken+AddToken, the
// lease's Token index may point at a different token (or be out of range),
// and charging by index would mis-record. An entry that is no longer in the
// pool (removed while the request was in flight) skips the recording.
func (p *Pool) recordChatEntry(entry *tokenEntry) {
	if entry == nil {
		return
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	for idx, tok := range *p.toks.Load() {
		if tok != entry {
			continue
		}
		// Authoritative bound under the lock: the msgsPerToken slice is
		// rebuilt under usageMu by AddToken/RemoveLastToken/RemoveAllTokens,
		// and a removal racing this snapshot can leave the entry present in
		// toks but absent from the usage slice — never index past it.
		if idx < 0 || idx >= len(p.msgsPerToken) {
			return
		}
		cutoff := time.Now().Add(-usageWindow)
		history := p.msgsPerToken[idx]
		first := 0
		for first < len(history) && history[first].Before(cutoff) {
			first++
		}
		p.msgsPerToken[idx] = append(history[first:], time.Now())
		return
	}
	// Entry removed from the pool: skip recording rather than charge a
	// reused index.
}

// usageWindow, pruning expired timestamps.
func (p *Pool) usageCount(token int) int {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return 0
	}
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	// Authoritative bound under the lock (see recordChat): the usage slice
	// may have been truncated/nil'd by a removal racing the snapshot check.
	if token < 0 || token >= len(p.msgsPerToken) {
		return 0
	}
	cutoff := time.Now().Add(-usageWindow)
	history := p.msgsPerToken[token]
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	p.msgsPerToken[token] = history[first:]
	return len(p.msgsPerToken[token])
}

// dailyLimitError builds the 429 surfaced when token is capped by
// MAX_MESSAGES_PER_DAY: RetryAfter is the time until the token's oldest
// recorded chat ages out of the 24h window (the earliest moment a slot
// frees).
func (p *Pool) dailyLimitError(token int) *upstream.RateLimitError {
	return &upstream.RateLimitError{
		RetryAfter:  p.usageResetIn(token),
		Limit:       float64(p.cfg.Load().MaxMessagesPerDay),
		RecentCount: float64(p.usageCount(token)),
		Body:        "daily message limit reached",
	}
}

// usageResetIn is how long until token's oldest usage timestamp ages out of
// the window (0 when the token has no recorded usage or the reset is due).
func (p *Pool) usageResetIn(token int) time.Duration {
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	// Bounds check under the lock (see recordChat): the usage slice may
	// have been truncated/nil'd by a removal racing this call — usageResetIn
	// previously had no guard at all and indexed past the end.
	if token < 0 || token >= len(p.msgsPerToken) {
		return 0
	}
	history := p.msgsPerToken[token]
	if len(history) == 0 {
		return 0
	}
	reset := time.Until(history[0].Add(usageWindow))
	if reset < 0 {
		return 0
	}
	return reset
}

// --- bridge mode internals ---

// bestDailyLimit picks the daily-cap error whose window frees first: the
// client retries when the first token has a free slot again.
func bestDailyLimit(entries []*upstream.RateLimitError) *upstream.RateLimitError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.RetryAfter < best.RetryAfter {
			best = e
		}
	}
	return best
}

// bridgeRecordChat appends one successful upstream chat for the bridge
// entry and prunes its usage history outside the 24h window.
func (p *Pool) bridgeRecordChat(entry *bridgeEntry) {
	if entry == nil {
		return
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	cutoff := time.Now().Add(-usageWindow)
	history := entry.usage
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	entry.usage = append(history[first:], time.Now())
	p.bridgeDailyUsage++
}

// bridgeUsageCount returns how many successful chats the bridge entry sent
// within the last usageWindow, pruning expired timestamps.
func (p *Pool) bridgeUsageCount(entry *bridgeEntry) int {
	if entry == nil {
		return 0
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	cutoff := time.Now().Add(-usageWindow)
	history := entry.usage
	first := 0
	for first < len(history) && history[first].Before(cutoff) {
		first++
	}
	entry.usage = history[first:]
	return len(entry.usage)
}

// bridgeUsageResetIn is how long until the bridge entry's oldest usage
// timestamp ages out of the window (0 when no usage is recorded or the
// reset is due).
func (p *Pool) bridgeUsageResetIn(entry *bridgeEntry) time.Duration {
	if entry == nil {
		return 0
	}
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	history := entry.usage
	if len(history) == 0 {
		return 0
	}
	reset := time.Until(history[0].Add(usageWindow))
	if reset < 0 {
		return 0
	}
	return reset
}

// bridgeDailyLimitError builds the 429 surfaced when the bridge entry is
// capped by MAX_MESSAGES_PER_DAY (mirrors dailyLimitError for fixed
// tokens): RetryAfter is the time until the entry's oldest recorded chat
// ages out of the 24h window.
func (p *Pool) bridgeDailyLimitError(entry *bridgeEntry) *upstream.RateLimitError {
	return &upstream.RateLimitError{
		RetryAfter:  p.bridgeUsageResetIn(entry),
		Limit:       float64(p.cfg.Load().MaxMessagesPerDay),
		RecentCount: float64(p.bridgeUsageCount(entry)),
		Body:        "daily message limit reached",
	}
}
