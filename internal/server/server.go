// Package server exposes the OpenAI-compatible HTTP surface of the
// freebuff-proxy bridge: POST /v1/chat/completions (stream + non-stream),
// GET /v1/models, and GET /healthz. Stdlib only.
//
// Responsibilities (PRD §6 error matrix):
//   - optional client auth (Bearer / x-api-key exact match, constant-time)
//   - request sanitization via internal/convert before the upstream call
//   - retry-once recovery for session-invalid / run-invalid chat errors
//   - 30-min token cooldown on upstream auth rejection
//   - error mapping to the OpenAI error shape, 503 + Retry-After for the
//     waiting room, 502 when every token is exhausted
//   - SSE relay (sanitized chunks + [DONE]) and non-streaming accumulation
//   - client-disconnect propagation to the upstream (request context)
package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/convert"
	"freebuff-proxy/internal/dashboard"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/ratelimit"
	"freebuff-proxy/internal/reasoningcache"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/tokenestimate"
	"freebuff-proxy/internal/updatecheck"
	"freebuff-proxy/internal/upstream"
)

const (
	// maxRequestBody caps the inbound chat-completions body (32MB).
	maxRequestBody = 32 << 20
	// maxStreamLine caps one upstream SSE line the scanner will buffer.
	maxStreamLine = 16 << 20
)

// Server is the HTTP handler holder: routes are built by Handler(). cfg is an
// atomic pointer because /admin/reload swaps it while requests are in flight;
// every read site must Load() it once per request and use the local.
type Server struct {
	cfg     atomic.Pointer[config.Config]
	pool    *pool.Pool
	reg     *registry.Registry
	logger  *slog.Logger
	started time.Time

	// logs is the optional dashboard log viewer ring (nil = disabled); its
	// Counts feed freebuff_proxy_log_events_total on /metrics.
	logs *logring.Handler

	// dash is the embedded admin UI (htmx + vendored assets).
	dash *dashboard.Dashboard
	// adminAuth guards the dashboard: a stateless HMAC-signed session cookie
	// issued against ADMIN_TOKEN, plus a per-IP login rate limiter.
	adminAuth *adminAuth
	// adminSaveMu serializes .env saves (config editor) so a rejected save
	// cannot clobber a newer accepted one.
	adminSaveMu sync.Mutex
	// configPath is the -config JSON path ("" when none); reloads re-apply it
	// so JSON overrides survive dashboard saves and /admin/reload.
	configPath string

	// version is the running release tag (""/dev for dev builds); the
	// dashboard badge compares it against the latest GitHub release (#50b).
	// updates is the cached latest-release checker (nil = no badge).
	version string
	updates *updatecheck.Checker

	// authClient drives the headless OAuth login wizard (issue #62): a
	// token-less upstream client whose transport/stealth wiring matches the
	// pooled clients. nil disables the wizard endpoints with 503.
	authClient *upstream.Client
	// tokenEstimator counts tokens locally for /v1/messages/count_tokens
	// (nil only if the embedded codec failed to initialize at startup).
	tokenEstimator *tokenestimate.Estimator
	// loginFlows is the in-flight login-flow registry keyed by flow id
	// (fingerprint): start POSTs /api/auth/cli/code, status polls it until
	// the authToken lands (then AddToken + persist).
	loginMu    sync.Mutex
	loginFlows map[string]*loginFlow
	// reasoningCache caches reasoning content and signatures for tool calls across turns.
	reasoningCache *reasoningcache.Cache
	// rateLimiter caps client request rates per source IP (issue #137).
	rateLimiter *ratelimit.Limiter
	// rateLimitRejections tracks total client requests rejected by local rate limiter.
	rateLimitRejections atomic.Int64
}

// loginFlow is one in-flight headless login (issue #62).
type loginFlow struct {
	ID         string // short flow id shown to the client (fingerprint prefix)
	Code       *upstream.CLILoginCode
	Started    time.Time
	Done       bool
	Completing bool // one status poll is mid-completion (guards double-add)
	Token      string
	Error      string
	Index      int // pooled token index after AddToken (0 when bridge)
}

// loginFlowTTL drops stale flows (never completed; browser closed).
const loginFlowTTL = 10 * time.Minute

// WithVersion wires the running release tag + update checker for the
// dashboard badge (issue #50b). A nil checker disables the badge.
func WithVersion(version string, updates *updatecheck.Checker) Option {
	return func(s *Server) {
		s.version = version
		s.updates = updates
	}
}

// WithLoginClient wires the token-less upstream client that drives the
// headless OAuth login wizard (issue #62). A nil client disables the
// wizard endpoints with 503.
func WithLoginClient(c *upstream.Client) Option {
	return func(s *Server) {
		s.authClient = c
	}
}

// Option configures optional server features (release-version badge).
type Option func(*Server)

// New builds the server over the configured pool and registry. A nil logger
// falls back to slog.Default(). The started timestamp pins /v1/models
// "created" and /healthz uptime. logs is the optional dashboard log viewer
// ring (nil disables the /admin/logs page data). configPath is the -config
// JSON path the process was started with ("" = none), used by reloads so a
// dashboard save or /admin/reload re-applies the JSON overrides. opts
// configure optional features (release-version badge, login wizard client).
func New(cfg *config.Config, p *pool.Pool, reg *registry.Registry, logger *slog.Logger, logs *logring.Handler, configPath string, opts ...Option) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{pool: p, reg: reg, logger: logger, started: time.Now(), configPath: configPath, loginFlows: make(map[string]*loginFlow), logs: logs}
	s.cfg.Store(cfg)
	s.rateLimiter = ratelimit.New(cfg.RateLimitPerIP, cfg.RateLimitBurst, 10000)
	// The token estimator shares one o200k_base codec process-wide, so
	// count_tokens requests never rebuild the vocabulary.
	est, err := tokenestimate.New()
	if err != nil {
		logger.Warn("token estimator unavailable; /v1/messages/count_tokens will fail", "err", err)
	}
	s.tokenEstimator = est
	for _, opt := range opts {
		opt(s)
	}
	dashOpts := []dashboard.Option{}
	if s.version != "" {
		dashOpts = append(dashOpts, dashboard.WithVersion(s.version, s.updates))
	}
	s.dash = dashboard.New(func() *config.Config { return s.cfg.Load() }, p, reg, logger, logs, dashOpts...)
	s.adminAuth = newAdminAuth()
	s.reasoningCache = reasoningcache.New(10000, 2*time.Hour)
	convert.SetReasoningLookup(func(toolID string, content, toolCallsJSON string) (string, string, bool) {
		return s.reasoningCache.Get(toolID, content, toolCallsJSON)
	})
	return s
}

