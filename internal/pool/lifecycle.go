package pool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// LeaseRelease decrements the leased run's inflight counter. Call when the
// request completes or fails. Safe on nil leases.
func (p *Pool) LeaseRelease(lease *Lease) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.Release(lease.Run)
		return
	}
	if lease.entry == nil {
		return // synthetic lease without a backing entry
	}
	lease.entry.runs.Release(lease.Run)
	// A lease on a removed token (RemoveLastToken swapped the snapshot out
	// from under a concurrent Acquire) releases through its own entry — the
	// bounds-checked index path would no-op and leak the run's inflight, or
	// mis-target a reused index. RemoveLastToken parked the entry undrained
	// when it observed the slip; drain it once its last lease has released.
	p.retiredMu.Lock()
	_, parked := p.retired[lease.entry]
	p.retiredMu.Unlock()
	if parked && lease.entry.runs.InflightCount() == 0 {
		p.drainRemovedToken(lease.entry)
	}
}

// LeaseAbandon releases a lease whose downstream client context was
// cancelled mid-chat (issue #53, CLI DELETE-on-exit parity): when this was
// the LAST in-flight request on the run, the run is dropped from the active
// set and FINISHed through the bounded queue so upstream does not keep an
// abandoned agent run alive until rotation. Concurrent requests on the same
// run keep it alive. The server calls this instead of LeaseRelease when it
// observes a client disconnect.
func (p *Pool) LeaseAbandon(lease *Lease) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.ReleaseAbandoned(lease.Run)
		return
	}
	if lease.entry != nil {
		lease.entry.runs.ReleaseAbandoned(lease.Run)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	(*toks)[lease.Token].runs.ReleaseAbandoned(lease.Run)
}

// RecordRunStep records a completed chat step on the lease's run (issue
// #114): steps are accumulated in memory and sent WITH FINISH — recording
// is local-only and never an upstream call (the CLI has no /steps
// endpoint). The server fires it after a successful chat with the response
// message id ("" when the stream never carried one).
func (p *Pool) RecordRunStep(lease *Lease, messageID string) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.RecordStep(lease.Run, messageID)
		return
	}
	if lease.entry != nil {
		lease.entry.runs.RecordStep(lease.Run, messageID)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	(*toks)[lease.Token].runs.RecordStep(lease.Run, messageID)
}

// MarkRunFailed marks the lease's run as failed for its eventual FINISH
// (issue #114): the server calls it when a chat dies on a terminal upstream
// error so the run does not FINISH as completed (a gateway with zero failed
// runs looks synthetic). The run stays active; only its terminal status is
// recorded. Nil-safe (an acquire failure leaves no lease).
func (p *Pool) MarkRunFailed(lease *Lease) {
	if lease == nil || lease.Run == nil {
		return
	}
	if lease.Bridge != nil {
		lease.Bridge.runs.MarkFailed(lease.Run)
		return
	}
	if lease.entry != nil {
		lease.entry.runs.MarkFailed(lease.Run)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	(*toks)[lease.Token].runs.MarkFailed(lease.Run)
}

// RecordSpend adds tokens to the lease's backing token spend ledger (issue
// #87): the server reports the usage block of a completed chat. Non-positive
// deltas are ignored. Production caller: chatCore feeds the relay's observed
// usage total once per successful chat completion (#122). The daily $15/$5/
// $0.50 ceilings are server-enforced and cohort-dependent, so this
// token-count ledger is a heuristic proxy, not exact USD accounting — see
// spend.go's package comment.
func (p *Pool) RecordSpend(lease *Lease, tokens int64) {
	if lease == nil || tokens <= 0 {
		return
	}
	if lease.Bridge != nil {
		p.bridgeRecordSpend(lease.Bridge, tokens)
		return
	}
	if lease.entry != nil {
		p.recordSpendEntry(lease.entry, tokens)
		return
	}
	toks := p.toks.Load()
	if lease.Token < 0 || lease.Token >= len(*toks) {
		return
	}
	p.recordSpend(lease.Token, tokens)
}

// InvalidateSession drops the cached free session of token so the next
// Acquire re-creates it (session-invalid recovery). The invalidation is
// guarded to the given instance id (issue #132): after a pre-emptive
// re-admit replaced the cache, a chat that rode the old superseded instance
// failing must not invalidate the fresh one. Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateSession(token int, instanceID string) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	(*toks)[token].session.InvalidateInstance(instanceID)
}

