package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/runs"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/upstream"
)

// relayFunc relays the upstream SSE reader to the client in the endpoint's
// wire format (chat.completion chunks, Responses events, or Anthropic
// events). Implementations set their own headers, flush, and write terminal
// frames. chatStart is when the upstream chat call returned; the first
// relayed chunk records the upstream TTFB phase.
type relayFunc func(ctx context.Context, w http.ResponseWriter, up io.Reader, stats *relayStats, chatStart time.Time)

// handleChat is the OpenAI chat-completions entry point: sanitize the
// request, then route through chatCore with the chat wire format.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			s.writeJSONError(w, http.StatusRequestEntityTooLarge,
				"request body exceeds the 32MB limit", "invalid_request_error", "content_too_large", 0)
		} else {
			s.writeJSONError(w, http.StatusBadRequest,
				"failed to read request body: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		}
		return
	}

	// The raw map decides the response mode (stream) before sanitization.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	rawModel, _ := raw["model"].(string)
	if rawModel == "" {
		s.writeJSONError(w, http.StatusBadRequest,
			"missing required field \"model\"; available: "+strings.Join(s.reg.Models(), ", "),
			"invalid_request_error", "model_not_found", 0)
		return
	}
	model := s.reg.ResolveModel(rawModel)
	if !s.modelAllowed(model) {
		// MODELS_ALLOW: the resolved model (alias + -max upgrade applied)
		// is outside the operator allowlist — reject like an unknown model.
		s.writeJSONError(w, http.StatusNotFound,
			"model not allowed by MODELS_ALLOW", "invalid_request_error", "model_not_found", 0)
		return
	}
	stream := false
	if v, ok := raw["stream"].(bool); ok {
		stream = v
	}
	normalized, err := convert.NormalizeRequest(body, model)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest,
			"request body must be a valid JSON object: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	var relay relayFunc
	if stream {
		relay = s.relayStream
	} else {
		relay = s.relayJSON
	}
	s.chatCore(w, r, model, stream, normalized, convert.ExtractReasoningEffort(raw), "chat", relay)
}