// Handler returns the route table wrapped in an access-log middleware. Method
// mismatches and unknown paths get the ServeMux's automatic 405/404.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.requireAuth(s.handleChat))
	mux.HandleFunc("POST /v1/responses", s.requireAuth(s.handleResponses))
	mux.HandleFunc("POST /v1/messages", s.requireAuth(s.handleMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", s.requireAuth(s.handleMessagesCountTokens))
	mux.HandleFunc("POST /v1/embeddings", s.requireAuth(s.handleEmbeddings))
	mux.HandleFunc("GET /v1/models", s.requireAuth(s.handleModels))
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /admin/reload", s.requireAdminToken(s.requireAuth(s.adminCSRF(http.HandlerFunc(s.handleReload)))))
	// Admin dashboard: cookie-authenticated browser UI. Assets are static
	// and public — the login page (served without a cookie) references them,
	// so they must NOT sit behind dashboardAuth. Overview/tokens/metrics are
	// read-only status and stay open when ADMIN_TOKEN is unset (legacy).
	// Config (read + write) and logs expose secrets and are gated further:
	// with ADMIN_TOKEN unset they require a loopback client.
	// GET /admin/login serves the SPA login page (client-side form, posts to
	// the JSON API below); with ADMIN_TOKEN unset it redirects straight to
	// the dashboard (handleAdminLogin's first branch). POST /admin/login is
	// the JSON token-check API.
	mux.HandleFunc("GET /admin/login", s.handleAdminLogin)
	// POST /admin/login consumes the per-IP login-attempt budget, so it must
	// carry the same CSRF gate as the other mutating admin routes: without it
	// a malicious page could fire cross-origin POSTs with wrong tokens and
	// lock the victim out of the dashboard (5 fails → 1-minute lockout,
	// repeatable).
	mux.HandleFunc("POST /admin/login", s.adminCSRF(http.HandlerFunc(s.handleAdminLogin)))
	// Admin dashboard API routes (JSON)
	mux.Handle("GET /admin/api/overview", s.dashboardAuth(s.dash.APIHandler("overview")))
	mux.Handle("GET /admin/api/tokens", s.dashboardAuth(s.dash.APIHandler("tokens")))
	mux.Handle("GET /admin/api/models", s.dashboardAuth(s.dash.APIHandler("models")))
	mux.Handle("GET /admin/api/traces", s.dashboardAuth(s.dash.APIHandler("traces")))
	mux.Handle("GET /admin/api/setup", s.dashboardAuth(s.dash.APIHandler("setup")))
	mux.Handle("GET /admin/api/config", s.dashboardAuth(s.adminSensitive(s.dash.APIHandler("config"))))
	mux.Handle("GET /admin/api/logs", s.dashboardAuth(s.adminSensitive(s.dash.APIHandler("logs"))))
	mux.Handle("GET /admin/api/metrics", s.dashboardAuth(s.dash.APIHandler("metrics")))
	mux.Handle("GET /admin/api/version", s.dashboardAuth(http.HandlerFunc(s.dash.APIVersion)))

	// SPA: all admin/* GET routes serve the embedded Svelte SPA
	mux.Handle("GET /admin", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
	mux.Handle("GET /admin/", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
	mux.Handle("GET /admin/tokens", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
	mux.Handle("GET /admin/models", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
	mux.Handle("GET /admin/traces", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
	mux.Handle("GET /admin/setup", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
	mux.Handle("GET /admin/playground", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
	mux.Handle("GET /admin/config", s.dashboardAuth(s.adminSensitive(http.HandlerFunc(s.dash.ServeSPA))))
	mux.Handle("GET /admin/logs", s.dashboardAuth(s.adminSensitive(http.HandlerFunc(s.dash.ServeSPA))))
	mux.Handle("GET /admin/metrics", s.dashboardAuth(http.HandlerFunc(s.dash.ServeSPA)))
	mux.Handle("POST /admin/playground/chat", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handlePlaygroundChat)))))
	mux.Handle("POST /admin/login/start", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleLoginStart)))))
	mux.Handle("GET /admin/login/status", s.dashboardAuth(s.adminSensitive(http.HandlerFunc(s.handleLoginStatus))))
	mux.Handle("POST /admin/config", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleConfigSave)))))
	mux.Handle("POST /admin/tokens/{id}/unlock", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenUnlock)))))
	mux.Handle("POST /admin/tokens/{id}/finish", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenFinish)))))
	mux.Handle("POST /admin/tokens/{id}/test", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenTest)))))
	mux.Handle("POST /admin/tokens/test-all", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenTestAll)))))
	mux.Handle("POST /admin/tokens/add", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenAdd)))))
	mux.Handle("POST /admin/tokens/remove", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleTokenRemove)))))
	mux.Handle("POST /admin/mode", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleModeSwitch)))))
	mux.Handle("POST /admin/diag", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleDiag)))))
	mux.Handle("POST /admin/smoke", s.dashboardAuth(s.adminSensitive(s.adminCSRF(http.HandlerFunc(s.handleSmoke)))))
	// Static assets: serve from embedded dist/assets
	mux.Handle("GET /admin/assets/", noDirListing(http.StripPrefix("/admin/assets/", http.FileServerFS(mustSubFS(dashboard.DistFS(), "assets")))))
	// CORS middleware wraps the whole route table: it answers OPTIONS
	// preflights on the /v1/* API surface with 204 and stamps the allow
	// headers on every /v1/* response. Admin routes are intentionally left
	// untouched (cookie-authenticated dashboard; SameSite=Strict already
	// blocks cross-site reads, and an allow-origin would add nothing there).
	cors := s.corsMiddleware(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// D1: mint the request's correlation id exactly once here, then
		// carry it in the request context so every downstream log line
		// (chat routing/done/trace, request failed, upstream do/retry)
		// shares it. Handlers reached without this wrapper (direct calls
		// in tests) mint a fallback id in chatCore.
		reqID := newReqID()
		r = r.WithContext(context.WithValue(r.Context(), reqIDKey{}, reqID))
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		cors.ServeHTTP(sw, r)
		attrs := []any{
			"req_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"ms", time.Since(start).Milliseconds(),
			"remote", remoteHost(r),
		}
		// The client's X-Request-Id is preserved as a separate
		// client_request_id field (never trusted as the correlation key).
		if crid := clientRequestID(r); crid != "" {
			attrs = append(attrs, "client_request_id", crid)
		}
		// T17: LOG_ACCESS=false disables access lines entirely. Quiet
		// endpoints (/healthz, /metrics, OPTIONS preflights) are
		// rate-limited to one access line per path per accessQuietWindow so
		// a poller or browser preflight does not flood the log; every other
		// path keeps one line per request. req_id/client_request_id survive
		// in both cases.
		if !s.cfg.Load().LogAccess {
			return
		}
		if quietAccessPath(r.Method, r.URL.Path) && !accessLogDue(r.URL.Path, start) {
			return
		}
		s.logger.Info("access", attrs...)
	})
}

// quietAccessPath reports whether path is a poll/fire-and-forget endpoint
// whose access lines are rate-limited (T17): /healthz, /metrics, and CORS
// OPTIONS preflights. Every other path logs one access line per request.
func quietAccessPath(method, path string) bool {
	return path == "/healthz" || path == "/metrics" || method == http.MethodOptions
}

// accessQuietWindow is the quiet-endpoint access gate window: at most one
// access line per path per window (T17). A var so tests can shrink it.
var accessQuietWindow = 60 * time.Second

// accessLogGate is the per-process quiet-path access gate: map[path]lastLog
// plus a mutex (T17). The path set is bounded by the route table, so no
// cleanup is needed.
var accessLogGate = struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time
}{lastSeen: make(map[string]time.Time)}

// accessLogDue reports whether an access line may fire for path now,
// recording the current attempt. The first request for a path and any
// request at least accessQuietWindow after the last line fire; requests
// inside the window are suppressed.
func accessLogDue(path string, now time.Time) bool {
	accessLogGate.mu.Lock()
	defer accessLogGate.mu.Unlock()
	last, ok := accessLogGate.lastSeen[path]
	if !ok || now.Sub(last) >= accessQuietWindow {
		accessLogGate.lastSeen[path] = now
		return true
	}
	return false
}

// resetAccessLogGate clears the quiet-path access gate (test hook).
func resetAccessLogGate() {
	accessLogGate.mu.Lock()
	defer accessLogGate.mu.Unlock()
	clear(accessLogGate.lastSeen)
}

// corsOrigin returns the configured Access-Control-Allow-Origin, treating an
// empty value as the "*" default (an empty .env line must not disable CORS).
func (s *Server) corsOrigin() string {
	origin := strings.TrimSpace(s.cfg.Load().CORSAllowedOrigin)
	if origin == "" {
		return "*"
	}
	return origin
}

// corsMiddleware answers CORS preflights on the /v1/* API surface and stamps
// the allow headers on /v1/* responses. An OPTIONS request for any /v1/*
// path is answered with 204 before the route table sees it (so unknown
// /v1/* subpaths still get a clean preflight, matching the reference
// proxy-freebuff OPTIONS → 204). Admin routes pass through untouched.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			h := w.Header()
			origin := s.corsOrigin()
			h.Set("Access-Control-Allow-Origin", origin)
			// When the origin is pinned (not "*"), vary on Origin so caches
			// never serve the pinned header to a different requester.
			if origin != "*" {
				h.Add("Vary", "Origin")
			}
			h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version")
			h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// statusWriter captures the response status for access logging. It forwards
// Flusher/Hijacker/Pusher so streaming and similar protocols keep working
// through the access-log wrapper.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

func (w *statusWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

// mustSubFS returns the named subtree of an embed.FS. The directory is
// embedded at compile time, so a missing subtree is an invariant violation,
// not a runtime condition.
func mustSubFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("dashboard: embedded subtree missing: " + err.Error())
	}
	return sub
}

// noDirListing rejects directory requests so FileServerFS never renders an
// index listing of the embedded assets.
func noDirListing(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// remoteHost returns the client host without the port.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- auth ---

// requireAuth wraps a handler with client-auth enforcement. When no API keys
// are configured the handler passes through untouched; /healthz is always
// exempt (the caller wires it without requireAuth). Bridge mode (no
// AUTH_TOKENS) also passes through: the Authorization header IS the upstream
// token there, and API_KEYS is meaningless. Hybrid mode passes a Bearer
// token through (bridge relay: the client's own FreeBuff credential), but
// token-less requests fall back to the pool and must still pass the
// API_KEYS gate — an x-api-key is the API_KEYS scheme, never a bridge token.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if len(cfg.APIKeys) == 0 || cfg.BridgeMode() {
			next(w, r)
			return
		}
		if cfg.HybridMode && bearerToken(r) != "" {
			next(w, r)
			return
		}
		if !s.authorized(r) {
			s.writeJSONError(w, http.StatusUnauthorized,
				"Invalid API key", "invalid_request_error", "invalid_api_key", 0)
			return
		}
		next(w, r)
	}
}

// extractBearerToken extracts the token from an Authorization header if it has
// a case-insensitive "Bearer " prefix (per RFC 7235 / RFC 6750). Returns the
// trimmed token and true if the prefix matches, or ("", false) otherwise.
func extractBearerToken(authHeader string) (string, bool) {
	authHeader = strings.TrimSpace(authHeader)
	if len(authHeader) >= 7 && strings.EqualFold(authHeader[:7], "bearer ") {
		return strings.TrimSpace(authHeader[7:]), true
	}
	return "", false
}

// authorized reports whether the request carries a configured API key,
// either as "Authorization: Bearer <key>" or "x-api-key: <key>". Comparison
// is constant-time against every configured key.
func (s *Server) authorized(r *http.Request) bool {
	cfg := s.cfg.Load()
	provided := ""
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		provided = tok
	} else if h := r.Header.Get("x-api-key"); h != "" {
		provided = h
	}
	if provided == "" {
		return false
	}
	for _, key := range cfg.APIKeys {
		if subtle.ConstantTimeCompare([]byte(provided), []byte(key)) == 1 {
			return true
		}
	}
	return false
}

// requireAdminToken guards POST /admin/reload when ADMIN_TOKEN is set: the
// request must present it as "Authorization: Bearer <token>" (constant-time
// compare). When ADMIN_TOKEN is unset the handler passes through untouched —
// the legacy API_KEYS gate still applies via requireAuth, and main.go logs a
// startup warning for the open (default) case.
func (s *Server) requireAdminToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if cfg.AdminToken == "" {
			next(w, r)
			return
		}
		provided := ""
		if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
			provided = tok
		}
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AdminToken)) != 1 {
			s.writeJSONError(w, http.StatusUnauthorized,
				"Invalid admin token", "invalid_request_error", "invalid_admin_token", 0)
			return
		}
		next(w, r)
	}
}

