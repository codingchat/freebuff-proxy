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
//     when every token fails, the pool surfaces the highest-precedence
//     non-empty error bucket (ban > country-blocked > model-IP-limited >
//     rate-limit > waiting-room > daily cap) instead of a generic 502 — a
//     queued token surfaces 503 + Retry-After as soon as no higher bucket
//     is populated.
//   - run-invalid / session-invalid recoveries are NOT handled here: the
//     caller (server) retries once via a fresh Acquire after invalidating.
//   - anything else → next token; all failed → combined error (only when no
//     error-bucket matched any token).
package pool

import (
	"context"
	cryptoRand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/notify"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// maintainInterval is how often the background job rotates aged runs and
// advances queued sessions (PRD §3: 60s maintain ticker). Session-liveness
// polls run on their own jittered schedule (see sessionPoll* below), not on
// this coarse grid.
const maintainInterval = time.Minute

// Session-liveness poll cadence (gap #2; reference/freebuff sdk
// polling-backoff.ts): while active the CLI polls the compact session every
// 30s ±20% (24–36s), capped to remaining+1s near expiry so the poll lands
// just after expires_at; on failure it backs off 20s → 300s (×2 per
// consecutive failure), never scheduling a retry before the server's
// Retry-After floor.
const (
	// sessionPollCheckInterval is the maintain loop's fine-grained wake-up
	// grid for due session polls; rotation/queued-advance stay on
	// maintainInterval.
	sessionPollCheckInterval = 2 * time.Second
	// sessionPollBaseInterval is the CLI's active poll cadence (30s).
	sessionPollBaseInterval = 30 * time.Second
	// sessionPollBackoffBase is the first failure backoff (20s); each
	// consecutive failure doubles it up to sessionPollBackoffMax (300s).
	sessionPollBackoffBase = 20 * time.Second
	sessionPollBackoffMax  = 300 * time.Second
)

// usageWindow is the rolling window for the per-token daily message cap
// (MAX_MESSAGES_PER_DAY): a token may send at most N successful chat
// requests per 24h of usage history.
const usageWindow = 24 * time.Hour

// shutdownTimeout bounds each token's Shutdown during Pool.Shutdown when the
// caller's context carries no earlier deadline.
const shutdownTimeout = 10 * time.Second

// maxBridgeEntries caps the in-memory bridge cache: one entry (upstream
// client + session manager + run manager) per distinct client token. LRU
// eviction makes room when the cap is exceeded.
const maxBridgeEntries = 32

// bridgeIdleEvict is how long a bridge entry may sit unused before the
// maintain loop FINISHes its runs and drops it from the cache.
const bridgeIdleEvict = 2 * time.Hour

// Lease is one acquired right to send a chat request through a specific
// token. The caller must call Pool.LeaseRelease when the request completes
// or fails (it decrements the run's inflight counter).
type Lease struct {
	Token             int    // index into config.AuthTokens (-1 for bridge leases)
	Model             string // the model this lease's session/run is bound to (authoritative for opts.Model; may differ from the requested model after #100 fallback)
	AgentID           string
	Run               *runs.Run
	SessionInstanceID string       // "" when the session is disabled
	Bridge            *bridgeEntry // nil for pooled (fixed-token) leases
	// entry is the fixed-token entry backing this lease, pinned by Acquire so
	// LeaseRelease always releases through the right run manager regardless of
	// the lease's Token index.
	entry *tokenEntry
	// AcquiredAt is when this lease was handed out (per acquire attempt,
	// not per run — a chat retry re-acquires and gets a fresh timestamp).
	// The chat success path uses it to clear unfit marks that PREDATE this
	// admission (a retry's fresh acquire proves the mark stale, while an
	// older in-flight chat's success must not erase a mark that landed
	// after its admission).
	AcquiredAt time.Time
}

// bridgeEntry is one lazily-created client-token slot in bridge mode: the
// upstream client, session manager, and run manager for a single client-
// supplied token, created on first use and reused across that client's
// later requests. lastUsed and usage are guarded by Pool.bridgeMu.
type bridgeEntry struct {
	token    string
	client   *upstream.Client
	session  *session.Manager
	runs     *runs.RunManager
	lastUsed time.Time
	usage    []time.Time // rolling 24h successful-chat timestamps (MAX_MESSAGES_PER_DAY)
	// spend is the per-client-token spend ledger (issue #87); guarded by
	// Pool.bridgeMu like usage.
	spend *spendLedger
	// nextPollAt / pollFailures carry the session-liveness poll schedule
	// (gap #2), touched only by the maintain goroutine (bridgeSessionPollTick).
	nextPollAt   time.Time
	pollFailures int
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
	Messages24h          int    // successful chats in the last 24h (MAX_MESSAGES_PER_DAY usage)
	DailyLimit           int    // configured MAX_MESSAGES_PER_DAY (0 = unlimited)
	UsagePct             int    // percentage of daily limit used (0 when unlimited)
	RiskLevel            string // "low", "moderate", "high", "critical" account safety indicator (#6)
	// Spend24h / SpendDay / SpendWeek / SpendMonth are the local per-token
	// spend ledger (issue #87/#122): tokens spent in the rolling 24h window
	// and the current Pacific day/week/month buckets (with rollover —
	// boundaries are America/Los_Angeles wall-clock, DST-correct). Fed by
	// pool.RecordSpend from chat usage blocks; surfaced next to Messages24h.
	Spend24h        int64
	SpendDay        int64
	SpendWeek       int64
	SpendMonth      int64
	SpendDayStart   time.Time
	SpendWeekStart  time.Time
	SpendMonthStart time.Time
	// SpendLimit is the configured MAX_SPEND_PER_DAY ADVISORY ceiling in
	// ledger units (0 = unlimited). Never enforced: the upstream $ ceilings
	// ($15 full / $5 limited / $0.50 restricted, server-enforced, issue
	// #122) are the real gate and the proxy cannot know the account's
	// restricted cohort. SpendPct is the Pacific-day bucket's percentage of
	// SpendLimit (0 when unlimited). SpendLimited counts upstream
	// spend_limited refusals observed for this token since process start.
	SpendLimit   int64
	SpendPct     int
	SpendLimited int
	// CountryCode / CountryBlockReason are the token's last known upstream
	// region-block state. CountryBlockReason is non-empty when the account
	// (or its egress region) is blocked; surfaced by /v1/models availability
	// annotation and healthz.
	CountryCode        string
	CountryBlockReason string
	// SessionActiveUsersForIP is the last known distinct-user count on the
	// token's egress IP (upstream activeUsersForIp); zero when the session
	// response did not carry it.
	SessionActiveUsersForIP int
	// QuotaByModel is the live per-model session quota from the last
	// admission (key = model id); empty until the session reports it.
	// Entitlement is a top-level per-token view (empty: the upstream wire
	// nests entitlement inside each rate-limit entry).
	QuotaByModel map[string]session.QuotaSnapshot
	Entitlement  map[string]float64
	// Standing is the upstream account standing block (issue #96); nil until
	// the session reports it.
	Standing *upstream.SessionStanding
	// TransientRetries / FingerprintRotations are this token's upstream
	// client counters (TRANSIENT_RETRIES): retried transport failures and
	// pinned TLS fingerprint swaps. Surfaced per-token in /metrics.
	TransientRetries     int64
	FingerprintRotations int64
	// RateLimitEvents is this token's upstream rate-limit classification
	// ledger (T7), keyed by upstream body code (rate_limited, ip_capped,
	// spend_limited, insufficient_quota, limit_burst_rate,
	// free_mode_rate_limited, ...). Surfaced per-token in /metrics.
	RateLimitEvents map[string]int64
}

// Pool balances requests across the configured tokens.
type Pool struct {
	// cfg is the pool's write-once configuration, set at construction. The
	// pool is CLI-only: there is no runtime config reload, so every reader
	// reads the field directly.
	cfg *config.Config
	reg *registry.Registry
	// toks is the fixed-token list, built once at construction. It never
	// changes after New: readers index it directly instead of re-loading a
	// swappable snapshot.
	toks []*tokenEntry

	rr     atomic.Uint64 // round-robin start index
	logger *slog.Logger

	// requestsServed counts successful upstream chat calls across BOTH
	// pooled and bridge leases (bridge entries are ephemeral and excluded
	// from the per-token counters, so this is the mode-independent total).
	requestsServed atomic.Uint64

	once   sync.Once
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Usage tracking for MAX_MESSAGES_PER_DAY: one timestamp per successful
	// upstream chat, per token. Guarded by usageMu.
	usageMu      sync.Mutex
	msgsPerToken [][]time.Time

	// createGate bounds concurrent session admissions (issue #86): per-model
	// and global in-flight create counters with wait-or-503, wired from
	// SESSION_CREATE_MAX_PARALLEL_GLOBAL/PER_MODEL.
	gate *createGate

	// Spend ledger (issue #87): per-token token spend, rolling 24h window
	// plus day/week/month buckets with rollover. Guarded by spendMu;
	// spendPerToken stays index-aligned with msgsPerToken (both are built
	// once alongside the fixed-token list).
	spendMu       sync.Mutex
	spendPerToken []*spendLedger

	// Idle rotation (IDLE_ROTATION_TIMEOUT): last successful Acquire and
	// whether the maintain loop already FINISHed all runs for the current
	// idle stretch. Guarded by lastActiveMu.
	lastActiveMu sync.Mutex
	lastActive   time.Time
	idleFinished bool

	// Bridge mode (no AUTH_TOKENS): lazily-created per-client-token entries.
	// bridgeOrder keeps the LRU order, oldest first. Guarded by bridgeMu.
	bridgeMu    sync.Mutex
	bridge      map[string]*bridgeEntry
	bridgeOrder []string

	// bridgeCreateGate bounds concurrent bridge client creation (B1):
	// upstream.New involves network calls; limiting concurrency to 4
	// prevents thundering-herd creation when many new client tokens
	// arrive simultaneously.
	bridgeCreateGate chan struct{}

	// bridgeDailyUsage tracks the total number of successful chats across
	// ALL bridge entries for the BRIDGE_DAILY_LIMIT global cap (B5).
	// Guarded by bridgeMu.
	bridgeDailyUsage int

	// unfit is the per-(egress, model) unfit registry (issue #74 P2): models
	// refused upstream with limited_ip on this egress are marked unfit for
	// modelUnfitTTL so new requests are refused fast (409 model_ip_limited)
	// and re-admission does not burn a daily session slot. The server guards
	// NEW requests against it; Acquire deliberately does NOT consult it (the
	// chat recovery loop re-acquires through the plain acquire closure and
	// must reach a different token in mixed pools). Guarded by unfitMu.
	unfitMu sync.Mutex
	unfit   map[unfitKey]unfitEntry

	// store persists session state across restarts (SESSION_PERSIST); nil
	// disables. Injected by the caller (main) via SetSessionStore so there
	// is exactly one store shared by pooled and bridge entries.
	store *session.Store

	// notify fires best-effort webhook alerts (issue #48): pool_exhausted
	// when every token is rate-limited, token_banned when a ban is
	// classified. nil disables. Wired by main from WEBHOOK_URL.
	notify   *notify.Sender
	notifyMu sync.Mutex // guards notify reads/writes (P2-1 data race)
}

type tokenEntry struct {
	session *session.Manager
	runs    *runs.RunManager
	client  *upstream.Client

	// Session-liveness poll schedule (gap #2): nextPollAt is when the next
	// compact poll is due (zero = due on the next sessionPollTick pass);
	// pollFailures counts consecutive poll failures for the 20s→300s backoff.
	// Touched only by the maintain goroutine (sessionPollTick), so no lock is
	// needed.
	nextPollAt   time.Time
	pollFailures int
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

	p := &Pool{reg: reg, logger: slog.Default(), bridge: make(map[string]*bridgeEntry), unfit: make(map[unfitKey]unfitEntry), bridgeCreateGate: make(chan struct{}, 4)}
	p.cfg = cfg
	p.msgsPerToken = make([][]time.Time, len(cfg.AuthTokens))
	p.spendPerToken = make([]*spendLedger, len(cfg.AuthTokens))
	for i := range p.spendPerToken {
		p.spendPerToken[i] = newSpendLedger()
	}
	p.gate = newCreateGate(cfg.SessionCreateMaxParallelGlobal, cfg.SessionCreateMaxParallelPerModel)
	toks := make([]*tokenEntry, 0, len(cfg.AuthTokens))
	for i := range cfg.AuthTokens {
		sess := sessions[i]
		sess.SetReAdmitLead(cfg.SessionReAdmitLead)
		sess.SetAdmissionProbeTTL(cfg.SessionProbeCacheTTL)
		toks = append(toks, &tokenEntry{
			session: sess,
			runs:    runs.NewRunManagerOpts(clients[i], sess, runOptions(cfg)),
			client:  clients[i],
		})
	}
	p.toks = toks
	return p, nil
}

// runOptions maps config knobs to the run manager's Options (issues
// #90/#55): the bounded finish queue and draining-list bounds.
func runOptions(cfg *config.Config) runs.Options {
	return runs.Options{
		RotationInterval:    cfg.RotationInterval,
		FinishQueueSize:     cfg.RunFinishQueueSize,
		InlineFinishTimeout: cfg.RunFinishInlineTimeout,
		DrainQueueCap:       cfg.RunsDrainQueueCap,
		DrainTTL:            cfg.RunsDrainTTL,
	}
}

// TokenCount returns the current fixed-token count.
func (p *Pool) TokenCount() int {
	return len(p.toks)
}

// SetSessionStore injects the shared session-state store used by the pool's
// fixed-token and bridge entries. Call before the pool starts serving
// requests; the fixed-token session managers are built by the caller and
// must use the same store instance. A nil store disables persistence.
func (p *Pool) SetSessionStore(store *session.Store) {
	p.store = store
	// Issue #40: run persistence rides the same store. The fixed-token run
	// managers were built before the store existed (SetSessionStore runs
	// after New), so inject it here.
	for _, tok := range p.toks {
		tok.runs.SetStore(store)
	}
}

// SetNotifier wires the best-effort webhook sender (issue #48, WEBHOOK_URL);
// nil disables alerts. Safe to call at runtime (nil-friendly).
func (p *Pool) SetNotifier(n *notify.Sender) {
	p.notifyMu.Lock()
	defer p.notifyMu.Unlock()
	p.notify = n
}

// Chat sends a chat-completion request through the leased token's upstream
// client, returning the raw SSE body reader on 2xx. The caller must release
// the lease (LeaseRelease) once the request completes or fails, and close
// the returned body.
// multi-token failover: a single token either yields a lease or its error
// is returned as-is. Registry misses pass through.
func (p *Pool) Chat(ctx context.Context, lease *Lease, opts upstream.ChatOptions, body []byte) (io.ReadCloser, error) {
	if lease == nil {
		return nil, errors.New("pool: chat: invalid lease")
	}
	if lease.Bridge != nil {
		rc, err := lease.Bridge.client.ChatCompletions(ctx, opts, body)
		if err == nil {
			// Only chats that actually went upstream count against the
			// daily cap; errors are not recorded.
			p.bridgeRecordChat(lease.Bridge)
			p.requestsServed.Add(1)
		}
		return rc, err
	}
	// Fixed-token leases dispatch through their backing entry — the
	// authoritative owner pinned by Acquire: the entry is a stable pointer
	// that always dispatches to the right account, regardless of the lease's
	// Token index.
	if lease.entry != nil {
		rc, err := lease.entry.client.ChatCompletions(ctx, opts, body)
		if err == nil {
			p.recordChatEntry(lease.entry)
			p.requestsServed.Add(1)
		}
		return rc, err
	}
	// Synthetic leases without an entry keep the historical index path.
	toks := p.toks
	if lease.Token < 0 || lease.Token >= len(toks) {
		return nil, errors.New("pool: chat: invalid lease token")
	}
	rc, err := toks[lease.Token].client.ChatCompletions(ctx, opts, body)
	if err == nil {
		p.recordChat(lease.Token)
		p.requestsServed.Add(1)
	}
	return rc, err
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

// --- internals ---

// idleFor is how long the pool has gone without a successful Acquire (0
// when no request ever arrived, so a freshly prewarmed pool is not treated
// as idle).
func (p *Pool) idleFor() time.Duration {
	p.lastActiveMu.Lock()
	defer p.lastActiveMu.Unlock()
	if p.lastActive.IsZero() {
		return 0
	}
	return time.Since(p.lastActive)
}

// setIdleFinishedOnce marks the idle FINISH as done and reports whether
// this call performed it (false when it was already done). The next
// Acquire success resets the flag.
func (p *Pool) setIdleFinishedOnce() bool {
	p.lastActiveMu.Lock()
	defer p.lastActiveMu.Unlock()
	if p.idleFinished {
		return false
	}
	p.idleFinished = true
	return true
}

// prewarm starts a run for every agent on every token, best-effort, bounded
// by the request timeout.
func (p *Pool) prewarm(ctx context.Context, agentIDs []string) {
	defer p.wg.Done()
	toks := p.toks
	for i, tok := range toks {
		preCtx, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
		tok.runs.Prewarm(preCtx, agentIDs)
		cancel()
		p.logger.Debug("pool: prewarm done", "token", i+1, "agents", len(agentIDs))
	}
}

// maintainLoop ticks every maintainInterval: per token, rotate aged runs and
// advance queued sessions. Session-liveness polls run on their own finer
// jittered schedule (sessionPollTick fires when a token's nextPollAt is
// due; see the sessionPoll* constants). When IDLE_ROTATION_TIMEOUT is set,
// the pool pauses this activity after it has been idle past the timeout:
// one pass FINISHes all runs (so no rotation/session-refresh activity
// continues upstream) and every further pass is skipped until the next
// request — Acquire re-creates runs on demand.
func (p *Pool) maintainLoop(ctx context.Context) {
	defer p.wg.Done()
	ticker := time.NewTicker(maintainInterval)
	defer ticker.Stop()
	// The poll grid is finer than maintainInterval so the per-token jittered
	// ~30s liveness polls (gap #2) are not quantized onto the 60s rotation
	// grid — a due poll fires on the first grid point at/after nextPollAt.
	pollTicker := time.NewTicker(sessionPollCheckInterval)
	defer pollTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.maintainTick(ctx)
		case <-pollTicker.C:
			p.sessionPollTick(ctx)
		}
	}
}