// chatCore is the shared acquire→relay core for every completion-style
// endpoint (chat completions, Responses, Anthropic messages): acquire a
// token lease (bridge routing included), call upstream with
// retry-once recovery, then relay the forced stream to the client through
// relay. kind names the endpoint in request/done log lines.
func (s *Server) chatCore(w http.ResponseWriter, r *http.Request, model string, stream bool, normalized []byte, reasoningEffort, kind string, relay relayFunc) {
	// D1: the access wrapper minted the request's correlation id; direct
	// handler calls (tests) mint here so it is never empty. The value is
	// threaded into the request context AND into ChatOptions.RequestID so
	// the upstream client's do()/retry lines share it.
	reqID := reqIDFrom(r.Context())
	if reqID == "" {
		reqID = newReqID()
	}
	st := &chatTraceState{reqID: reqID, clientRequestID: clientRequestID(r)}
	ctx, phases := phasetiming.WithContext(context.WithValue(r.Context(), reqIDKey{}, reqID))
	start := time.Now()

	agentID, _ := s.reg.AgentForModel(model)
	reqAttrs := []any{
		"model", model,
		"agent", agentID,
		"stream", stream,
		"remote", remoteHost(r),
	}
	if reasoningEffort != "" {
		reqAttrs = append(reqAttrs, "reasoning_effort", reasoningEffort)
	}
	s.logger.Info(kind+" request", reqAttrs...)
	// Client-side rate limiting per source IP (issue #137): reject rapid-fire
	// bursts and spam locally before token lease acquisition or upstream calls.
	if allowed, retryAfter := s.rateLimiter.Allow(r.RemoteAddr); !allowed {
		phases.Since(phasetiming.TotalMS, start)
		retrySec := int(math.Ceil(retryAfter.Seconds()))
		if retrySec < 1 {
			retrySec = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retrySec))
		s.logger.Warn(kind+" rate limit exceeded",
			"remote", remoteHost(r),
			"req_id", reqID,
			"retry_after_sec", retrySec,
		)
		s.rateLimitRejections.Add(1)
		s.writeJSONError(w, http.StatusTooManyRequests,
			fmt.Sprintf("client rate limit exceeded (Retry-After: %ds)", retrySec),
			"rate_limit_exceeded", "rate_limit_exceeded", 0)
		return
	}
	// Bridge routing: bridge mode relays the client's Authorization header
	// as the upstream token.  No token in bridge → 401 before touching
	// the pool.
	var up io.ReadCloser
	var lease *pool.Lease
	cfg := s.cfg.Load()
	fallbackUsed := false
	tok := bearerToken(r)
	bridge := false
	switch {
	case cfg.BridgeMode():
		// Bridge: the client token is the only upstream credential.
		bridge = true
		tok = clientToken(r)
	}
	// Issue #74 P2: refuse new requests fast when (egress, model) is marked
	// unfit — the direct egress cannot serve this model for ~5 min. The
	// pooled path only: bridge clients relay their own token (the client's
	// own account may serve the model on this egress and their session
	// slots are theirs to spend), so the registry never gates them.
	// MarkModelUnfit always stores a LimitedIpError, so lie is non-nil in
	// practice; the bare sentinel keeps the refusal deterministic if it
	// ever is nil.
	if !bridge {
		if until, lie := s.pool.ModelUnfit(model); !until.IsZero() && time.Now().Before(until) {
			phases.Since(phasetiming.TotalMS, start)
			s.logger.Info(kind+" request refused", "model", model, "reason", "model_limited_on_egress", "until", until.Format(time.RFC3339))
			// Never mutate the registry's stored error (SEC-1): concurrent
			// refusals would race on RetryAfter. Surface a per-request
			// shallow copy carrying the computed window.
			refuseErr := upstream.ErrModelIPLimited
			if lie != nil {
				refuseErr = &upstream.LimitedIpError{Model: lie.Model, Body: lie.Body, RetryAfter: time.Until(until)}
			}
			s.traceChat(nil, model, time.Since(start).Milliseconds(), "error", "model_ip_limited", phases.All(), st)
			s.writeError(w, r, refuseErr, model, nil)
			return
		}
	}
	// Acquire is timed per call; on the retry-once path the last acquire
	// wins (that is the lease-producing one, matching the pool's
	// per-attempt session/run phases).
	acquireTimed := func(acquire func(context.Context, string) (*pool.Lease, error)) func(context.Context, string) (*pool.Lease, error) {
		return func(ctx context.Context, model string) (*pool.Lease, error) {
			acquireStart := time.Now()
			l, err := acquire(ctx, model)
			phases.Since(phasetiming.AcquireMS, acquireStart)
			return l, err
		}
	}
	var err error
	if bridge {
		if tok == "" {
			s.writeJSONError(w, http.StatusUnauthorized,
				"bridge mode: send your FreeBuff token as Authorization: Bearer <token> (no AUTH_TOKENS configured on the proxy)",
				"invalid_request_error", "missing_bearer_token", 0)
			return
		}
		up, lease, err = s.chatAttempt(ctx, model, normalized, st,
			acquireTimed(func(ctx context.Context, model string) (*pool.Lease, error) {
				return s.pool.AcquireBridge(ctx, tok, model)
			}),
			s.pool.Chat,
			s.pool.InvalidateBridgeSession,
			s.pool.InvalidateBridgeRun,
			func(l *pool.Lease) { s.pool.CooldownBridge(l, runs.DefaultCooldown) },
			s.pool.CooldownBridgeBan,
			s.pool.CooldownBridgeRateLimit,
			s.pool.CooldownBridgeIpCapped,
			s.pool.CooldownBridgeCountryBlocked,
		)
	} else {
		// Issue #100: bounded queue-time model fallback. When the request's
		// model has a configured fallback (FALLBACK_MODEL) and the pool
		// surfaces a waiting-room/queue delay of at least FALLBACK_AFTER_MS,
		// re-route the SAME token to the fallback model instead of handing
		// the client a 503 the client would have to wait out. Conservative:
		// only when a fallback is configured; the switch is surfaced to the
		// client via the X-FreeBuff-Fallback-Model response header and in
		// the routing log line.
		acquire := acquireTimed(func(ctx context.Context, model string) (*pool.Lease, error) { return s.pool.Acquire(ctx, model) })
		fallbackModel := cfg.FallbackModels[model]
		if cfg.FallbackAfter > 0 && fallbackModel != "" && fallbackModel != model {
			wrapped := acquire
			acquire = func(ctx context.Context, m string) (*pool.Lease, error) {
				l, err := wrapped(ctx, m)
				if err == nil || errors.Is(err, registry.ErrModelNotFound) {
					return l, err
				}
				var wr *session.WaitingRoomError
				if errors.As(err, &wr) && wr.RetryAfter >= cfg.FallbackAfter {
					s.logger.Info("model fallback: waiting room exceeds FALLBACK_AFTER_MS; switching model",
						"model", m, "fallback", fallbackModel, "retry_after", wr.RetryAfter.String())
					// Drop the queued session caches so the fallback-model
					// acquire can CREATE a fresh session instead of
					// re-surfacing the same waiting room (issue #100).
					if cleared := s.pool.ClearQueuedCaches(); cleared > 0 {
						s.logger.Debug("model fallback: cleared queued session caches", "cleared", cleared)
					}
					l2, err2 := wrapped(ctx, fallbackModel)
					if err2 == nil {
						fallbackUsed = true
					}
					return l2, err2
				}
				return l, err
			}
		}
		up, lease, err = s.chatAttempt(ctx, model, normalized, st,
			acquire,
			s.pool.Chat,
			func(l *pool.Lease) { s.pool.InvalidateSession(l.Token, l.SessionInstanceID) },
			func(l *pool.Lease, agentID string) { s.pool.InvalidateRun(l.Token, agentID) },
			func(l *pool.Lease) { s.pool.CooldownToken(l.Token, runs.DefaultCooldown) },
			func(l *pool.Lease, be *upstream.BanError) { s.pool.CooldownTokenBan(l.Token, be) },
			func(l *pool.Lease, rle *upstream.RateLimitError) { s.pool.CooldownTokenRateLimit(l.Token, rle) },
			func(l *pool.Lease, ice *upstream.IpCappedError) { s.pool.CooldownTokenIpCapped(l.Token, ice) },
			func(l *pool.Lease, cbe *upstream.CountryBlockedError) {
				s.pool.CooldownTokenCountryBlocked(l.Token, cbe)
			},
		)
	}
	if err != nil {
		phases.Since(phasetiming.TotalMS, start)
		s.traceChat(lease, model, time.Since(start).Milliseconds(), "error", chatErrClass(err), phases.All(), st)
		// Issue #114: a chat that died on a terminal upstream error must
		// not leave its run FINISHing as completed — report it honestly
		// (nil-safe: an acquire failure leaves no lease).
		s.pool.MarkRunFailed(lease)
		s.writeError(w, r, err, model, lease)
		return
	}
	// PREFER_MAX_MODELS gating: the admission just reported the token's
	// accessTier + provisioned model set (QuotaByModel keys from the
	// session snapshot); fold them into the runtime config so the next
	// request's ResolveModel gates -max upgrades for limited tokens and
	// refuses -max variants the account was not provisioned (upstream
	// provisions -max roots per-account — issue #140). First request for a
	// token still resolves with the previously-known (or env-set) tier.
	s.rememberAccessTier(lease.TierAccess, lease.ProvisionedModels)
	defer func() { _ = up.Close() }()
	// Issue #53: when the downstream client disconnects mid-stream, abandon
	// the lease instead of a plain release — the run is FINISHed through the
	// bounded queue (last-in-flight only) so upstream does not keep an
	// abandoned agent run alive until the 6h rotation. A normal completion
	// releases the lease as before.
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if r.Context().Err() != nil {
			s.pool.LeaseAbandon(lease)
			return
		}
		s.pool.LeaseRelease(lease)
	}
	defer release()

	routingAttrs := []any{
		"req_id", reqID,
		"token", tokenLabel(lease),
		"model", model,
		"agent", lease.AgentID,
		"instance_id", lease.SessionInstanceID,
		"tier", lease.TierAccess,
		"country", lease.TierCountry,
	}
	if reasoningEffort != "" {
		routingAttrs = append(routingAttrs, "reasoning_effort", reasoningEffort)
	}
	if fallbackUsed {
		// Surface the transparent model switch to the client (issue #100):
		// the streamed response itself is indistinguishable, so the header
		// is the notice.
		w.Header().Set("X-FreeBuff-Fallback-Model", cfg.FallbackModels[model])
		routingAttrs = append(routingAttrs, "fallback", cfg.FallbackModels[model])
	}
	s.logger.Info(kind+" routing", routingAttrs...)

	chatStart := time.Now()
	stats := &relayStats{}
	relay(ctx, w, up, stats, chatStart)
	// Issue #114: record the completed chat as a run step — steps are
	// batched in memory and sent WITH FINISH (the CLI has no /steps
	// endpoint). The response message id is not extracted from the stream;
	// the CLI step schema allows a null messageId.
	s.pool.RecordRunStep(lease, "")
	// Issue #122: feed the per-token spend ledger once per successful chat
	// completion with the usage total observed by the relay (0 when the
	// upstream stream carried none — RecordSpend ignores non-positive).
	s.pool.RecordSpend(lease, stats.usageTokens)
	phases.Since(phasetiming.TotalMS, start)
	ms := time.Since(start).Milliseconds()
	s.logger.Info(kind+" done", chatDoneAttrs(reqID, model, lease.AgentID, stream, ms, stats.chunks, stats.bytes, reasoningEffort)...)
	s.traceChat(lease, model, ms, "ok", "", phases.All(), st)
}