// --- dashboard auth ---

// adminCookieName is the HttpOnly session cookie set after a successful
// ADMIN_TOKEN login. The value is stateless: unix expiry + HMAC-SHA256 over
// the expiry, keyed by a per-process random secret. No server-side session
// store; restart invalidates all sessions, which is the safe default for an
// admin UI.
const (
	adminCookieName = "fb_admin"
	adminCookieTTL  = 24 * time.Hour
)

// adminAuth issues and validates dashboard session cookies and rate-limits
// login attempts per remote IP.
type adminAuth struct {
	key   [32]byte
	mu    sync.Mutex
	fails map[string]failEntry
}

// failEntry tracks consecutive failed logins from one IP.
type failEntry struct {
	count int
	until time.Time
}

func newAdminAuth() *adminAuth {
	a := &adminAuth{fails: make(map[string]failEntry)}
	_, _ = rand.Read(a.key[:])
	return a
}

// cookieValue builds "expiry.hmac" for the given expiry.
func (a *adminAuth) cookieValue(expiry time.Time) string {
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte(strconv.FormatInt(expiry.Unix(), 10)))
	return strconv.FormatInt(expiry.Unix(), 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

// valid reports whether the cookie value carries a not-yet-expired HMAC
// signature. Constant-time comparison via hmac.Equal.
func (a *adminAuth) valid(value string) bool {
	dot := strings.IndexByte(value, '.')
	if dot < 0 {
		return false
	}
	exp, err := strconv.ParseInt(value[:dot], 10, 64)
	if err != nil || time.Now().Unix() >= exp {
		return false
	}
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte(value[:dot]))
	got, err := hex.DecodeString(value[dot+1:])
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return false
	}
	return true
}

// maxLoginFails caps consecutive failed logins from one IP before lockout;
// loginFailsCap bounds the fails map so distinct IP scans cannot grow it
// without bound (expired entries are dropped on access).
const (
	maxLoginFails = 5
	loginLockout  = time.Minute
	loginFailsCap = 1024
)

func (a *adminAuth) setCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    a.cookieValue(time.Now().Add(adminCookieTTL)),
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   int(adminCookieTTL.Seconds()),
	})
}

// allow reports whether ip may attempt a login right now. Entries track the
// running failure count until a lockout is set (until non-zero); an expired
// lockout is dropped so the map does not grow without bound.
func (a *adminAuth) allow(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.fails[ip]
	if !ok {
		return true
	}
	if !e.until.IsZero() {
		if time.Now().Before(e.until) {
			return false
		}
		delete(a.fails, ip)
	}
	return true
}

// recordFail counts a failed login, locking ip out after maxLoginFails. The
// map is capped: when a new IP arrives at the cap, expired entries are swept
// first, then the oldest remaining lockout is dropped (a brute-force scan
// rotating fresh IPs cannot grow the map without bound).
func (a *adminAuth) recordFail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.fails[ip]
	e.count++
	if e.count >= maxLoginFails {
		e.until = time.Now().Add(loginLockout)
		e.count = 0
	}
	if _, exists := a.fails[ip]; !exists && len(a.fails) >= loginFailsCap {
		now := time.Now()
		for k, v := range a.fails {
			if now.After(v.until) {
				delete(a.fails, k)
			}
		}
		if len(a.fails) >= loginFailsCap {
			// No expired entries to reclaim — drop one lockout (map
			// iteration order is fine; the bound is what matters).
			for k := range a.fails {
				delete(a.fails, k)
				break
			}
		}
	}
	a.fails[ip] = e
}

func (a *adminAuth) clearFails(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, ip)
}

// loginFailState snapshots the failure entry for ip: the current attempt
// count and whether ip is locked out (T15 audit trail).
func (a *adminAuth) loginFailState(ip string) (attempts int, locked bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.fails[ip]
	if !ok {
		return 0, false
	}
	return e.count, !e.until.IsZero() && time.Now().Before(e.until)
}

// dashboardAuth guards the browser UI. With ADMIN_TOKEN unset the dashboard
// is open (legacy behavior, matching /admin/reload; main.go warns at startup).
// Otherwise the request must carry a valid fb_admin cookie; missing/invalid
// sessions are redirected to the login page. htmx polls get 401 + HX-Redirect
// so the login page replaces the swapped region instead of a bare fragment.
func (s *Server) dashboardAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.cfg.Load()
		if cfg.AdminToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(adminCookieName); err == nil && s.adminAuth.valid(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/admin/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusFound)
	})
}

// adminSensitive gates the secret-bearing admin routes (config read/write,
// logs) in the default-open mode: when ADMIN_TOKEN is unset, only loopback
// clients may access them, so a remotely reachable proxy cannot leak or let
// anyone rewrite the .env. With ADMIN_TOKEN set the cookie gate already ran
// (this middleware is wrapped inside dashboardAuth). The Host header must
// also be loopback-named: a DNS-rebinding page (attacker.com → 127.0.0.1)
// arrives from a loopback RemoteAddr while its Host stays attacker-owned,
// which would otherwise defeat the gate (SEC-2).
// adminSensitive gates secret-bearing admin routes. When ADMIN_TOKEN is set,
// dashboardAuth validates the session cookie. When ADMIN_TOKEN is unset
// (optional auth), all admin routes are open to facilitate easy monitoring.
func (s *Server) adminSensitive(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

// adminCSRF rejects cross-origin mutating admin requests. Browsers send
// Origin (and/or Sec-Fetch-Site) on every POST; a malicious site's form
// would carry an Origin that does not match the proxy's own host. Requests
// with NEITHER header (curl, API clients, legacy tests) pass through, so the
// admin API stays scriptable while a victim's browser cannot drive the
// dashboard cross-site. Origin is compared case-insensitively per RFC 6454
// host matching; Sec-Fetch-Site must be same-origin or none (direct
// navigation). Wired inside dashboardAuth → adminSensitive so the cookie
// and loopback gates still run first.
func (s *Server) adminCSRF(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !strings.EqualFold(u.Host, r.Host) {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusForbidden)
					s.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
					return
				}
			}
			if sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs != "" {
				if sfs != "same-origin" && sfs != "none" {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.WriteHeader(http.StatusForbidden)
					s.dash.RenderConfigResult(w, r, false, "Cross-origin request rejected.")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	}
}

// handleAdminLogin serves the SPA login page on GET and processes the token
// form on POST: constant-time ADMIN_TOKEN comparison, per-IP rate limiting,
// and a signed session cookie on success. With ADMIN_TOKEN unset GET
// redirects straight to the dashboard.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	if r.Method != http.MethodPost {
		// GET/HEAD: render the SPA login page. The Svelte form posts to this
		// same route; with ADMIN_TOKEN unset there is nothing to log in to.
		if cfg.AdminToken == "" {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		s.dash.ServeSPA(w, r)
		return
	}
	if cfg.AdminToken == "" {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	ip := remoteHost(r)
	if r.Method == http.MethodPost {
		if !s.adminAuth.allow(ip) {
			// T15: audit the lockout rejection — attempts is the lockout
			// bound that was crossed; the submitted credential is never
			// logged.
			s.logger.Warn("admin login failed", "remote", ip, "attempts", maxLoginFails, "reason", "locked_out")
			s.dash.RenderLogin(w, r, "Too many failed attempts — try again in a minute.")
			return
		}
		token := r.FormValue("token")
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AdminToken)) == 1 {
			s.adminAuth.clearFails(ip)
			// Secure only when the login arrived over an actual TLS
			// connection (direct HTTPS or a TLS-terminating reverse proxy
			// setting X-Forwarded-Proto). A Secure cookie over plain HTTP is
			// rejected by browsers, silently breaking remote login.
			s.adminAuth.setCookie(w, r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		s.adminAuth.recordFail(ip)
		attempts, locked := s.adminAuth.loginFailState(ip)
		if locked {
			attempts = maxLoginFails
		}
		// T15: audit a failed login — remote, running attempt count, and
		// reason only; the credential itself is never logged.
		s.logger.Warn("admin login failed", "remote", ip, "attempts", attempts, "reason", "invalid_token")
		s.dash.RenderLogin(w, r, "Invalid admin token.")
		return
	}
	s.dash.RenderLogin(w, r, "")
}

// maxEnvSize caps the .env editor payload (64KB is generous for a config file).
const maxEnvSize = 64 << 10

// tokenActionID parses the {id} path value into a 0-based token index.
func tokenActionID(r *http.Request) (int, error) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 {
		return 0, errors.New("invalid token id")
	}
	return id, nil
}

// handleTokenUnlock clears a token's cooldown/rate-limit/ban lock. Gated as
// sensitive: unlocking a banned token lets upstream traffic resume, so it is
// loopback-only in open mode.
func (s *Server) handleTokenUnlock(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = s.pool.UnlockToken(id)
	}
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Unlock failed: "+err.Error())
		return
	}
	s.logger.Info("dashboard token unlocked", "token", id)
	s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" unlocked — no cooldown or ban window remains.")
}

// handleTokenFinish finishes all active runs of a token.
func (s *Server) handleTokenFinish(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	if err == nil {
		err = s.pool.FinishTokenRuns(r.Context(), id)
	}
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Finish failed: "+err.Error())
		return
	}
	s.logger.Info("dashboard token runs finished", "token", id)
	s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" runs finished.")
}

