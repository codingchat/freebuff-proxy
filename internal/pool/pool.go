// Package pool is the multi-token front door: it picks the token that will
// serve a model request, then leases a run from that token's RunManager and
// an instance from its session manager. Port of freebuff2api-quorinex
// run_manager.go (Acquire half) with the upstream/session/runs split of this
// project.
//
// Failover semantics (PRD §6 error matrix):
//   - 401 (ErrAuthRejected) from a token's run START → 30-min cooldown for
//     that token, try the next.
//   - session waiting room → remember the best position, try the next token;
//     only when every token is queued does the pool surface the waiting-room
//     error (503 + Retry-After upstream).
//   - run-invalid / session-invalid recoveries are NOT handled here: the
//     caller (server) retries once via a fresh Acquire after invalidating.
//   - anything else → next token; all failed → combined error.
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// maintainInterval is how often the background job rotates aged runs and
// advances queued sessions (PRD §3: 60s maintain ticker).
const maintainInterval = time.Minute

// shutdownTimeout bounds each token's Shutdown during Pool.Shutdown when the
// caller's context carries no earlier deadline.
const shutdownTimeout = 10 * time.Second

// Lease is one acquired right to send a chat request through a specific
// token. The caller must call Pool.LeaseRelease when the request completes
// or fails (it decrements the run's inflight counter).
type Lease struct {
	Token             int // index into config.AuthTokens
	AgentID           string
	Run               *runs.Run
	SessionInstanceID string // "" when the session is disabled
}

// TokenSnapshot is one token's healthz view.
type TokenSnapshot struct {
	Token                int
	CooldownUntil        time.Time
	SessionStatus        string
	SessionInstanceID    string
	SessionQueuePosition int
	SessionQueueDepth    int
	ActiveRuns           int
	Requests             int
}