// provisionedSet derives the set of model ids upstream provisioned for the
// probed token from a probe session state's RateLimitsByModel map (key =
// model id). Absent/empty map → nil (unknown, tier gate alone decides).
// Mirrors pool.provisionedFromQuota for the probe path.
func provisionedSet(st *upstream.SessionState) map[string]bool {
	if st == nil || len(st.RateLimitsByModel) == 0 {
		return nil
	}
	out := make(map[string]bool, len(st.RateLimitsByModel))
	for id := range st.RateLimitsByModel {
		out[id] = true
	}
	return out
}

// rememberAccessTier folds an upstream session-probe/admission-observed
// accessTier ("full"/"limited") into the runtime config — s.cfg plus the
// registry and pool copies, mirroring the reload triple-store — so
// ResolveModel's -max upgrade gate consults it on the next request. A probe
// observation never overrides an operator-set ACCESS_TIER
// (AccessTierExplicit); empty tiers are ignored (unknown tier keeps the
// current value, so a fresh config still treats empty as full).
//
// When provisioned is non-empty it is ALSO folded in as the account's
// provisioned model set (rateLimitsByModel keys from the same response):
// the -max upgrade gate then refuses variants absent from the set, which is
// the accurate per-account signal — upstream provisions -max roots
// per-account, so most full-tier tokens have only base models and would
// otherwise trip 403 free_mode_invalid_agent_model on every upgrade
// (issue #140).
func (s *Server) rememberAccessTier(tier string, provisioned map[string]bool) {
	tier = strings.TrimSpace(tier)
	if tier == "" && len(provisioned) == 0 {
		return
	}
	cur := s.cfg.Load()
	next := *cur
	if tier != "" && !cur.AccessTierExplicit {
		next.AccessTier = tier
	}
	if len(provisioned) > 0 {
		next.ProvisionedModels = provisioned
	}
	if next.AccessTier == cur.AccessTier && next.ProvisionedModels == nil {
		return
	}
	s.cfg.Store(&next)
	s.reg.SetConfig(&next)
	s.pool.SetConfig(&next)
}

// traceChat records a structured "chat trace" entry for the dashboard
// traces page (the page filters the shared log ring by msg == "chat trace").
// phases carries the per-request latency phases (#89); the map is ordered
// deterministically for stable log output. st carries the retry-once
// attempt history (nil-safe: a refusal before any chat attempt passes a
// zero state).
func (s *Server) traceChat(lease *pool.Lease, model string, ms int64, status, errClass string, phases map[string]int64, st *chatTraceState) {
	attrs := []any{"model", model, "status", status, "ms", ms}
	if st != nil {
		if st.reqID != "" {
			attrs = append(attrs, "req_id", st.reqID)
		}
		if st.clientRequestID != "" {
			attrs = append(attrs, "client_request_id", st.clientRequestID)
		}
		if st.attempts > 0 {
			attrs = append(attrs, "attempts", st.attempts)
		}
		if seen := st.statusesSeen(); seen != "" {
			attrs = append(attrs, "statuses_seen", seen)
		}
		if st.retried {
			attrs = append(attrs, "retried", true, "backoff_ms", st.backoffMs)
		}
	}
	if lease != nil {
		attrs = append(attrs,
			"token", tokenLabel(lease),
			"agent", lease.AgentID,
			"trace_session_id", lease.Run.TraceSessionID,
		)
	}
	if errClass != "" {
		attrs = append(attrs, "error", errClass)
	}
	for _, name := range []string{
		phasetiming.AcquireMS,
		phasetiming.SessionRefreshMS,
		phasetiming.RunAcquireMS,
		phasetiming.UpstreamTTFBMS,
		phasetiming.TotalMS,
	} {
		if v, ok := phases[name]; ok {
			attrs = append(attrs, name, v)
		}
	}
	s.logger.Info("chat trace", attrs...)
}