// handleTokenTest probes a token with a zero-cost upstream GET probe (no
// session claim, no model needed) and renders the result plus the live
// per-model quota when the upstream response carries it.
func (s *Server) handleTokenTest(w http.ResponseWriter, r *http.Request) {
	id, err := tokenActionID(r)
	var state *upstream.SessionState
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		state, err = s.pool.ProbeToken(ctx, id)
	}
	if err != nil {
		if errors.Is(err, upstream.ErrNoActiveSession) {
			s.logger.Info("dashboard token probe ok (no active session)", "token", id)
			s.dash.RenderConfigResult(w, r, true, "Token "+strconv.Itoa(id)+" OK — zero-cost probe succeeded (no active session).")
			return
		}
		s.logger.Warn("dashboard token probe failed", "token", id, "err", err)
		s.dash.RenderConfigResult(w, r, false, "Token "+strconv.Itoa(id)+" test failed: "+err.Error())
		return
	}
	msg := "Token " + strconv.Itoa(id) + " OK — zero-cost probe succeeded"
	if q := quotaSummary(state); q != "" {
		msg += " (" + q + ")"
	}
	msg += "."
	// The probe is the pooled equivalent of a session admission: fold the
	// observed accessTier + provisioned model set (rateLimitsByModel keys)
	// into the runtime config for ResolveModel's -max upgrade gate
	// (PREFER_MAX_MODELS gating).
	s.rememberAccessTier(state.AccessTier, provisionedSet(state))
	s.logger.Info("dashboard token probe ok", "token", id)
	s.dash.RenderConfigResult(w, r, true, msg)
}

// handleTokenTestAll probes every pooled token (dashboard "Test all"). Each
// probe is a zero-cost upstream GET (no session claim, no model needed);
// per-token results are rendered as a fragment.
func (s *Server) handleTokenTestAll(w http.ResponseWriter, r *http.Request) {
	count := 0
	for _, snap := range s.pool.PoolSnapshot().Tokens {
		i := snap.Token
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		state, err := s.pool.ProbeToken(ctx, i)
		cancel()
		ok := err == nil || errors.Is(err, upstream.ErrNoActiveSession)
		msg := "ok"
		switch {
		case errors.Is(err, upstream.ErrNoActiveSession):
			msg = "ok (no active session)"
		case err != nil:
			msg = err.Error()
		default:
			if q := quotaSummary(state); q != "" {
				msg = "ok (" + q + ")"
			}
		}
		s.dash.RenderTestResult(w, r, i, ok, msg, "")
		count++
	}
	if count == 0 {
		s.dash.RenderConfigResult(w, r, false, "No tokens to test (bridge mode has no fixed AUTH_TOKENS).")
	}
}

// handleEmbeddings answers POST /v1/embeddings with a structured
// unsupported-endpoint error: the proxy serves chat completions only, and
// the error body points clients at /v1/chat/completions and the live model
// list so a picker/fallback client can self-correct. 400 with the
// documented "unsupported_endpoint" code (distinct from the mux's bare 404,
// which gives an embeddings client no actionable signal).
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.logger.Warn("unsupported endpoint requested", "path", r.URL.Path, "remote", remoteHost(r), "status", http.StatusBadRequest)
	s.writeJSONError(w, http.StatusBadRequest,
		"this proxy serves chat completions only; embeddings are not supported. Use POST /v1/chat/completions with one of: "+strings.Join(s.reg.Models(), ", "),
		"unsupported_endpoint", "unsupported_endpoint", 0)
}

// smokeRequest is the dashboard smoke-test payload (a real chat through the
// exact client path clients use).
type smokeRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Token  string `json:"token"` // bridge mode: client token to relay upstream
}

// maxSmokeBytes bounds the upstream body read for the smoke preview.
const maxSmokeBytes = 32 << 10

// handleSmoke sends one real chat request through the pool (Acquire + Chat,
// the same path clients use) and reports status, latency, and a content
// preview. Bridge mode requires a client token in the payload.
func (s *Server) handleSmoke(w http.ResponseWriter, r *http.Request) {
	var req smokeRequest
	// The dashboard form posts urlencoded model=&prompt=&token=; read those
	// first and only fall back to JSON for programmatic clients (mirrors
	// handleTokenAdd).
	var err error
	req.Model = strings.TrimSpace(r.FormValue("model"))
	req.Prompt = strings.TrimSpace(r.FormValue("prompt"))
	req.Token = strings.TrimSpace(r.FormValue("token"))
	if req.Model == "" && req.Prompt == "" && req.Token == "" {
		var body []byte
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err = json.Unmarshal(body, &req); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Invalid request JSON: "+err.Error())
			return
		}
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	req.Token = strings.TrimSpace(req.Token)
	if req.Model == "" {
		req.Model = probeModel(s.reg)
		if req.Model == "" {
			s.dash.RenderConfigResult(w, r, false, "No models in the registry to test.")
			return
		}
	}
	if req.Prompt == "" {
		req.Prompt = "ping"
	}
	if len(req.Prompt) > 200 {
		s.dash.RenderConfigResult(w, r, false, "Prompt too long (max 200 chars).")
		return
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	ctx, phases := phasetiming.WithContext(ctx)

	cfg := s.cfg.Load()
	chatBody := []byte(`{"model":` + strconv.Quote(req.Model) + `,"messages":[{"role":"user","content":` + strconv.Quote(req.Prompt) + `}],"stream":false}`)
	chatOpts := upstream.ChatOptions{Model: req.Model}

	var lease *pool.Lease
	var up io.ReadCloser
	acquireStart := time.Now()
	if cfg.BridgeMode() {
		if req.Token == "" {
			s.dash.RenderConfigResult(w, r, false, "Bridge mode: include a client token in the smoke request.")
			return
		}
		lease, err = s.pool.AcquireBridge(ctx, req.Token, req.Model)
	} else {
		lease, err = s.pool.Acquire(ctx, req.Model)
	}
	phases.Since(phasetiming.AcquireMS, acquireStart)
	if err == nil {
		up, err = s.pool.Chat(ctx, lease, chatOpts, chatBody)
	}
	if err != nil {
		if lease != nil {
			s.pool.LeaseRelease(lease)
		}
		phases.Since(phasetiming.TotalMS, start)
		s.logger.Warn("dashboard smoke test failed", "model", req.Model, "err", err)
		s.dash.RenderConfigResult(w, r, false, "Smoke test failed: "+err.Error())
		return
	}
	defer s.pool.LeaseRelease(lease)
	defer func() { _ = up.Close() }()

	// Read a bounded prefix of the SSE stream for the preview.
	chatStart := time.Now()
	preview, readErr := readBounded(up, maxSmokeBytes)
	phases.Since(phasetiming.UpstreamTTFBMS, chatStart)
	phases.Since(phasetiming.TotalMS, start)
	ms := time.Since(start).Milliseconds()
	if readErr != nil {
		s.dash.RenderConfigResult(w, r, false, "Smoke test: upstream accepted but stream read failed: "+readErr.Error())
		return
	}
	s.dash.RenderSmokeResult(w, r, req.Model, tokenLabel(lease), ms, preview, dashboard.PhaseList(phases.All()))
}

// handlePlaygroundChat is the dashboard playground's streaming chat
// endpoint (issue #45): it routes a {model, prompt} through the exact same
// /v1/chat/completions pipeline (acquire → upstream → SSE relay) without an
// API key — dashboard auth + CSRF already ran. The page streams the SSE.
func (s *Server) handlePlaygroundChat(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "failed to read request: "+err.Error(), "invalid_request_error", "invalid_json", 0)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "request must be a JSON object", "invalid_request_error", "invalid_json", 0)
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Model == "" {
		if m := probeModel(s.reg); m != "" {
			req.Model = m
		} else {
			s.writeJSONError(w, http.StatusBadRequest, "no model specified and no models in the registry", "invalid_request_error", "model_not_found", 0)
			return
		}
	}
	if req.Prompt == "" {
		req.Prompt = "ping"
	}
	// Build a chat-completions request and run it through the real handler
	// (streaming forced, exactly like /v1/chat/completions).
	chatBody := []byte(`{"model":` + strconv.Quote(req.Model) +
		`,"messages":[{"role":"user","content":` + strconv.Quote(req.Prompt) + `}],"stream":true}`)
	playReq := r.Clone(r.Context())
	playReq.Body = io.NopCloser(bytes.NewReader(chatBody))
	playReq.ContentLength = int64(len(chatBody))
	s.handleChat(w, playReq)
}