// Pool balances requests across the configured tokens.
type Pool struct {
	cfg  *config.Config
	reg  *registry.Registry
	toks []*tokenEntry

	rr     atomic.Uint64 // round-robin start index
	logger *slog.Logger

	once   sync.Once
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type tokenEntry struct {
	session *session.Manager
	runs    *runs.RunManager
	client  *upstream.Client
}

// New builds the pool over the configured tokens. len(clients) and
// len(sessions) must both equal len(cfg.AuthTokens); each pair is bound to
// one token and one RunManager.
func New(cfg *config.Config, clients []*upstream.Client, sessions []*session.Manager, reg *registry.Registry) (*Pool, error) {
	if cfg == nil {
		return nil, errors.New("pool: nil config")
	}
	if reg == nil {
		return nil, errors.New("pool: nil registry")
	}
	if len(clients) != len(cfg.AuthTokens) {
		return nil, fmt.Errorf("pool: %d clients for %d tokens", len(clients), len(cfg.AuthTokens))
	}
	if len(sessions) != len(cfg.AuthTokens) {
		return nil, fmt.Errorf("pool: %d sessions for %d tokens", len(sessions), len(cfg.AuthTokens))
	}

	p := &Pool{cfg: cfg, reg: reg, logger: slog.Default()}
	for i := range cfg.AuthTokens {
		p.toks = append(p.toks, &tokenEntry{
			session: sessions[i],
			runs:    runs.NewRunManager(clients[i], sessions[i], cfg.RotationInterval),
			client:  clients[i],
		})
	}
	return p, nil
}

// Acquire resolves the model's agent, picks a start token round-robin, and
// fails over linearly until a token yields both a run and a session. Returns
// a lease on success. Registry misses (unknown model) are returned as-is.
func (p *Pool) Acquire(ctx context.Context, model string) (*Lease, error) {
	if len(p.toks) == 0 {
		return nil, errors.New("pool: no auth tokens configured")
	}
	agentID, err := p.reg.AgentForModel(model)
	if err != nil {
		return nil, err
	}

	start := int(p.rr.Add(1)-1) % len(p.toks)
	var errs []string
	var waiting []*session.WaitingRoomError
	var rateLimited []*upstream.RateLimitError

	for offset := 0; offset < len(p.toks); offset++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		idx := (start + offset) % len(p.toks)
		tok := p.toks[idx]
		name := fmt.Sprintf("token-%d", idx+1)

		if until := tok.runs.CooldownUntil(); time.Now().Before(until) {
			errs = append(errs, fmt.Sprintf("%s: cooling down until %s", name, until.Format(time.RFC3339)))
			p.logger.Debug("pool: token skipped (cooldown)", "token", idx+1, "until", until.Format(time.RFC3339))
			if rle := tok.runs.RateLimitError(); rle != nil {
				rateLimited = append(rateLimited, rle)
			}
			continue
		}

		run, err := tok.runs.Acquire(ctx, agentID)
		if err != nil {
			if errors.Is(err, upstream.ErrAuthRejected) {
				tok.runs.Cooldown(runs.DefaultCooldown)
				p.logger.Debug("pool: token cooling down", "token", idx+1, "duration", runs.DefaultCooldown.String())
			}
			if rle := asRateLimit(err); rle != nil {
				tok.runs.CooldownRateLimit(rle)
				rateLimited = append(rateLimited, rle)
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}

		instanceID, err := tok.session.EnsureSession(ctx)
		if err != nil {
			// Release the run lease we just acquired — otherwise the
			// inflight counter never returns to zero and the run can
			// never be FINISHed on rotation (draining-list leak).
			tok.runs.Release(run)
			var wr *session.WaitingRoomError
			if errors.As(err, &wr) {
				waiting = append(waiting, wr)
			}
			if rle := asRateLimit(err); rle != nil {
				tok.runs.CooldownRateLimit(rle)
				rateLimited = append(rateLimited, rle)
			}
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
			continue
		}

		p.logger.Debug("pool: lease acquired", "token", idx+1, "model", model, "agent", agentID, "instance_id", instanceID)
		return &Lease{Token: idx, AgentID: agentID, Run: run, SessionInstanceID: instanceID}, nil
	}

	if len(waiting) == len(p.toks) && len(waiting) > 0 {
		wr := bestWaitingRoom(waiting)
		p.logger.Debug("pool: waiting room surfaced", "position", wr.Position, "queue_depth", wr.QueueDepth, "retry_after", wr.RetryAfter.String())
		return nil, wr
	}
	if len(rateLimited) == len(p.toks) && len(rateLimited) > 0 {
		return nil, bestRateLimit(rateLimited)
	}
	return nil, fmt.Errorf("unable to acquire run from any token: %s", strings.Join(errs, "; "))
}

// LeaseRelease decrements the leased run's inflight counter. Call when the
// request completes or fails. Safe on nil leases.
func (p *Pool) LeaseRelease(lease *Lease) {
	if lease == nil || lease.Token < 0 || lease.Token >= len(p.toks) || lease.Run == nil {
		return
	}
	p.toks[lease.Token].runs.Release(lease.Run)
}

// InvalidateSession drops the cached free session of token so the next
// Acquire re-creates it (session-invalid recovery). Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateSession(token int) {
	if token < 0 || token >= len(p.toks) {
		return
	}
	p.toks[token].session.Invalidate()
}

// InvalidateRun drops the current run of token for agentID so the next
// Acquire starts a fresh one (run-invalid recovery). Out-of-range tokens are
// ignored.
func (p *Pool) InvalidateRun(token int, agentID string) {
	if token < 0 || token >= len(p.toks) {
		return
	}
	p.toks[token].runs.Invalidate(agentID)
}

// CooldownToken puts token in a cooldown window of duration d (auth-reject
// recovery, e.g. runs.DefaultCooldown). Out-of-range tokens are ignored.
func (p *Pool) CooldownToken(token int, d time.Duration) {
	if token < 0 || token >= len(p.toks) {
		return
	}
	p.toks[token].runs.Cooldown(d)
}

// CooldownTokenRateLimit applies a rate-limit cooldown to token
// (remembered so Acquire surfaces 429 + Retry-After during the window).
// Out-of-range tokens are ignored.
func (p *Pool) CooldownTokenRateLimit(token int, rle *upstream.RateLimitError) {
	if token < 0 || token >= len(p.toks) || rle == nil {
		return
	}
	p.toks[token].runs.CooldownRateLimit(rle)
}

// Chat sends a chat-completion request through the leased token's upstream
// client, returning the raw SSE body reader on 2xx. The caller must release
// the lease (LeaseRelease) once the request completes or fails, and close
// the returned body.
func (p *Pool) Chat(ctx context.Context, lease *Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
	if lease == nil || lease.Token < 0 || lease.Token >= len(p.toks) {
		return nil, errors.New("pool: chat: invalid lease token")
	}
	return p.toks[lease.Token].client.ChatCompletions(ctx, opts, body)
}