// chatErrClass buckets an upstream error into the trace error column.
func chatErrClass(err error) string {
	switch err.(type) {
	case *upstream.RateLimitError:
		return "rate_limited"
	case *upstream.BanError:
		return "banned"
	case *upstream.IpCappedError:
		return "ip_capped"
	case *upstream.LimitedIpError:
		return "model_ip_limited"
	case *upstream.SessionLimitError:
		return "session_limit_reached"
	case *upstream.WaitingRoomError, *session.WaitingRoomError, *upstream.WaitingRoomRequiredError:
		return "waiting_room"
	case *upstream.SessionSupersededError:
		return "session_superseded"
	case *upstream.UpstreamError:
		return "upstream"
	default:
		return "error"
	}
}

// chatDoneAttrs builds the structured log attributes for a completed chat,
// including reasoning effort when the client requested it.
func chatDoneAttrs(reqID, model, agent string, stream bool, ms int64, chunks, bytes int, reasoningEffort string) []any {
	attrs := []any{
		"req_id", reqID,
		"model", model,
		"agent", agent,
		"stream", stream,
		"ms", ms,
		"bytes", bytes,
	}
	if stream {
		attrs = append(attrs, "chunks", chunks)
	}
	if reasoningEffort != "" {
		attrs = append(attrs, "reasoning_effort", reasoningEffort)
	}
	return attrs
}

// chatTraceState accumulates the per-request attempt history for the chat
// trace line: how many upstream chat attempts fired, the HTTP statuses
// observed per attempt (success = 200), whether the retry-once recovery
// re-acquired a lease, and the measured re-acquire wait before the retry.
// Created in chatCore (which owns the req_id), filled by chatAttempt's
// retry loop.
type chatTraceState struct {
	reqID           string
	clientRequestID string
	attempts        int
	statuses        []int
	retried         bool
	backoffMs       int64
}

// statusesSeen renders the observed attempt statuses comma-joined
// ("409,200"), or "" when no attempt status was observed.
func (st *chatTraceState) statusesSeen() string {
	if len(st.statuses) == 0 {
		return ""
	}
	parts := make([]string, len(st.statuses))
	for i, s := range st.statuses {
		parts[i] = strconv.Itoa(s)
	}
	return strings.Join(parts, ",")
}

// attemptStatus extracts the upstream HTTP status carried by a chat error,
// or 0 when the error carries none (wrapped sentinels such as
// ErrSessionInvalid/ErrRunInvalid, and transport-level failures). A 0 is
// skipped in statuses_seen — only observed statuses are listed.
func attemptStatus(err error) int {
	switch e := err.(type) {
	case *upstream.UpstreamError:
		return e.Status
	case *upstream.CreditsError:
		return e.Status
	case *upstream.CapacityDeferredError:
		return e.Status
	case *upstream.SessionSupersededError:
		return e.Status
	case *upstream.SessionLimitError:
		return e.Status
	case *upstream.WaitingRoomRequiredError:
		// The canonical 428 waiting_room_required (#94); the marker can
		// ride 428/429 alike, 428 is the documented gate. No named
		// net/http constant exists for 428, so spell it out.
		return 428
	case *upstream.RateLimitError:
		// RateLimitError.Status is the upstream "429" string; parse when
		// numeric, else the 429 bucket is implicit.
		if n, perr := strconv.Atoi(e.Status); perr == nil {
			return n
		}
		return http.StatusTooManyRequests
	}
	return 0
}