// handleLoginStart begins the headless OAuth login wizard (issue #62):
// POST /admin/login/start → the server requests a fresh /api/auth/cli/code
// from upstream and returns {flow_id, login_url, expires_at} for the page
// to hand to the user; the page then polls GET /admin/login/status.
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if s.authClient == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "login wizard disabled (no upstream auth client)", "server_error", "login_unavailable", 0)
		return
	}
	s.pruneLoginFlows()
	code, err := s.authClient.StartCLILogin(r.Context())
	if err != nil {
		s.logger.Warn("login wizard: start failed", "err", err)
		s.writeJSONError(w, http.StatusBadGateway, "failed to start browser login: "+err.Error(), "server_error", "login_start_failed", 0)
		return
	}
	flowID := shortFlowID(code.FingerprintID)
	flow := &loginFlow{ID: flowID, Code: code, Started: time.Now()}
	s.loginMu.Lock()
	s.loginFlows[code.FingerprintID] = flow
	s.loginMu.Unlock()
	s.logger.Info("login wizard: started", "flow", flowID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"flow_id":     flowID,
		"fingerprint": code.FingerprintID, // full id: the status poll key
		"login_url":   code.LoginURL,
		"expires_at":  code.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// handleLoginStatus polls an in-flight login (issue #62): the server polls
// upstream /api/auth/cli/status; when the authToken appears the token is
// added to the live pool AND persisted to .env (survives restart), like the
// dashboard "Add token" action.
func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	s.pruneLoginFlows()
	fp := strings.TrimSpace(r.URL.Query().Get("fingerprint"))
	if fp == "" {
		s.writeJSONError(w, http.StatusBadRequest, "missing fingerprint query param", "invalid_request_error", "bad_request", 0)
		return
	}
	s.loginMu.Lock()
	flow := s.loginFlows[fp]
	s.loginMu.Unlock()
	if flow == nil {
		s.writeJSONError(w, http.StatusNotFound, "login flow not found or expired — start a new one", "invalid_request_error", "login_flow_missing", 0)
		return
	}
	// Read the completion state under the lock: concurrent status polls
	// (second tab, htmx retry) must not both proceed to addTokenPersist —
	// the completing flag is set before the network poll so exactly one
	// goroutine owns the add.
	s.loginMu.Lock()
	done := flow.Done
	completing := flow.Completing
	flow.Completing = true
	s.loginMu.Unlock()
	if done {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "token_index": flow.Index, "token": flow.Token})
		return
	}
	if completing {
		// Another poll is mid-completion; report pending so the client
		// re-polls instead of double-adding.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	status, err := s.authClient.PollCLILogin(r.Context(), flow.Code)
	if err != nil {
		// Transient poll failure: keep the flow alive, report pending. A
		// later poll may retry completion.
		s.loginMu.Lock()
		flow.Completing = false
		s.loginMu.Unlock()
		s.logger.Debug("login wizard: poll failed", "flow", flow.ID, "err", err)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	if !status.Done {
		s.loginMu.Lock()
		flow.Completing = false
		s.loginMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "pending"})
		return
	}
	// Completed: add to the pool + persist to .env (mirrors handleTokenAdd).
	// All completion fields are written under the lock so a concurrent poll
	// observing Done reads a consistent record.
	flow.Done = true
	flow.Token = status.AuthToken
	s.loginMu.Lock()
	s.loginFlows[fp] = flow
	s.loginMu.Unlock()
	index, addErr := s.addTokenPersist(r.Context(), status.AuthToken)
	if addErr != nil {
		flow.Error = addErr.Error()
		s.loginMu.Lock()
		flow.Completing = false
		s.loginMu.Unlock()
		s.logger.Warn("login wizard: token persist failed", "flow", flow.ID, "err", addErr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": addErr.Error()})
		return
	}
	flow.Index = index
	s.logger.Info("login wizard: completed", "flow", flow.ID, "token_index", index, "user", status.User.Name)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "token_index": index, "user": status.User.Name})
}

// addTokenPersist adds token to the live pool and persists the new
// AUTH_TOKENS list to .env (mirrors handleTokenAdd's mutation + persistence
// sequence, without the dashboard fragment render).
func (s *Server) addTokenPersist(ctx context.Context, token string) (int, error) {
	// Tier gate (mirrors handleTokenAdd): a banned/country-blocked token
	// minted from a datacenter IP must never enter the pool — it would fail
	// every request with 403 and amplify the ban (issue #140).
	if _, err := s.probeTokenGate(ctx, token); err != nil {
		return 0, fmt.Errorf("token rejected by probe: %w", err)
	}
	cfg := s.cfg.Load()
	existing := cfg.AuthTokens
	if len(existing) > 0 {
		idx, err := s.pool.AddToken(token)
		if err != nil {
			return 0, fmt.Errorf("add token to pool: %w", err)
		}
		// Persist the runtime list (pool may have bridge additions too, but
		// AUTH_TOKENS is the fixed set — append only when not already there).
		tokens := append([]string(nil), existing...)
		seen := false
		for _, t := range tokens {
			if t == token {
				seen = true
				break
			}
		}
		if !seen {
			tokens = append(tokens, token)
		}
		if err := s.syncTokensAfterMutation(tokens); err != nil {
			return 0, err
		}
		return idx, nil
	}
	// Bridge mode (no fixed tokens): the first wizard token switches to
	// pooled mode, exactly like handleTokenAdd.
	idx, err := s.pool.AddToken(token)
	if err != nil {
		return 0, fmt.Errorf("add token to pool: %w", err)
	}
	if err := s.syncTokensAfterMutation([]string{token}); err != nil {
		return 0, err
	}
	return idx, nil
}

// shortFlowID renders a compact flow id for the UI/logs.
func shortFlowID(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

// pruneLoginFlows drops flows older than loginFlowTTL.
func (s *Server) pruneLoginFlows() {
	cutoff := time.Now().Add(-loginFlowTTL)
	s.loginMu.Lock()
	for fp, flow := range s.loginFlows {
		if flow.Started.Before(cutoff) {
			delete(s.loginFlows, fp)
		}
	}
	s.loginMu.Unlock()
}

// readBounded reads up to n bytes from r, tolerating an EOF mid-prefix.
func readBounded(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:got], nil
}

// envUpdate is one KEY=VALUE replacement for updateEnvKeys.
type envUpdate struct {
	Key   string
	Value string
}

// updateEnvKeys rewrites the given KEY=VALUE lines in .env (appending each
// missing key), preserving every other line. The existing EOL style is
// preserved — CRLF files stay CRLF — so a Windows-edited .env is never
// rewritten with mixed line endings. Updates apply in order; later updates
// to an already-replaced key win (callers keep keys distinct).
func updateEnvKeys(updates []envUpdate) ([]byte, error) {
	content, err := os.ReadFile(".env")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	crlf := bytes.Contains(content, []byte("\r"))
	lines := make([]string, 0, len(content)/8)
	for _, l := range strings.Split(string(content), "\n") {
		lines = append(lines, strings.TrimSuffix(l, "\r"))
	}
	// A file ending with a newline has a trailing "" split element that is
	// an artifact of that newline, not a real blank line; drop it so
	// appended keys do not land after a spurious blank line.
	trailingNL := len(content) > 0 && content[len(content)-1] == '\n'
	if trailingNL {
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1]
		}
	}
	for _, u := range updates {
		line := u.Key + "=" + u.Value
		replaced := false
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), u.Key+"=") {
				lines[i] = line
				replaced = true
				break
			}
		}
		if !replaced {
			if n := len(lines); n == 1 && lines[0] == "" {
				// Empty (or missing) file: the new line is the whole file.
				lines[0] = line
			} else {
				lines = append(lines, line)
			}
		}
	}
	eol := "\n"
	if crlf {
		eol = "\r\n"
	}
	out := []byte(strings.Join(lines, eol))
	if trailingNL {
		out = append(out, eol...)
	}
	if err := writeFileAtomic(".env", out); err != nil {
		return nil, err
	}
	return out, nil
}

// updateAuthTokensEnv rewrites the AUTH_TOKENS= line in .env (appending it
// when absent), preserving every other line. Returns the new content. The
// existing EOL style is preserved — CRLF files stay CRLF — so a
// Windows-edited .env is never rewritten with mixed line endings.
func updateAuthTokensEnv(tokens []string) ([]byte, error) {
	return updateEnvKeys([]envUpdate{{Key: "AUTH_TOKENS", Value: strings.Join(tokens, ",")}})
}

// syncTokensAfterMutation updates .env + reloads config after a pool token
// mutation, so the change survives a restart and cfg reflects the new list.
// It fails (without touching the pool) when the reload does NOT land the
// requested tokens: a higher-precedence source — a real environment
// AUTH_TOKENS (docker-compose env_file injects every .env line into the
// container environment, which then overrides the file) or a -config JSON
// file — would otherwise leave cfg/pool/.env permanently divergent while
// the UI keeps claiming the token was persisted.
func (s *Server) syncTokensAfterMutation(tokens []string) error {
	// Snapshot the .env before writing so a reload-verification failure can
	// restore it byte-exact (mirrors handleModeSwitch's persist → verify →
	// rollback). Otherwise the failed add leaves AUTH_TOKENS=<new> in .env
	// while the live pool holds the old list — the very divergence the
	// caller is trying to avoid.
	old, oldErr := os.ReadFile(".env")
	if _, err := updateAuthTokensEnv(tokens); err != nil {
		return fmt.Errorf("persist AUTH_TOKENS: %w", err)
	}
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		restoreEnvFile(old, oldErr)
		return fmt.Errorf("reload config: %w", err)
	}
	if !reflect.DeepEqual(newCfg.AuthTokens, tokens) {
		restoreEnvFile(old, oldErr)
		return fmt.Errorf("AUTH_TOKENS=%q overrides .env (environment or -config JSON) — the %d token(s) were persisted to .env but NOT activated; clear AUTH_TOKENS from the environment/config, or restart the container without env_file, then retry", strings.Join(newCfg.AuthTokens, ","), len(tokens))
	}
	s.cfg.Store(&newCfg)
	s.reg.SetConfig(&newCfg)
	s.pool.SetConfig(&newCfg)
	s.rateLimiter.SetRate(newCfg.RateLimitPerIP, newCfg.RateLimitBurst)
	return nil
}

// probeTokenGate validates a token BEFORE it enters the pool (dashboard Add
// Token + login wizard). The zero-cost GET session probe is authoritative
// for account health: a banned or country-blocked account would otherwise be
// added and then fail every chat call with 403 — the exact incident that
// banned the account (repeated 403s → demotion → ban, issue #140). The
// probe claims no session slot and burns no daily allowance.
//
// Returns the session state when the token is usable (any non-terminal
// status: active/queued/disabled/idle), or a wrapped error when the account
// is dead (ErrBanned / ErrCountryBlocked / ErrAuthRejected / ErrRateLimited
// / ErrNoActiveSession is NOT fatal — a token with no active session is
// still valid and will admit on first use).
func (s *Server) probeTokenGate(ctx context.Context, token string) (*upstream.SessionState, error) {
	state, err := s.pool.ProbeNewToken(ctx, token)
	if err != nil {
		if errors.Is(err, upstream.ErrNoActiveSession) {
			// No active session is fine: the pool will create one on first
			// use. Treat as usable.
			return state, nil
		}
		return nil, err
	}
	if state != nil {
		switch state.Status {
		case "banned":
			return nil, fmt.Errorf("token is banned upstream (status banned): %w", upstream.ErrBanned)
		case "country_blocked":
			return nil, fmt.Errorf("token is country-blocked upstream: %w", upstream.ErrCountryBlocked)
		}
	}
	return state, nil
}