// maintainTick runs one maintenance pass: the idle handling (see
// maintainLoop), then the per-token rotate/refresh work. Split out of
// maintainLoop so tests can drive a pass without waiting for the
// minute-long ticker.
func (p *Pool) maintainTick(ctx context.Context) {
	toks := p.toks
	cfg := p.cfg
	if cfg.IdleRotationTimeout > 0 && p.idleFor() > cfg.IdleRotationTimeout {
		// Past the idle threshold. If this is the first idle pass, FINISH
		// every run so the token's rotation/refresh activity stops
		// upstream; sessions are left untouched. Later passes skip the
		// per-token work entirely while the pool stays idle.
		if !p.setIdleFinishedOnce() {
			// Later idle passes still sweep idle bridge entries: without
			// this, entries idle past bridgeIdleEvict are never evicted
			// while the pool stays idle and their sessions stay admitted
			// upstream until expiry.
			p.bridgeMaintain(ctx, true)
			return
		}
		for _, tok := range toks {
			// Skip tokens with outstanding leases: FINISHing this run
			// would kill an in-flight chat; leave it for rotation once the
			// lease drains (same rule as the bridge idle sweep).
			if tok.runs.InflightCount() > 0 {
				continue
			}
			// Thread the maintain ctx: Pool.Shutdown cancels it first, so a
			// mid-drain FINISH must abort on cancel instead of blocking
			// shutdown for the full upstream call timeout.
			tok.runs.FinishAllRuns(ctx)
		}
		p.bridgeMaintain(ctx, true)
		return
	}
	for i, tok := range toks {
		// Cooldown: skip all per-token maintain work (rotate, draining
		// FINISH, queued-session advance). Upstream calls during a cooldown
		// look like abuse; the skip is silent — the cooldown itself is
		// already surfaced elsewhere (Acquire logs the skip).
		if time.Now().Before(tok.runs.CooldownUntil()) {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		tok.runs.Maintain(mCtx)
		// Advance queued sessions (GET poll). Skipped while a chat is in
		// flight: the upstream allows one client per account at a time, and
		// a poll GET that lands mid-chat can kick the active session (428
		// waiting_room). Mirror the reference session manager's in-flight
		// gate (reference/freebuff-proxy-hengxin session-manager.js:37-49,
		// 259-260). Active-session liveness polls are NOT part of this pass
		// — they run on the jittered sessionPollTick schedule (gap #2).
		if tok.runs.InflightCount() == 0 {
			snap := tok.session.Snapshot()
			if snap.Status == "queued" {
				if _, err := tok.session.EnsureSession(mCtx); err != nil {
					p.logger.Debug("pool: maintain session not ready", "token", i+1, "err", err)
				} else {
					// Issue #90a: the queue advanced to active — pre-create
					// the run for the session's model agent so the first
					// request on this session does not pay the START latency.
					after := tok.session.Snapshot()
					if agentID, err := p.reg.AgentForModel(after.Model); err == nil && agentID != "" {
						_ = tok.runs.Precreate(mCtx, agentID)
					}
				}
			}
		}
		cancel()
	}
	// Bridge sweep: drop entries idle past bridgeIdleEvict (runs FINISHed
	// best-effort), maintain the rest like the fixed tokens above.
	p.bridgeMaintain(ctx, false)
}

// sessionPollTick runs the per-token session-liveness polls on their own
// jittered schedule (see the sessionPoll* constants): an active (or
// in-grace ended) session is compact-polled every ~30s ±20% — capped to
// remaining+1s near expiry — with 20s→300s failure backoff honoring the
// server's Retry-After, mirroring the CLI's liveness fingerprint (gap #2;
// reference/freebuff sdk polling-backoff.ts). Rotation and queued-session
// advance stay on the coarse maintainInterval ticker (maintainTick). The
// poll is skipped while a chat is in flight (the upstream allows one client
// per account at a time; a poll landing mid-chat can kick the active
// session with 428) and while the token cools down, exactly like
// maintainTick.
func (p *Pool) sessionPollTick(ctx context.Context) {
	cfg := p.cfg
	if cfg.IdleRotationTimeout > 0 && p.idleFor() > cfg.IdleRotationTimeout {
		// Session polls pause with the fixed tokens while idle (the
		// maintain pass already FINISHed every run upstream).
		return
	}
	toks := p.toks
	for i, tok := range toks {
		if time.Now().Before(tok.runs.CooldownUntil()) {
			// Cooldown: no session poll (same rule as maintainTick).
			continue
		}
		if tok.runs.InflightCount() > 0 {
			// Mid-chat in-flight gate (same rule as maintainTick): a poll
			// GET can kick the active session (428 waiting_room). Leave the
			// schedule due; the next pass polls once the lease drains.
			continue
		}
		now := time.Now()
		if !tok.nextPollAt.IsZero() && now.Before(tok.nextPollAt) {
			continue
		}
		mCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		err := tok.session.Poll(mCtx)
		cancel()
		var delay time.Duration
		if err != nil {
			tok.pollFailures++
			delay = sessionPollBackoffDelay(tok.pollFailures, sessionPollRetryAfter(err))
			p.logger.Debug("pool: session poll failed", "token", i+1, "err", err, "retry_in", delay)
		} else {
			tok.pollFailures = 0
			delay = sessionPollSuccessDelay(tok.session.Snapshot())
		}
		tok.nextPollAt = time.Now().Add(delay)
	}
	p.bridgeSessionPollTick(ctx, cfg)
}

// sessionPollSuccessDelay returns the delay before the next liveness poll
// after a SUCCESSFUL poll: ~30s ±20% jitter, capped so a poll near expiry
// lands ~1s after expires_at (the CLI observes the status flip then;
// reference/freebuff sdk polling-backoff.ts). Sessions already inside the
// grace drain poll at the plain jittered cadence.
func sessionPollSuccessDelay(snap session.SessionSnapshot) time.Duration {
	d := sessionPollJittered(sessionPollBaseInterval)
	if !snap.ExpiresAt.IsZero() {
		if rem := time.Until(snap.ExpiresAt); rem > 0 && rem+time.Second < d {
			d = rem + time.Second
		}
	}
	return d
}

// sessionPollBackoffDelay returns the delay after a FAILED poll: 20s ×2 per
// consecutive failure (cap 300s) with equal jitter over the lower half of
// the window, and never before the server's Retry-After floor (multiplied
// by 1 ± 0.2 jitter, capped 300s) — polling-backoff.ts semantics.
func sessionPollBackoffDelay(failures int, retryAfter time.Duration) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := sessionPollBackoffBase << min(failures-1, 5)
	if d > sessionPollBackoffMax {
		d = sessionPollBackoffMax
	}
	d = d/2 + time.Duration(sessionRand()%uint64(d/2))
	if retryAfter > 0 {
		// Floor retryAfter to avoid uint64(0) modulo panic when the
		// server's Retry-After is absurdly small (1ns). P2-5.
		if retryAfter < 5*time.Nanosecond {
			retryAfter = 5 * time.Nanosecond
		}
		ra := retryAfter - retryAfter/5 + time.Duration(sessionRand()%uint64(2*retryAfter/5))
		if ra > d {
			d = ra
		}
		if d > sessionPollBackoffMax {
			d = sessionPollBackoffMax
		}
	}
	return d
}