// chatAttempt runs the retry-once recovery loop for one chat request: chat
// through the leased token; on session-invalid / run-invalid the lease is
// released, the cached session/run invalidated, and a fresh lease acquired
// once; on auth-reject / ban / rate-limit / ip-capped the token is cooled
// down (ip_capped bounded to its retryAfterMs — never the Pacific-midnight
// lock) and the error returned for writeError. The acquire/chat/invalidate/
// cooldown hooks are closures so the pooled (fixed-token) and bridge paths
// share the exact same recovery semantics. On success the returned body
// reader and final lease belong to the caller: close the body and release
// the lease via Pool.LeaseRelease when done.
func (s *Server) chatAttempt(
	ctx context.Context,
	model string,
	normalized []byte,
	st *chatTraceState,
	acquire func(context.Context, string) (*pool.Lease, error),
	chat func(context.Context, *pool.Lease, upstream.ChatOptions, []byte) (io.ReadCloser, error),
	invalidateSession func(*pool.Lease),
	invalidateRun func(*pool.Lease, string),
	cooldownAuth func(*pool.Lease),
	cooldownBan func(*pool.Lease, *upstream.BanError),
	cooldownRate func(*pool.Lease, *upstream.RateLimitError),
	cooldownIpCapped func(*pool.Lease, *upstream.IpCappedError),
	cooldownCountry func(*pool.Lease, *upstream.CountryBlockedError),
) (io.ReadCloser, *pool.Lease, error) {
	lease, err := acquire(ctx, model)
	if err != nil {
		return nil, nil, err
	}

	// The lease is the authoritative source for the model its session/run
	// are bound to: after a #100 fallback the acquire returned a lease for
	// the FALLBACK model while the caller still holds the requested model.
	// opts.Model, the body model and x-freebuff-model must all agree with
	// the lease (review P2 — previously the request went upstream labeled
	// with the requested model against the fallback session/run).
	effectiveModel := lease.Model
	if effectiveModel == "" {
		effectiveModel = model
	}
	if effectiveModel != model {
		if renormalized, nerr := convert.NormalizeRequest(normalized, effectiveModel); nerr == nil {
			normalized = renormalized
		}
	}

	opts := upstream.ChatOptions{
		Model:             effectiveModel,
		RunID:             lease.Run.RunID,
		SessionInstanceID: lease.SessionInstanceID,
		TraceSessionID:    lease.Run.TraceSessionID,
		// D1: the request's correlation id, threaded to the upstream
		// client so its do()/retry log lines share the server's req_id.
		RequestID: st.reqID,
		// Issue #113: stamp the run's 1-based per-chat step counter so
		// codebuff_metadata["llm_step_number"] matches the CLI (each chat
		// call is one agent step; run-agent-step.ts increments per step).
		// Incremented once per chatAttempt — the retry-once loop below
		// retries the SAME step.
		StepNumber: int(lease.Run.NextStepNumber()),
	}

	released := false
	release := func() {
		if !released {
			released = true
			s.pool.LeaseRelease(lease)
		}
	}
	defer release()

	var up io.ReadCloser
	attempts := 0
	// failTime pins when the failed chat attempt returned; the measured
	// re-acquire wait below becomes the trace's backoff_ms.
	var failTime time.Time
	// transientErr remembers the default-branch chat error so the retry
	// announcement can log it AFTER the re-acquire (with a real backoff_ms).
	var transientErr error
	for {
		chatStart := time.Now()
		up, err = chat(ctx, lease, opts, normalized)
		attempts++
		st.attempts = attempts
		if err == nil {
			st.statuses = append(st.statuses, http.StatusOK)
			// Issue #74 P2: a successful chat is egress-level proof the
			// model is servable again — drop any (egress, model) unfit mark.
			// Only marks created before THIS lease's acquisition (a retry
			// re-acquires after the mark, so its success clears it; an
			// older in-flight chat succeeding must not erase a mark that
			// landed after its admission — that would reopen the
			// limited_ip re-admission burn).
			if !lease.AcquiredAt.IsZero() {
				s.pool.ClearModelUnfitBefore(effectiveModel, lease.AcquiredAt)
			}
			if attempts > 1 {
				// T13: the retry-once recovery landed — one Debug line that
				// greps the whole retry chain by req_id (ms = the retry
				// chat call's duration).
				s.logger.Debug("chat retry succeeded",
					"attempts", attempts, "req_id", st.reqID,
					"ms", time.Since(chatStart).Milliseconds())
			}
			released = true // Disarm deferred release: ownership transferred to caller
			return up, lease, nil
		}
		if s := attemptStatus(err); s != 0 {
			st.statuses = append(st.statuses, s)
		}
		failTime = time.Now()
		switch {
		case errors.Is(err, upstream.ErrModelIPLimited):
			// Issue #74 P2: the egress IP is limited for the requested
			// model. Mark (egress, model) unfit for ~5 min so new requests
			// refuse fast instead of re-admitting against a known-limited
			// gate (each admission burns a daily session slot). Retry once
			// through a fresh acquire — a different token (full-tier
			// account) may still serve the model. The session is bound to
			// its admitted model and is NOT invalidated.
			var lie *upstream.LimitedIpError
			if errors.As(err, &lie) {
				s.pool.MarkModelUnfit(effectiveModel, lie)
			} else {
				s.pool.MarkModelUnfit(effectiveModel, nil)
			}
			release()
			if attempts > 1 {
				return nil, nil, err
			}
		case errors.Is(err, upstream.ErrSessionInvalid):
			release()
			invalidateSession(lease)
			if attempts > 1 {
				return nil, nil, err
			}
		case errors.Is(err, upstream.ErrWaitingRoomRequired):
			// #116: 428 waiting_room_required is session-ENDING
			// (endsTheSession:true — the seat is gone mid-chat;
			// reference/freebuff freebuff-session.ts FREEBUFF_GATE_CODES).
			// Drop the cached session and re-admit ONCE for this request
			// (mirror the ErrSessionInvalid budget: attempts > 1 surfaces
			// the error; the WAITING_ROOM_CHAIN fires before the next
			// create). Never loops — a single reacquire, then surface.
			release()
			invalidateSession(lease)
			if attempts > 1 {
				return nil, nil, err
			}
		case errors.Is(err, upstream.ErrSessionSuperseded):
			// #119: 409 session_superseded — another instance took over
			// the account. Drop the cached session and re-admit ONCE
			// for this request (mirror the ErrSessionInvalid budget:
			// attempts > 1 surfaces the error). The session is already
			// invalidated so the next request re-joins fresh; auto-
			// retry here avoids the 30s model lock9router would apply
			// on a503 response. Never loops — a single reacquire, then
			// surface.
			release()
			invalidateSession(lease)
			if attempts > 1 {
				return nil, nil, err
			}
		case errors.Is(err, upstream.ErrRunInvalid):
			release()
			invalidateRun(lease, lease.AgentID)
			if attempts > 1 {
				return nil, nil, err
			}
		case errors.Is(err, upstream.ErrAuthRejected):
			cooldownAuth(lease)
			release()
			return nil, nil, err
		case errors.Is(err, upstream.ErrBanned):
			var be *upstream.BanError
			if errors.As(err, &be) {
				cooldownBan(lease, be)
			}
			release()
			return nil, nil, err
		case errors.Is(err, upstream.ErrRateLimited):
			var rle *upstream.RateLimitError
			if errors.As(err, &rle) {
				cooldownRate(lease, rle)
			}
			release()
			return nil, nil, err
		case errors.Is(err, upstream.ErrIpCapped):
			// ip_capped is admission-only (too many distinct users on the
			// egress IP), NOT a quota reset: cool the token via
			// cooldownIpCapped's bounded re-admission (#118) — full
			// retryAfterMs + jitter, capped per token per day (the 3rd hit
			// in a rolling window locks until Pacific midnight) — and never
			// invalidate the session (existing sessions keep running).
			var ice *upstream.IpCappedError
			if errors.As(err, &ice) {
				cooldownIpCapped(lease, ice)
			}
			release()
			return nil, nil, err
		case errors.Is(err, upstream.ErrCountryBlocked):
			// A chat-path country block cools the token like the admission
			// path does: without it the cached session stays "active" and
			// every request re-hits upstream run-start inside the window.
			var cbe *upstream.CountryBlockedError
			if errors.As(err, &cbe) {
				cooldownCountry(lease, cbe)
			}
			release()
			return nil, nil, err
		case errors.Is(err, upstream.ErrCredits):
			// #117: 402 is NEVER retried — the CLI throws immediately and
			// 402 is NOT in RETRYABLE_STATUS_CODES (reference/freebuff sdk
			// error-utils.ts line 16; run-agent-step.ts throws on 402). A
			// blind retry would burn a fresh lease against the same quota
			// wall (2 upstream chat POSTs). Surface for writeError, which
			// maps it to 402 out_of_credits.
			release()
			return nil, nil, err
		default:
			release()
			// Retryable UpstreamErrors (e.g. deployment_outside_hours) are
			// temporarily unavailable, not transient: a blind retry burns a
			// fresh lease against the same wall. Surface them for writeError
			// (503 upstream_retryable) instead.
			var ue *upstream.UpstreamError
			if errors.As(err, &ue) && ue.Retryable {
				return nil, nil, err
			}
			// T8: a retry cannot succeed on a canceled context (the log
			// watch showed `transient chat error, retrying once
			// err="context canceled"`) — surface the original error instead
			// of re-acquiring into a dead ctx.
			if attempts > 1 || ctx.Err() != nil {
				return nil, nil, err
			}
			transientErr = err
		}
		lease, err = acquire(ctx, effectiveModel)
		if err != nil {
			return nil, nil, err
		}
		released = false
		st.retried = true
		// The effective backoff before the retry: the re-acquire wait after
		// the failed attempt (a waiting-room/session gate can stall it).
		st.backoffMs = time.Since(failTime).Milliseconds()
		if transientErr != nil {
			// T13: logged here (not at the failure) so backoff_ms reflects
			// the real re-acquire wait before the retry attempt.
			s.logger.Debug("transient chat error, retrying once",
				"err", transientErr,
				"reason", chatErrClass(transientErr),
				"backoff_ms", st.backoffMs,
				"attempt", attempts,
				"req_id", st.reqID)
			transientErr = nil
		}
		// A fresh lease may bind a different model (fallback path): refresh
		// the effective model + body so opts.Model, the body and the
		// lease's session/run stay consistent.
		effectiveModel = lease.Model
		if effectiveModel == "" {
			effectiveModel = model
		}
		if effectiveModel != model {
			if renormalized, nerr := convert.NormalizeRequest(normalized, effectiveModel); nerr == nil {
				normalized = renormalized
			}
		}
		opts.Model = effectiveModel
		if lease.Run.RunID != opts.RunID {
			// The retry landed on a FRESH run (run-invalid path): the new
			// run's step counter starts at 1 — stamp its number so
			// llm_step_number stays per-run like the CLI.
			opts.StepNumber = int(lease.Run.NextStepNumber())
		}
		opts.RunID = lease.Run.RunID
		opts.SessionInstanceID = lease.SessionInstanceID
	}
}