// handleTokenAdd adds a token to the live pool and persists it (dashboard
// "Add token"). Rolls the pool back if persistence fails.
func (s *Server) handleTokenAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	req.Token = strings.TrimSpace(r.FormValue("token"))
	if req.Token == "" {
		// JSON fallback for programmatic clients.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<10))
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Invalid request: "+err.Error())
			return
		}
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" || strings.HasPrefix(strings.ToLower(req.Token), "bearer ") {
		s.dash.RenderConfigResult(w, r, false, "Invalid token (must not start with 'Bearer ').")
		return
	}

	// adminSaveMu serializes the pool mutation + persist + reload with the
	// other .env writers (config editor, token remove, mode switch) so a
	// concurrent save cannot interleave and lose a token from .env.
	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()

	cfg := s.cfg.Load()
	// Divergence guard (mirrors handleTokenRemove): a config-editor
	// AUTH_TOKENS edit or /admin/reload can diverge cfg.AuthTokens from the
	// live pool. Adding to a stale list would persist cfg.AuthTokens+new to
	// .env while the pool holds its own list, leaving pool/.env/cfg
	// permanently divergent — and the next remove is rejected by the same
	// guard, stranding the operator until restart.
	if len(cfg.AuthTokens) != s.pool.TokenCount() {
		s.dash.RenderConfigResult(w, r, false, "AUTH_TOKENS in .env differs from the live pool — reconcile in the Config editor or restart.")
		return
	}
	// Tier gate: reject dead accounts before they enter the pool. The probe
	// is zero-cost (no session slot claimed); a banned/country-blocked/
	// auth-rejected token is refused with a clear message instead of being
	// added and failing every request with 403 (the ban amplifier).
	state, err := s.probeTokenGate(r.Context(), req.Token)
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, "Token rejected by probe: "+err.Error())
		return
	}
	if state != nil && state.AccessTier != "" {
		s.logger.Info("dashboard token probed", "remote", remoteHost(r), "tier", state.AccessTier, "status", state.Status)
	}
	idx, err := s.pool.AddToken(req.Token)
	if err != nil {
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	// Build the persist list from cfg (the fixed AUTH_TOKENS set) plus the
	// new token, skipping any token already present: a duplicate add must
	// not write `tok,cb,cb` to .env — splitList would collapse it on reload
	// and the strict reload check would reject the add and roll back.
	tokens := append([]string{}, cfg.AuthTokens...)
	seen := false
	for _, t := range tokens {
		if t == req.Token {
			seen = true
			break
		}
	}
	if !seen {
		tokens = append(tokens, req.Token)
	}
	if err := s.syncTokensAfterMutation(tokens); err != nil {
		_ = s.pool.RemoveLastToken()
		s.logger.Warn("dashboard token add rolled back", "remote", remoteHost(r), "err", err)
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	s.logger.Info("dashboard token added", "remote", remoteHost(r), "index", idx)
	s.dash.RenderConfigResult(w, r, true, "Token added at index "+strconv.Itoa(idx)+" and persisted to .env.")
}

// handleTokenRemove removes the last pooled token (dashboard "Remove last").
func (s *Server) handleTokenRemove(w http.ResponseWriter, r *http.Request) {
	// adminSaveMu serializes the pool mutation + persist + reload with the
	// other .env writers, exactly like handleTokenAdd.
	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()

	cfg := s.cfg.Load()
	// A config-editor AUTH_TOKENS edit or /admin/reload can diverge
	// cfg.AuthTokens from the live pool; removing "the last token" from a
	// stale list would persist the wrong .env and leave pool/.env/cfg
	// permanently inconsistent.
	if len(cfg.AuthTokens) != s.pool.TokenCount() {
		s.dash.RenderConfigResult(w, r, false, "AUTH_TOKENS in .env differs from the live pool — reconcile in the Config editor or restart.")
		return
	}
	removed := ""
	if len(cfg.AuthTokens) > 0 {
		removed = cfg.AuthTokens[len(cfg.AuthTokens)-1]
	}
	if err := s.pool.RemoveLastToken(); err != nil {
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	tokens := cfg.AuthTokens
	if len(tokens) > 0 {
		tokens = tokens[:len(tokens)-1]
	}
	if err := s.syncTokensAfterMutation(tokens); err != nil {
		// Roll the pool back so a failed persist does not leave the token
		// removed from the pool but still listed in .env/cfg (mirrors
		// handleTokenAdd's rollback).
		if removed != "" {
			if _, addErr := s.pool.AddToken(removed); addErr != nil {
				s.logger.Warn("dashboard token remove rollback re-add failed", "remote", remoteHost(r), "err", addErr)
			}
		}
		s.logger.Warn("dashboard token remove rolled back", "remote", remoteHost(r), "err", err)
		s.dash.RenderConfigResult(w, r, false, err.Error())
		return
	}
	s.logger.Info("dashboard token removed", "remote", remoteHost(r))
	s.dash.RenderConfigResult(w, r, true, "Last token removed and persisted to .env.")
}

// handleModeSwitch flips between bridge, pooled, and hybrid mode at runtime
// (dashboard mode control). Pooled→bridge removes all tokens; bridge→pooled
// requires at least one token to add (use the Add-token form first). Hybrid
// keeps the pooled tokens and additionally relays client-supplied tokens;
// switching to it persists HYBRID_MODE=true in .env.
//
// Order matters: the .env is persisted and the config reloaded BEFORE the
// live pool is drained, and the reload result is verified to actually be in
// the requested mode. Otherwise a failed persist (or a higher-precedence
// AUTH_TOKENS/HYBRID_MODE source such as a -config JSON file or real
// environment variable) would empty the pool while cfg still claims pooled —
// leaving the proxy broken and the dashboard pill showing the old mode.
func (s *Server) handleModeSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	req.Mode = r.FormValue("mode")
	if req.Mode == "" {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request: "+err.Error())
			return
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Invalid request: "+err.Error())
			return
		}
	}
	cfg := s.cfg.Load()
	switch strings.ToLower(strings.TrimSpace(req.Mode)) {
	case "bridge":
		if cfg.BridgeMode() && !cfg.HybridMode {
			s.dash.RenderConfigResult(w, r, false, "Already in bridge mode.")
			return
		}
		// adminSaveMu serializes the persist → verify → rollback sequence
		// with the other .env writers (config editor, token add/remove) so a
		// concurrent save cannot interleave between the write and the reload.
		// The live-pool drain stays outside the lock, after the reload is
		// verified (persist → verify → drain).
		s.adminSaveMu.Lock()
		defer s.adminSaveMu.Unlock()
		// Persist AUTH_TOKENS= (explicit empty) + HYBRID_MODE=false and
		// reload, verifying the effective config actually lands in bridge
		// mode before touching the live pool. Roll the .env back on failure.
		old, oldErr := os.ReadFile(".env")
		if _, err := updateEnvKeys([]envUpdate{{Key: "AUTH_TOKENS", Value: ""}, {Key: "HYBRID_MODE", Value: "false"}}); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to persist .env: "+err.Error())
			return
		}
		newCfg, err := config.Load(s.configPath)
		if err != nil {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Reload rejected: "+err.Error())
			return
		}
		if !newCfg.BridgeMode() {
			// A higher-precedence source (e.g. AUTH_TOKENS in a -config JSON
			// file or the real environment) still supplies tokens — .env alone
			// cannot clear it, so the switch cannot succeed.
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Could not switch to bridge mode: AUTH_TOKENS is still set by a -config JSON file or the environment, which overrides .env. Clear it there, or run without -config, then retry.")
			return
		}
		s.cfg.Store(&newCfg)
		s.reg.SetConfig(&newCfg)
		s.pool.SetConfig(&newCfg)
		s.rateLimiter.SetRate(newCfg.RateLimitPerIP, newCfg.RateLimitBurst)
		s.pool.RemoveAllTokens(r.Context())
		s.logger.Info("dashboard switched to bridge mode")
		s.dash.RenderConfigResult(w, r, true, "Switched to bridge mode — AUTH_TOKENS cleared; clients now send their own token.")
	case "pooled":
		if !cfg.BridgeMode() && !cfg.HybridMode {
			s.dash.RenderConfigResult(w, r, false, "Already in pooled mode.")
			return
		}
		if cfg.BridgeMode() {
			s.dash.RenderConfigResult(w, r, false, "Pooled mode needs tokens — add one via the Add-token form first.")
			return
		}
		// Hybrid → pooled: keep the tokens, just clear HYBRID_MODE.
		s.adminSaveMu.Lock()
		defer s.adminSaveMu.Unlock()
		old, oldErr := os.ReadFile(".env")
		if _, err := updateEnvKeys([]envUpdate{{Key: "HYBRID_MODE", Value: "false"}}); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to persist .env: "+err.Error())
			return
		}
		newCfg, err := config.Load(s.configPath)
		if err != nil {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Reload rejected: "+err.Error())
			return
		}
		if newCfg.HybridMode {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Could not switch to pooled mode: HYBRID_MODE is still true via a -config JSON file or the environment, which overrides .env. Clear it there, then retry.")
			return
		}
		s.cfg.Store(&newCfg)
		s.reg.SetConfig(&newCfg)
		s.pool.SetConfig(&newCfg)
		s.rateLimiter.SetRate(newCfg.RateLimitPerIP, newCfg.RateLimitBurst)
		s.logger.Info("dashboard switched to pooled mode", "auth_tokens", len(newCfg.AuthTokens))
		s.dash.RenderConfigResult(w, r, true, "Switched to pooled mode — HYBRID_MODE cleared; all requests now use the pool.")
	case "hybrid":
		if cfg.HybridMode {
			s.dash.RenderConfigResult(w, r, false, "Already in hybrid mode.")
			return
		}
		// Hybrid → pooled: keep the tokens, just clear HYBRID_MODE.
		s.adminSaveMu.Lock()
		defer s.adminSaveMu.Unlock()
		old, oldErr := os.ReadFile(".env")
		if _, err := updateEnvKeys([]envUpdate{{Key: "HYBRID_MODE", Value: "true"}}); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to persist .env: "+err.Error())
			return
		}
		newCfg, err := config.Load(s.configPath)
		if err != nil {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Reload rejected: "+err.Error())
			return
		}
		if !newCfg.HybridMode {
			restoreEnvFile(old, oldErr)
			s.dash.RenderConfigResult(w, r, false, "Could not switch to hybrid mode: HYBRID_MODE is still false via a -config JSON file or the environment, which overrides .env. Set it there, then retry.")
			return
		}
		s.cfg.Store(&newCfg)
		s.reg.SetConfig(&newCfg)
		s.pool.SetConfig(&newCfg)
		s.rateLimiter.SetRate(newCfg.RateLimitPerIP, newCfg.RateLimitBurst)
		msg := "Switched to hybrid mode — clients with a token relay it; token-less requests use the pool."
		if len(newCfg.AuthTokens) == 0 {
			msg += " Warning: no AUTH_TOKENS — token-less requests will fail (502) until a token is added."
			s.logger.Warn("hybrid mode enabled without AUTH_TOKENS: token-less requests will 502 until a token is added")
		} else {
			s.logger.Info("dashboard switched to hybrid mode", "auth_tokens", len(newCfg.AuthTokens))
		}
		s.dash.RenderConfigResult(w, r, true, msg)
	default:
		s.dash.RenderConfigResult(w, r, false, "Mode must be 'bridge', 'pooled', or 'hybrid'.")
	}
}