// InvalidateRun drops the current run of token for agentID so the next
// Acquire starts a fresh one (run-invalid recovery). Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateRun(token int, agentID string) {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return
	}
	(*toks)[token].runs.Invalidate(agentID)
}

// ClearQueuedCaches drops every token's cached QUEUED session (issue #100):
// the queue-time model fallback calls this before re-acquiring with the
// fallback model, so the fallback acquire creates a fresh session instead of
// re-surfacing the same waiting room. Returns how many queued caches were
// cleared. Other states (active/disabled) are untouched.
func (p *Pool) ClearQueuedCaches() int {
	toks := p.toks.Load()
	cleared := 0
	for _, tok := range *toks {
		if tok.session.ClearQueued() {
			cleared++
		}
	}
	return cleared
}

// InvalidateBridgeSession drops the cached free session of the bridge
// entry so the next AcquireBridge re-creates it (session-invalid recovery).
// Guarded to the lease's instance id (issue #132) — see InvalidateSession.
func (p *Pool) InvalidateBridgeSession(lease *Lease) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.session.InvalidateInstance(lease.SessionInstanceID)
}

// InvalidateBridgeRun drops the current run of the bridge entry for agentID
// so the next AcquireBridge starts a fresh one (run-invalid recovery).
func (p *Pool) InvalidateBridgeRun(lease *Lease, agentID string) {
	if lease == nil || lease.Bridge == nil {
		return
	}
	lease.Bridge.runs.Invalidate(agentID)
}

// RemoveLastToken removes the highest-index fixed token (dashboard action).
// Only the last index can be removed safely: removing a middle token would
// shift indices under in-flight leases. Refuses while the token has active
// runs. The removed token's runs are FINISHed and its admitted session
// ended (they used to leak upstream, contrast RemoveAllTokens); a lease
// that slips through the busy-check/swap race is released through the
// retired map and drained once it releases.
func (p *Pool) RemoveLastToken() error {
	toks := p.toks.Load()
	if len(*toks) == 0 {
		return errors.New("pool: no tokens to remove")
	}
	last := (*toks)[len(*toks)-1]
	if last.runs.InflightCount() > 0 {
		return errors.New("pool: token has in-flight requests; wait for them to finish")
	}
	next := append([]*tokenEntry{}, (*toks)[:len(*toks)-1]...)
	p.toks.Store(&next)
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	p.msgsPerToken = p.msgsPerToken[:len(p.msgsPerToken)-1]
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	p.spendPerToken = p.spendPerToken[:len(p.spendPerToken)-1]

	// The busy check above and the swap are TOCTOU: an Acquire that loaded
	// the pre-removal snapshot can lease the removed token in between. Park
	// the entry so that lease is still released (LeaseRelease bounds-checks
	// the new snapshot and would otherwise no-op, leaking the run's
	// inflight), then drain now when no lease slipped — finishing the
	// removed token's run and ending its admitted session. A slipped lease
	// keeps the entry parked; LeaseRelease drains it once the last lease
	// releases.
	slip := last.runs.InflightCount() > 0
	p.retiredMu.Lock()
	if p.retired == nil {
		p.retired = make(map[*tokenEntry]time.Time)
	}
	p.retired[last] = time.Now()
	p.retiredMu.Unlock()
	if !slip {
		p.drainRemovedToken(last)
	}
	return nil
}

// drainRemovedToken finishes the removed token's runs and ends its admitted
// session (mirrors RemoveAllTokens' run finish plus the session end that
// removal previously skipped), bounded by the per-token shutdown timeout so
// a hung upstream cannot block the dashboard action. guarded by
// tokenEntry.drained sync.Once to prevent double-drain when both
// LeaseRelease and pruneRetired race on the same retired entry.
func (p *Pool) drainRemovedToken(entry *tokenEntry) {
	entry.drained.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		entry.runs.FinishAllRuns(ctx)
		_ = entry.session.EndSession(ctx)
		p.retiredMu.Lock()
		defer p.retiredMu.Unlock()
		delete(p.retired, entry)
	})
}