// tokenLabel renders the lease's token for logging: "bridge" for bridge
// leases, the 1-based fixed-token index otherwise.
func tokenLabel(lease *pool.Lease) string {
	if lease == nil || lease.Bridge != nil {
		return "bridge"
	}
	return fmt.Sprintf("%d", lease.Token+1)
}

// relayStats accumulates per-response relay counters for logging.
type relayStats struct {
	chunks int
	bytes  int
	// usageTokens is the upstream usage total of the completed chat (the
	// final usage block), fed to the pool spend ledger once per successful
	// completion (#122). 0 when the stream carried no usage.
	usageTokens int64
}

// usageTotalTokens extracts the token total from a chat usage object
// (total_tokens, falling back to prompt+completion). Returns 0 when absent.
// Feeds the per-token spend ledger (#122).
func usageTotalTokens(usage any) int64 {
	u, ok := usage.(map[string]any)
	if !ok || u == nil {
		return 0
	}
	if total, ok := intOf(u["total_tokens"]); ok && total > 0 {
		return total
	}
	prompt, _ := intOf(u["prompt_tokens"])
	completion, _ := intOf(u["completion_tokens"])
	return prompt + completion
}

// keepaliveInterval is how long the relay may sit without relaying a data
// chunk before it emits an SSE comment frame to hold the connection open.
// Long upstream reasoning pauses produce no chunks, and proxies/clients may
// treat silence as a dead connection. A var (not const) so tests can shrink
// it.
var keepaliveInterval = 15 * time.Second

// lineChunk is one upstream SSE line or the terminal send. done is set only
// on the clean-EOF send (a real empty line also arrives as line==nil, so the
// terminal state must be explicit, not inferred from a nil slice).
type lineChunk struct {
	line []byte
	err  error
	done bool
}

// relayReadLoop drains r line by line onto ch, stopping when the stream
// ends or ctx is canceled. The final send carries done (clean EOF) or the
// terminal read error; on cancellation the goroutine exits without sending
// (the request context cancellation closes the upstream body read, so Scan
// returns promptly).
func relayReadLoop(ctx context.Context, r io.Reader, ch chan<- lineChunk) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		select {
		case ch <- lineChunk{line: line}:
		case <-ctx.Done():
			return
		}
	}
	var terminal lineChunk
	if err := scanner.Err(); err != nil {
		terminal = lineChunk{err: err}
	} else {
		terminal = lineChunk{done: true}
	}
	select {
	case ch <- terminal:
	case <-ctx.Done():
	}
}