// restoreEnvFile writes old content back to .env, or removes the file when it
// did not exist before. Best-effort rollback for failed mode switches. When
// the previous .env existed but was unreadable (oldErr not os.ErrNotExist),
// nothing is done: removing it would destroy the operator's file, and the old
// bytes needed for a restore were never read.
func restoreEnvFile(old []byte, oldErr error) {
	switch {
	case oldErr == nil:
		_ = writeFileAtomic(".env", old)
	case errors.Is(oldErr, os.ErrNotExist):
		_ = os.Remove(".env")
	}
}

// dialTarget returns the host:port to dial for an upstream base host,
// defaulting to 443 only when the host carries no explicit port — an
// UpstreamBaseURL like "https://host:8443" must not become "host:8443:443".
func dialTarget(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, "443")
}

// handleDiag runs the dashboard diagnostics: config state, upstream
// reachability (DNS + TLS), registry health, and per-token validity probes —
// the same checks -doctor performs, rendered as a fragment. The probes are
// zero-cost upstream GETs (no session claim, no model needed), so they always
// run for pooled and hybrid modes.
func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	checks := []dashboard.DiagCheck{}

	cfg := s.cfg.Load()
	switch cfg.EffectiveMode() {
	case "bridge":
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: "Configuration: bridge mode (clients relay their own token)"})
	case "hybrid":
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: fmt.Sprintf("Configuration: hybrid mode, %d pooled token(s) (client tokens relayed; token-less requests use the pool)", len(cfg.AuthTokens))})
	default:
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: fmt.Sprintf("Configuration: pooled mode, %d token(s)", len(cfg.AuthTokens))})
	}

	// Upstream reachability: DNS + TLS to the configured base host. The DNS
	// lookup uses the bare host, not u.Host verbatim: "host:8443" would be
	// treated as a literal DNS name and NXDOMAIN, a false red row (the -doctor
	// tool strips the port the same way). The display and dial target keep the
	// port so the TCP row still connects to the real endpoint.
	targetHost := "www.codebuff.com"
	dnsHost := targetHost
	if u, err := url.Parse(cfg.UpstreamBaseURL); err == nil && u.Host != "" {
		targetHost = u.Host
		if h := u.Hostname(); h != "" {
			dnsHost = h
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(ctx, dnsHost); err != nil {
		checks = append(checks, dashboard.DiagCheck{Message: "DNS lookup failed for " + dnsHost + ": " + err.Error()})
	} else {
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: "DNS resolves " + dnsHost})
	}
	hostForDial := dialTarget(targetHost)
	if conn, err := net.DialTimeout("tcp", hostForDial, 5*time.Second); err != nil {
		checks = append(checks, dashboard.DiagCheck{Message: "TCP connect to " + hostForDial + " failed: " + err.Error()})
	} else {
		_ = conn.Close()
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: "TCP reachable " + hostForDial})
	}

	checks = append(checks, dashboard.DiagCheck{OK: true, Message: fmt.Sprintf("Model registry: %d models", s.reg.ModelCount())})

	// Per-token validity probes (pooled and hybrid-with-tokens modes). Each
	// probe is a zero-cost upstream GET /api/v1/freebuff/session (no session
	// claim, no model needed), so they always run; a token with no active
	// session still counts as valid.
	if !cfg.BridgeMode() {
		for _, snap := range s.pool.PoolSnapshot().Tokens {
			idx := snap.Token
			probeCtx, probeCancel := context.WithTimeout(r.Context(), 8*time.Second)
			state, err := s.pool.ProbeToken(probeCtx, idx)
			probeCancel()
			switch {
			case errors.Is(err, upstream.ErrNoActiveSession):
				checks = append(checks, dashboard.DiagCheck{OK: true, Message: fmt.Sprintf("Token #%d validity probe succeeded (no active session)", idx+1)})
			case err != nil:
				checks = append(checks, dashboard.DiagCheck{Message: fmt.Sprintf("Token #%d validity probe failed: %v", idx+1, err)})
			default:
				msg := fmt.Sprintf("Token #%d validity probe succeeded", idx+1)
				if q := quotaSummary(state); q != "" {
					msg += " (" + q + ")"
				}
				checks = append(checks, dashboard.DiagCheck{OK: true, Message: msg})
			}
		}
	} else {
		checks = append(checks, dashboard.DiagCheck{Warn: true, Message: "No pooled tokens to probe (the smoke test uses a client token)."})
	}

	s.dash.RenderDiag(w, r, checks)
}

// handleConfigSave persists the submitted .env text and hot-reloads the
// config. The flow: write the file atomically (temp + rename) → full
// config.Load("") — the same pipeline used at startup, so every semantic
// validation (durations, URLs, fingerprints, Validate) runs — and swap the
// atomic pointer. Any failure restores the previous .env content. adminSaveMu
// serializes concurrent saves so a rejected save can never clobber a newer
// accepted one.
func (s *Server) handleConfigSave(w http.ResponseWriter, r *http.Request) {
	const envPath = ".env"
	r.Body = http.MaxBytesReader(w, r.Body, maxEnvSize)

	// The dashboard textarea posts application/x-www-form-urlencoded
	// (name="content"); a raw urlencoded body written verbatim as .env would
	// become "content=KEY=VALUE..." and destroy the file. Programmatic
	// clients (text/plain) post the raw .env text and keep the raw path.
	var content []byte
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request form.")
			return
		}
		content = []byte(r.FormValue("content"))
	} else {
		var err error
		content, err = io.ReadAll(r.Body)
		if err != nil {
			s.dash.RenderConfigResult(w, r, false, "Failed to read request body.")
			return
		}
	}

	// Guard: an empty payload (urlencoded POST without content=, or an empty
	// text/plain body) must never write an empty .env. config.Load succeeds
	// on an empty file with built-in defaults, so the write would silently
	// wipe the operator's AUTH_TOKENS/ADMIN_TOKEN/API_KEYS/SAFE_MODE while
	// reporting a green "Saved and reloaded". Reject it and leave the file
	// untouched.
	if len(bytes.TrimSpace(content)) == 0 {
		s.dash.RenderConfigResult(w, r, false, "Configuration rejected: empty .env content — nothing to save.")
		return
	}

	s.adminSaveMu.Lock()
	defer s.adminSaveMu.Unlock()

	old, oldErr := os.ReadFile(envPath)
	if err := writeFileAtomic(envPath, content); err != nil {
		s.dash.RenderConfigResult(w, r, false, "Failed to write .env: "+err.Error())
		return
	}
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		switch {
		case oldErr == nil:
			_ = writeFileAtomic(envPath, old)
		case errors.Is(oldErr, os.ErrNotExist):
			// The .env did not exist before the save: remove the rejected
			// write so the state matches.
			_ = os.Remove(envPath)
		default:
			// The previous .env existed but was unreadable (permissions, ACL):
			// deleting it would destroy the operator's file. Leave the newly
			// written content and warn — a restore is impossible without the
			// old bytes.
			s.logger.Warn("dashboard config save rejected; previous .env unreadable, not restored", "readErr", oldErr, "err", err)
		}
		s.logger.Warn("dashboard config save rejected", "err", err)
		s.dash.RenderConfigResult(w, r, false, "Configuration rejected: "+err.Error())
		return
	}
	oldCfg := s.cfg.Load()
	s.cfg.Store(&newCfg)
	s.reg.SetConfig(&newCfg)
	s.pool.SetConfig(&newCfg)
	s.rateLimiter.SetRate(newCfg.RateLimitPerIP, newCfg.RateLimitBurst)
	s.logger.Info("dashboard config saved and reloaded",
		"remote", remoteHost(r), "changed_keys", changedConfigKeys(oldCfg, &newCfg),
		"auth_tokens", len(newCfg.AuthTokens), "safe_mode", newCfg.SafeMode)
	s.dash.RenderConfigResult(w, r, true, "Saved and reloaded — effective configuration updated.")
}