// sessionPollJittered applies the CLI's symmetric ±20% jitter around d.
func sessionPollJittered(d time.Duration) time.Duration {
	span := d / 5
	return d - span + time.Duration(sessionRand()%uint64(2*span+1))
}

// sessionRand draws one uint64 from crypto/rand (the pool's jitter source,
// matching the upstream client's pattern). A read failure is unrecoverable
// in practice; fall back to the clock rather than panicking in a background
// loop.
func sessionRand() uint64 {
	var b [8]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b[:])
}

// sessionPollRetryAfter extracts the server's Retry-After floor from a
// failed session poll error (0 when the error carries none). The backoff
// never schedules a retry before this floor.
func sessionPollRetryAfter(err error) time.Duration {
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		return ue.RetryAfter
	}
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter
	}
	var wrr *upstream.WaitingRoomRequiredError
	if errors.As(err, &wrr) {
		return wrr.RetryAfter
	}
	var wr *session.WaitingRoomError
	if errors.As(err, &wr) {
		return wr.RetryAfter
	}
	return 0
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
// asRateLimit extracts a RateLimitError from err (nil when absent).
func asRateLimit(err error) *upstream.RateLimitError {
	var rle *upstream.RateLimitError
	if errors.As(err, &rle) {
		return rle
	}
	return nil
}