// RemoveAllTokens finishes every fixed token's runs and empties the pool
// (bridge-mode switch). In-flight leases on removed tokens no-op on release
// (bounds-checked index access). Config must be updated separately.
func (p *Pool) RemoveAllTokens(ctx context.Context) {
	toks := p.toks.Load()
	for _, t := range *toks {
		t.runs.FinishAllRuns(ctx)
		_ = t.session.EndSession(ctx)
	}
	empty := make([]*tokenEntry, 0)
	p.toks.Store(&empty)
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	p.msgsPerToken = nil
	p.spendMu.Lock()
	defer p.spendMu.Unlock()
	p.spendPerToken = nil
}

// FinishTokenRuns finishes all active runs of token (dashboard action).
func (p *Pool) FinishTokenRuns(ctx context.Context, token int) error {
	toks := p.toks.Load()
	if token < 0 || token >= len(*toks) {
		return fmt.Errorf("pool: token %d out of range", token)
	}
	(*toks)[token].runs.FinishAllRuns(ctx)
	return nil
}

// Shutdown stops the background jobs and drains every token: FINISH all
// runs, end the sessions, bounded by a 10s force deadline per token. Cached
// bridge entries (bridge mode) are drained best-effort the same way after
// the fixed tokens: FINISH all runs and end each entry's session so no
// upstream activity is left behind.
func (p *Pool) Shutdown(ctx context.Context) {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()

	var errs []string
	toks := p.toks.Load()
	for i, tok := range *toks {
		tokCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		tok.runs.Shutdown(tokCtx)
		cancel()
		// With run persistence the runs are intentionally kept alive for
		// restart-resume — not a drain failure (review P3).
		if !tok.runs.KeptForPersistence() {
			if snap := tok.runs.Snapshot(); snap.ActiveRuns > 0 {
				errs = append(errs, fmt.Sprintf("token-%d: %d runs left after shutdown", i+1, snap.ActiveRuns))
			}
		}
	}

	// Drain the cached bridge entries best-effort. The maintain loop is
	// already stopped (wg.Wait above), so the entry list is stable.
	p.bridgeMu.Lock()
	defer p.bridgeMu.Unlock()
	entries := make([]*bridgeEntry, 0, len(p.bridge))
	for _, entry := range p.bridge {
		entries = append(entries, entry)
	}
	for _, entry := range entries {
		entryCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		entry.runs.FinishAllRuns(entryCtx)
		if snap := entry.runs.Snapshot(); snap.ActiveRuns > 0 {
			errs = append(errs, fmt.Sprintf("bridge %s: %d runs left after shutdown", bridgeTokenLabel(entry), snap.ActiveRuns))
		}
		if err := entry.session.Shutdown(entryCtx); err != nil {
			errs = append(errs, fmt.Sprintf("bridge %s: shutdown session: %v", bridgeTokenLabel(entry), err))
		}
		cancel()
	}

	if len(errs) > 0 {
		slog.Warn("pool: shutdown incomplete", "errors", strings.Join(errs, "; "))
	}
}

// pruneRetired drops retired tokens that hold no leases and have been
// parked past the drain grace (their runs were already finished at removal
// or on the last release). Entries still carrying a lease stay until
// LeaseRelease drains them.
//
// NOTE: pruneRetired only deletes entries with InflightCount==0 (no active
// leases) and after the drain grace period. The drained sync.Once in
// drainRemovedToken guards against double-drain when LeaseRelease and
// pruneRetired race on the same retired entry.
func (p *Pool) pruneRetired() {
	p.retiredMu.Lock()
	defer p.retiredMu.Unlock()
	for entry, swappedAt := range p.retired {
		if entry.runs.InflightCount() == 0 && time.Since(swappedAt) > retiredDrainGrace {
			delete(p.retired, entry)
		}
	}
}