// relayStream forwards sanitized upstream SSE lines to the client with
// per-chunk flushing, a ": connecting" grace-flush comment, a keepalive
// comment every keepaliveInterval of relay silence, a [DONE] terminator,
// and an error chunk (then DONE) when the upstream stream dies while the
// client context is still live.
func (s *Server) relayStream(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.logger.Warn("response writer does not support flushing")
		return
	}
	w.WriteHeader(http.StatusOK)

	// The official CLI client treats a ": connecting" comment as the signal
	// that headers have flushed and the stream is live (grace flush): write
	// it before relaying anything so a client-side timeout can never fire
	// during a long upstream admission pause. Comment frames are ignored by
	// SSE parsers.
	_, _ = io.WriteString(w, ": connecting\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()

	lines := make(chan lineChunk)
	go relayReadLoop(ctx, r, lines)

	relayed := time.Now()
	first := true
	var reasoningParts []string
	var contentParts []string
	var streamModel string
	toolIDsMap := make(map[string]bool)
	var toolIDs []string
	endTurnCallIndexes := make(map[int]bool)
	seenRealToolCalls := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if time.Since(relayed) >= keepaliveInterval {
				_, _ = io.WriteString(w, ": keepalive\n\n")
				relayed = time.Now()
				flusher.Flush()
			}
		case lc := <-lines:
			if lc.err != nil {
				if ctx.Err() == nil {
					s.logger.Warn("upstream stream error", "err", lc.err)
					_, _ = w.Write(convert.ErrorChunk("upstream stream interrupted: "+lc.err.Error(), "upstream_stream_error"))
					_, _ = w.Write(convert.DONE)
					flusher.Flush()
				}
				s.ingestStreamReasoning(streamModel, reasoningParts, contentParts, toolIDs)
				return
			}
			if lc.done {
				// Clean end of stream (EOF is not a scanner error).
				_, _ = w.Write(convert.DONE)
				flusher.Flush()
				s.ingestStreamReasoning(streamModel, reasoningParts, contentParts, toolIDs)
				return
			}
			clean, drop := convert.SanitizeChunk(lc.line)
			if drop {
				// Non-chunk lines (upstream comments) still prove liveness.
				relayed = time.Now()
				continue
			}
			// Strip Codebuff end_turn pseudo-tool-calls (issue #140).
			// The proxy injects end_turn into every upstream request to pass
			// foreign_toolset validation; the model calls it when done. We
			// must not relay it to clients that never declared it.
			if bytes.Contains(clean, []byte(`"end_turn"`)) {
				var chunk map[string]any
				if json.Unmarshal(clean, &chunk) == nil {
					convert.StripEndTurnToolCalls(chunk)
					// Drop continuation fragments for already-stripped end_turn indexes.
					if len(endTurnCallIndexes) > 0 {
						if choices, ok := chunk["choices"].([]any); ok {
							for _, raw := range choices {
								choice, _ := raw.(map[string]any)
								if choice == nil {
									continue
								}
								delta, _ := choice["delta"].(map[string]any)
								if delta == nil {
									continue
								}
								tcs, _ := delta["tool_calls"].([]any)
								if len(tcs) == 0 {
									continue
								}
								filtered := make([]any, 0, len(tcs))
								for _, tc := range tcs {
									tcMap, _ := tc.(map[string]any)
									if tcMap == nil {
										filtered = append(filtered, tc)
										continue
									}
									idx := 0
									if i, ok := tcMap["index"].(float64); ok {
										idx = int(i)
									}
									if endTurnCallIndexes[idx] {
										continue // drop continuation fragment
									}
									filtered = append(filtered, tc)
								}
								if len(filtered) == 0 {
									delete(delta, "tool_calls")
								} else {
									delta["tool_calls"] = filtered
								}
							}
						}
					}
					// Track newly discovered end_turn AND real tool call indexes.
					if choices, ok := chunk["choices"].([]any); ok {
						for _, raw := range choices {
							choice, _ := raw.(map[string]any)
							if choice == nil {
								continue
							}
							delta, _ := choice["delta"].(map[string]any)
							if delta == nil {
								continue
							}
							tcs, _ := delta["tool_calls"].([]any)
							for _, tc := range tcs {
								tcMap, _ := tc.(map[string]any)
								if tcMap == nil {
									continue
								}
								fn, _ := tcMap["function"].(map[string]any)
								name, _ := fn["name"].(string)
								if name == "end_turn" {
									if i, ok := tcMap["index"].(float64); ok {
										endTurnCallIndexes[int(i)] = true
									}
								} else if name != "" {
									seenRealToolCalls = true
								}
							}
						}
					}
					reEncoded, err := json.Marshal(chunk)
					if err == nil {
						clean = reEncoded
					}
				}
			}
			// Rewrite finish_reason for the terminal chunk when ALL tool calls
			// in this stream were end_turn. The terminal chunk carries no
			// "end_turn" string (only finish_reason: "tool_calls"), so the
			// block above is skipped. Without this, finish_reason: "tool_calls"
			// leaks to clients that never declared end_turn.
			if !seenRealToolCalls && len(endTurnCallIndexes) > 0 {
				if bytes.Contains(clean, []byte(`"finish_reason":"tool_calls"`)) {
					var chunk map[string]any
					if json.Unmarshal(clean, &chunk) == nil {
						if rawChoices, ok := chunk["choices"].([]any); ok {
							for _, raw := range rawChoices {
								if choice, ok := raw.(map[string]any); ok {
									if fr, ok := choice["finish_reason"].(string); ok && fr == "tool_calls" {
										choice["finish_reason"] = "stop"
									}
								}
							}
						}
						if b, err := json.Marshal(chunk); err == nil {
							clean = b
						}
					}
				}
			}
			// The final chunk carries the usage block (or a usage-only
			// chunk when stream_options.include_usage is set); capture its
			// total for the spend ledger (#122). Cheap substring probe, so
			// the per-chunk path only pays for an unmarshal on the usage
			// chunk itself.
			if bytes.Contains(clean, []byte(`"usage"`)) {
				var u struct {
					Usage any `json:"usage"`
				}
				// Only adopt the total when the chunk actually carries a
				// usage block: a trailing "usage":null or a content chunk
				// merely mentioning the word must not zero the ledger.
				if json.Unmarshal(clean, &u) == nil && u.Usage != nil {
					stats.usageTokens = usageTotalTokens(u.Usage)
				}
			}
			if bytes.Contains(clean, []byte(`"choices"`)) {
				var chunk struct {
					Model   string `json:"model"`
					Choices []struct {
						Delta struct {
							Content          *string `json:"content"`
							ReasoningContent *string `json:"reasoning_content"`
							Reasoning        *string `json:"reasoning"`
							Thinking         *string `json:"thinking"`
							ToolCalls        []struct {
								ID string `json:"id"`
							} `json:"tool_calls"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal(clean, &chunk) == nil {
					if chunk.Model != "" {
						streamModel = chunk.Model
					}
					if len(chunk.Choices) > 0 {
						delta := chunk.Choices[0].Delta
						if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
							reasoningParts = append(reasoningParts, *delta.ReasoningContent)
						} else if delta.Reasoning != nil && *delta.Reasoning != "" {
							reasoningParts = append(reasoningParts, *delta.Reasoning)
						} else if delta.Thinking != nil && *delta.Thinking != "" {
							reasoningParts = append(reasoningParts, *delta.Thinking)
						}
						if delta.Content != nil && *delta.Content != "" {
							contentParts = append(contentParts, *delta.Content)
						}
						for _, tc := range delta.ToolCalls {
							if tc.ID != "" && !toolIDsMap[tc.ID] {
								toolIDsMap[tc.ID] = true
								toolIDs = append(toolIDs, tc.ID)
							}
						}
					}
				}
			}
			if first {
				first = false
				phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
			}
			frame := convert.EncodeSSE(clean)
			if _, err := w.Write(frame); err != nil {
				s.logger.Debug("stream write failed", "err", err)
				return
			}
			stats.chunks++
			stats.bytes += len(frame)
			relayed = time.Now()
			flusher.Flush()
		}
	}
}

func (s *Server) ingestStreamReasoning(model string, reasoningParts, contentParts, toolIDs []string) {
	if s.reasoningCache == nil || len(reasoningParts) == 0 {
		return
	}
	rc := strings.Join(reasoningParts, "")
	if rc == "" {
		return
	}
	cStr := strings.Join(contentParts, "")
	s.reasoningCache.Put(toolIDs, cStr, "", rc, "", model)
}

// relayJSON drains the upstream SSE stream through the accumulator and
// writes one chat.completion JSON response. On any decode or stream error
// nothing is written and a 502 is returned (the client asked for a single
// response; a partial one would be worse than none).
func (s *Server) relayJSON(ctx context.Context, w http.ResponseWriter, r io.Reader, stats *relayStats, chatStart time.Time) {
	acc := convert.NewAccumulator()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxStreamLine)
	first := true
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		if first {
			first = false
			phasetiming.FromContext(ctx).Since(phasetiming.UpstreamTTFBMS, chatStart)
		}
		if err := acc.Add(scanner.Bytes()); err != nil {
			s.writeJSONError(w, http.StatusBadGateway,
				"failed to decode upstream stream: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
			return
		}
		stats.chunks++
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() == nil {
			s.writeJSONError(w, http.StatusBadGateway,
				"upstream stream error: "+err.Error(), "upstream_error", "upstream_unavailable", 0)
		}
		return
	}
	out := acc.Finish()
	// Strip Codebuff end_turn pseudo-tool-calls from non-streaming response.
	if bytes.Contains(out, []byte(`"end_turn"`)) {
		var comp map[string]any
		if json.Unmarshal(out, &comp) == nil {
			convert.StripEndTurnToolCalls(comp)
			if b, err := json.Marshal(comp); err == nil {
				out = b
			}
		}
	}
	stats.bytes = len(out)
	// Capture the accumulated usage total for the spend ledger (#122);
	// only adopt when the response actually carries a usage block.
	var usageObj struct {
		Usage any `json:"usage"`
	}
	if json.Unmarshal(out, &usageObj) == nil && usageObj.Usage != nil {
		stats.usageTokens = usageTotalTokens(usageObj.Usage)
	}

	if s.reasoningCache != nil {
		var comp map[string]any
		if json.Unmarshal(out, &comp) == nil {
			model, _ := comp["model"].(string)
			if choices, ok := comp["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if msg, ok := choice["message"].(map[string]any); ok {
						rc, _ := msg["reasoning_content"].(string)
						if rc == "" {
							rc, _ = msg["reasoning"].(string)
						}
						if rc != "" {
							var toolIDs []string
							var tcJSON string
							if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
								for _, raw := range tcs {
									if tc, ok := raw.(map[string]any); ok {
										if id, ok := tc["id"].(string); ok && id != "" {
											toolIDs = append(toolIDs, id)
										}
									}
								}
								if b, err := json.Marshal(tcs); err == nil {
									tcJSON = string(b)
								}
							}
							cStr, _ := msg["content"].(string)
							s.reasoningCache.Put(toolIDs, cStr, tcJSON, rc, "", model)
						}
					}
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