// asIpCapped extracts an IpCappedError from err (nil when absent).
func asIpCapped(err error) *upstream.IpCappedError {
	var ice *upstream.IpCappedError
	if errors.As(err, &ice) {
		return ice
	}
	return nil
}

// asBan extracts a BanError from err (nil when absent).
func asBan(err error) *upstream.BanError {
	var be *upstream.BanError
	if errors.As(err, &be) {
		return be
	}
	return nil
}

// asCountryBlocked extracts a CountryBlockedError from err (nil when
// absent).
func asCountryBlocked(err error) *upstream.CountryBlockedError {
	var cbe *upstream.CountryBlockedError
	if errors.As(err, &cbe) {
		return cbe
	}
	return nil
}

// asLimitedIp extracts a LimitedIpError from err (nil when absent).
func asLimitedIp(err error) *upstream.LimitedIpError {
	var lie *upstream.LimitedIpError
	if errors.As(err, &lie) {
		return lie
	}
	return nil
}

// bestRateLimit picks the rate-limit error with the shortest retry
// window (the token that unblocks earliest bounds the wait).
func bestRateLimit(entries []*upstream.RateLimitError) *upstream.RateLimitError {
	best := entries[0]
	for _, e := range entries[1:] {
		if e.RetryAfter < best.RetryAfter {
			best = e
		}
	}
	return best
}