// Start launches the background jobs: a best-effort prewarm of every
// registry agent across every token (so the first request does not pay the
// START latency) and the 60s maintain loop (rotate aged runs + advance
// queued sessions). Both stop when ctx is canceled; Pool.Shutdown cancels.
func (p *Pool) Start(ctx context.Context) {
	p.once.Do(func() {
		agentIDs := p.reg.AgentIDs()
		runCtx, cancel := context.WithCancel(ctx)
		p.cancel = cancel
		p.wg.Add(2)
		go p.prewarm(runCtx, agentIDs)
		go p.maintainLoop(runCtx)
	})
}

// Shutdown stops the background jobs and drains every token: FINISH all
// runs, end the sessions, bounded by a 10s force deadline per token.
func (p *Pool) Shutdown(ctx context.Context) {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()

	var errs []string
	for i, tok := range p.toks {
		tokCtx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		tok.runs.Shutdown(tokCtx)
		cancel()
		if snap := tok.runs.Snapshot(); snap.ActiveRuns > 0 {
			errs = append(errs, fmt.Sprintf("token-%d: %d runs left after shutdown", i+1, snap.ActiveRuns))
		}
	}
	if len(errs) > 0 {
		slog.Warn("pool: shutdown incomplete", "errors", strings.Join(errs, "; "))
	}
}

// Snapshot returns the per-token healthz view.
func (p *Pool) Snapshot() []TokenSnapshot {
	out := make([]TokenSnapshot, 0, len(p.toks))
	for i, tok := range p.toks {
		rs := tok.runs.Snapshot()
		ss := tok.session.Snapshot()
		out = append(out, TokenSnapshot{
			Token:                i,
			CooldownUntil:        rs.CooldownUntil,
			ActiveRuns:           rs.ActiveRuns,
			Requests:             rs.Requests,
			SessionStatus:        ss.Status,
			SessionInstanceID:    ss.InstanceID,
			SessionQueuePosition: ss.QueuePosition,
			SessionQueueDepth:    ss.QueueDepth,
		})
	}
	return out
}

// --- internals ---

// prewarm starts a run for every agent on every token, best-effort, bounded
// by the request timeout.
func (p *Pool) prewarm(ctx context.Context, agentIDs []string) {
	defer p.wg.Done()
	for i, tok := range p.toks {
		preCtx, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
		tok.runs.Prewarm(preCtx, agentIDs)
		cancel()
		p.logger.Debug("pool: prewarm done", "token", i+1, "agents", len(agentIDs))
	}
}

// maintainLoop ticks every maintainInterval: per token, rotate aged runs and
// refresh the session (advances queued sessions past pollAt).
func (p *Pool) maintainLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(maintainInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i, tok := range p.toks {
				mCtx, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
				tok.runs.Maintain(mCtx)
				// Advance queued sessions only (GET poll — zero quota cost). Session
				// creation stays lazy on first request: a scheduled POST here would
				// burn one of the ~6 daily admissions every hour of uptime.
				if snap := tok.session.Snapshot(); snap.Status == "queued" {
					if _, err := tok.session.EnsureSession(mCtx); err != nil {
						p.logger.Debug("pool: maintain session not ready", "token", i+1, "err", err)
					}
				}
				cancel()
			}
		}
	}
}

// bestWaitingRoom picks the queue entry with the lowest position; ties break
// on the lowest queue depth (PRD §3: best-waiting-room-position selection).
func bestWaitingRoom(entries []*session.WaitingRoomError) *session.WaitingRoomError {
	best := entries[0]
	for _, candidate := range entries[1:] {
		if betterWait(candidate, best) {
			best = candidate
		}
	}
	return best
}

// betterWait reports whether a outranks b. Positions <= 0 mean "unknown" and
// rank below any known position (mirrors freebuff2api-quorinex).
func betterWait(a, b *session.WaitingRoomError) bool {
	if b == nil {
		return true
	}
	if a.Position <= 0 {
		return false
	}
	if b.Position <= 0 {
		return true
	}
	if a.Position != b.Position {
		return a.Position < b.Position
	}
	return a.QueueDepth < b.QueueDepth
}

// asRateLimit extracts a RateLimitError from err (nil when absent).
func asRateLimit(err error) *upstream.RateLimitError {
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle
	}
	return nil
}

// bestRateLimit picks the rate-limit error with the longest retry
// window (the token that unblocks last bounds the wait).
func bestRateLimit(entries []*upstream.RateLimitError) *upstream.RateLimitError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.RetryAfter > best.RetryAfter {
			best = e
		}
	}
	return best
}