// effectiveConfigKV renders cfg as a key→normalized-value map of the
// effective config surface (mirrors the dashboard config editor's effective
// table, T15). Secret-bearing values are reduced to counts or set/unset
// markers, so the map is safe to diff for the changed_keys audit log: only
// key NAMES are ever logged, never values.
func effectiveConfigKV(cfg *config.Config) map[string]string {
	return map[string]string{
		"LISTEN_ADDR":                           cfg.ListenAddr,
		"UPSTREAM_BASE_URL":                     cfg.UpstreamBaseURL,
		"AUTH_TOKENS":                           strconv.Itoa(len(cfg.AuthTokens)),
		"API_KEYS":                              strconv.Itoa(len(cfg.APIKeys)),
		"ADMIN_TOKEN":                           boolWord(cfg.AdminToken != ""),
		"ROTATION_INTERVAL":                     cfg.RotationInterval.String(),
		"REQUEST_TIMEOUT":                       cfg.RequestTimeout.String(),
		"SESSION_CALL_TIMEOUT":                  cfg.SessionCallTimeout.String(),
		"COST_MODE":                             cfg.CostMode,
		"TLS_FINGERPRINT":                       cfg.TLSFingerprint,
		"REGISTRY_REFRESH":                      cfg.RegistryRefresh.String(),
		"DEBUG_DUMP":                            strconv.FormatBool(cfg.DebugDump),
		"LOG_FILE":                              cfg.LogFile,
		"LOG_LEVEL":                             cfg.LogLevel,
		"LOG_FORMAT":                            cfg.LogFormat,
		"LOG_ACCESS":                            strconv.FormatBool(cfg.LogAccess),
		"LOG_RING_SIZE":                         strconv.Itoa(cfg.LogRingSize),
		"MAX_MESSAGES_PER_DAY":                  strconv.Itoa(cfg.MaxMessagesPerDay),
		"MAX_SPEND_PER_DAY":                     strconv.FormatInt(cfg.MaxSpendPerDay, 10),
		"IDLE_ROTATION_TIMEOUT":                 cfg.IdleRotationTimeout.String(),
		"SAFE_MODE":                             strconv.FormatBool(cfg.SafeMode),
		"HYBRID_MODE":                           strconv.FormatBool(cfg.HybridMode),
		"MODELS_HIDE_UNAVAILABLE":               strconv.FormatBool(cfg.ModelsHideUnavailable),
		"MODELS_ALLOW":                          strings.Join(cfg.ModelsAllow, ","),
		"CORS_ALLOWED_ORIGIN":                   cfg.CORSAllowedOrigin,
		"REQUEST_JITTER":                        cfg.RequestJitter.String(),
		"CLI_VERSION":                           cfg.CLIVersion,
		"MODEL_ALIASES":                         strconv.Itoa(len(cfg.ModelAliases)),
		"TRANSIENT_RETRIES":                     strconv.Itoa(cfg.TransientRetries),
		"SESSION_PERSIST":                       strconv.FormatBool(cfg.SessionPersist),
		"SESSION_STATE_FILE":                    cfg.SessionStateFile,
		"HTTP2_UPSTREAM":                        strconv.FormatBool(cfg.HTTP2Upstream),
		"SESSION_CREATE_MAX_PARALLEL_GLOBAL":    strconv.Itoa(cfg.SessionCreateMaxParallelGlobal),
		"SESSION_CREATE_MAX_PARALLEL_PER_MODEL": strconv.Itoa(cfg.SessionCreateMaxParallelPerModel),
		"RUN_FINISH_QUEUE_SIZE":                 strconv.Itoa(cfg.RunFinishQueueSize),
		"RUN_FINISH_INLINE_TIMEOUT":             cfg.RunFinishInlineTimeout.String(),
		"RUNS_DRAIN_QUEUE_CAP":                  strconv.Itoa(cfg.RunsDrainQueueCap),
		"RUNS_DRAIN_TTL":                        cfg.RunsDrainTTL.String(),
		"SESSION_RE_ADMIT_LEAD":                 cfg.SessionReAdmitLead.String(),
		"SESSION_PROBE_CACHE_TTL":               cfg.SessionProbeCacheTTL.String(),
		"WEBHOOK_URL":                           boolWord(cfg.WebhookURL != ""),
		"FALLBACK_AFTER_MS":                     cfg.FallbackAfter.String(),
		"FALLBACK_MODEL":                        strconv.Itoa(len(cfg.FallbackModels)),
		"ADOPT_CLI_SESSION":                     strconv.FormatBool(cfg.AdoptCLISession),
		"WAITING_ROOM_CHAIN":                    strconv.FormatBool(cfg.WaitingRoomChain),
	}
}

// boolWord renders a boolean flag as "set"/"unset" for the redacted
// effective-config table (never the raw value).
func boolWord(v bool) string {
	if v {
		return "set"
	}
	return "unset"
}

// changedConfigKeys returns the sorted names of effective config keys whose
// normalized value differs between oldCfg and newCfg (T15 audit trail). The
// values are compared only; never logged.
func changedConfigKeys(oldCfg, newCfg *config.Config) []string {
	oldKV := effectiveConfigKV(oldCfg)
	newKV := effectiveConfigKV(newCfg)
	var changed []string
	for k, v := range newKV {
		if oldKV[k] != v {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	return changed
}

// writeFileAtomic writes data to path via a temp file + rename: readers never
// observe a truncated file, and a crash mid-write leaves the previous content
// intact. os.Rename replaces an existing target atomically on every supported
// platform (Windows uses MoveFileEx with MOVEFILE_REPLACE_EXISTING); only
// filesystems without atomic replace support need the remove-then-rename
// fallback (a tiny non-atomic window, acceptable for an admin action).
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			// The target exists but rename-over-existing failed: fall back
			// to removing it first, then renaming.
			_ = os.Remove(path)
			if err := os.Rename(tmpName, path); err == nil {
				return nil
			} else {
				_ = os.Remove(tmpName)
				return err
			}
		}
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// clientToken returns the request's bearer token (Authorization: Bearer or
// x-api-key), trimmed. Empty when the request carries none. In bridge mode
// this token IS the client's FreeBuff token relayed upstream.
func clientToken(r *http.Request) string {
	provided := ""
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		provided = tok
	} else if h := r.Header.Get("x-api-key"); h != "" {
		provided = h
	}
	return strings.TrimSpace(provided)
}

// bearerToken returns only the Authorization: Bearer token. In hybrid mode
// this is the discriminator between bridge traffic (client relays its own
// FreeBuff token) and pooled traffic (no bearer; x-api-key is the API_KEYS
// scheme and must never be relayed upstream as a FreeBuff credential).
func bearerToken(r *http.Request) string {
	if tok, ok := extractBearerToken(r.Header.Get("Authorization")); ok {
		return tok
	}
	return ""
}

// --- correlation ids ---

// reqIDKey carries the per-request correlation id (req_id) through the
// request context. The key type is unexported so only this package can
// read/write it; the upstream client threads the same id a second way (via
// ChatOptions.RequestID) for its do()/retry log lines.
type reqIDKey struct{}

// reqIDFrom returns the request's correlation id, or "" when the request
// did not pass through the access wrapper (direct handler calls in tests).
func reqIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

// newReqID mints a UUIDv4 correlation id from crypto/rand (RFC 4122 §4.4:
// 122 random bits, version 4, variant 1). A rand failure is unrecoverable
// in practice; fall back to a time-seeded hex id rather than failing the
// request.
func newReqID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// clientRequestID sanitizes the client's X-Request-Id header for logging:
// trimmed, printable ASCII only (0x20-0x7e), max 64 runes. Returns "" when
// the header is absent or fails the checks — the field is then omitted from
// log lines (the proxy never trusts a client-supplied id as its correlation
// key, D1).
func clientRequestID(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if v == "" || utf8.RuneCountInString(v) > 64 {
		return ""
	}
	for _, b := range []byte(v) {
		if b < 0x20 || b > 0x7e {
			return ""
		}
	}
	return v
}

// handleReload handles POST /admin/reload for hot configuration reloads (#26).
func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("admin reload requested", "remote", remoteHost(r), "path", r.URL.Path)
	newCfg, err := config.Load(s.configPath)
	if err != nil {
		s.logger.Warn("admin reload failed", "remote", remoteHost(r), "path", r.URL.Path, "err", err)
		s.writeJSONError(w, http.StatusInternalServerError, "failed to reload config: "+err.Error(), "internal_error", "reload_failed", 0)
		return
	}
	s.cfg.Store(&newCfg)
	s.reg.SetConfig(&newCfg)
	s.pool.SetConfig(&newCfg)
	s.rateLimiter.SetRate(newCfg.RateLimitPerIP, newCfg.RateLimitBurst)
	s.logger.Info("config reloaded successfully", "remote", remoteHost(r), "path", r.URL.Path,
		"auth_tokens", len(newCfg.AuthTokens), "safe_mode", newCfg.SafeMode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"message":     "configuration reloaded",
		"auth_tokens": len(newCfg.AuthTokens),
		"safe_mode":   newCfg.SafeMode,
	})
}
