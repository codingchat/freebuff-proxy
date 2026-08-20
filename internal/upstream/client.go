// Package upstream implements the codebuff.com wire client with the CLI
// request envelope required to pass the free-mode gate
// (403 free_mode_cli_required): x-freebuff-* headers, codebuff_metadata,
// provider.data_collection=deny, forced streaming, and the cb_easp stop
// sentinel. Error handling mirrors proxy-freebuff's recovery matrix: typed
// sentinels let callers refresh sessions, rotate runs, or cool down tokens.
package upstream

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	cryptoRand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/net/http2"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/stealth"
	"freebuff-proxy/internal/telemetry"
)

// Client speaks the codebuff.com wire protocol for a single token.
type Client struct {
	token      string
	tokenIndex int // 0-based index into the pool's token list (0 for bridge clients)
	baseURL    string
	http       *http.Client

	requestTimeout     time.Duration
	sessionCallTimeout time.Duration
	requestJitter      time.Duration
	costMode           string
	userID             string // optional x-freebuff-acting-user-id (ACTING_USER_ID; see New's doc + client.go acting-user comment: only the token's OWN account id is safe)
	debugDump          bool

	// transientRetriesLimit is TRANSIENT_RETRIES: the maximum number of
	// additional attempts after a transient transport failure (0 disables
	// retries entirely). Only transport-level failures (dial/TLS/reset/EOF)
	// retry; classified upstream errors never do.
	transientRetriesLimit int

	// capacityDeferredRetries counts free_mode_capacity_deferred retries
	// served by this client: the free-tier capacity queue is retried
	// in-place against the SAME lease/session, bounded by the
	// TRANSIENT_RETRIES budget (per-request, tracked separately from
	// transient transport retries).
	capacityDeferredRetries atomic.Int64

	// stealthProfile is the active TLS fingerprint. profileMu guards swaps
	// made by the retry loop (rotating the pinned profile before a retry);
	// newRequest and the dialer read it per request/connection. nil means
	// the plain Go transport.
	profileMu      sync.Mutex
	stealthProfile *stealth.Profile

	// http2Upstream negotiates HTTP/2 with the upstream so the TLS ALPN list
	// matches real browsers ("h2,http/1.1") instead of the h1-only list that
	// is itself a JA4 ALPN mismatch (#51). false forces HTTP/1.1.
	http2Upstream bool

	// risk is the passive ban-risk engine fed from session/probe responses
	// (#64). Production always uses stealth.DefaultRiskEngine; nil disables
	// feeding (test seam).
	risk *stealth.RiskEngine

	// Counters surfaced via the pool snapshot for /metrics.
	transientRetries     atomic.Int64 // transient transport failures retried
	fingerprintRotations atomic.Int64 // pinned fingerprint swaps ahead of a retry

	// rateLimitEvents is the T7 rate-limit ledger: upstream rate-limit
	// classifications counted by body code (rate_limited, spend_limited,
	// ip_capped, insufficient_quota, limit_burst_rate,
	// free_mode_rate_limited, ...). rateLimitMu guards the map; values are
	// atomics so snapshot reads never race a concurrent classification.
	rateLimitMu     sync.Mutex
	rateLimitEvents map[string]*atomic.Int64

	// waitingRoomRequired records that the last upstream refusal was a 428
	// waiting_room_required (issue #94): the pre-session ad-chain + streak
	// flow must fire before the next session create (WAITING_ROOM_CHAIN
	// gate). Set by classifyError; consumed (cleared) by the pool's
	// acquire path when the chain fires.
	waitingRoomRequired atomic.Bool

	// authOnly marks a token-less client built by NewForAuth (issue #62):
	// newRequest must never attach auth headers (there is no credential),
	// and the /api/auth/cli/* flow uses its own login-request helper.
	authOnly bool

	// retryBackoff overrides the randomized 200-600ms pre-retry sleep (test
	// seam; nil uses the crypto/rand jitter).
	retryBackoff func() time.Duration
}

// TokenKey returns a stable, non-secret key derived from the client token
// for session-state persistence. The key is a SHA-256 hex digest of the raw
// token, so the token itself never appears in the persisted file.
func (c *Client) TokenKey() string {
	sum := sha256.Sum256([]byte(c.token))
	return hex.EncodeToString(sum[:])
}

// cliUserAgent mirrors the official CLI chat user agent: the pinned
// @codebuff/llm-providers version, NOT the CLI_VERSION knob
// (reference/freebuff model-provider.ts:150; llm-providers package.json
// 1.0.0). The upstream free-tier gate (403 free_mode_cli_required) keys on
// the CLI request envelope (x-freebuff-* headers, codebuff_metadata, forced
// streaming and the cb_easp stop sentinel — see the package comment), but
// the server still fingerprints the UA, and 0.10.7 (the SDK version) is
// never emitted by a real CLI. Every upstream API call (chat + session +
// agent-runs) sends this UA — no browser persona (#108/#109).
const cliUserAgent = "ai-sdk/openai-compatible/1.0.0/codebuff"

const (
	// maxErrorBodyRead caps the upstream error response body read for
	// classification and logging.
	maxErrorBodyRead = 2048
	// maxDumpRead caps the debug dump body read.
	maxDumpRead = 51200
)

// New builds the client for one token.
func New(token string, cfg *config.Config) (*Client, error) {
	return NewWithIndex(token, 0, cfg)
}

// NewWithIndex builds the client for token at tokenIndex (the token's
// 0-based position in the pool's token list). Egress is always DIRECT: this
// gateway spoofs the official FreeBuff CLI, which has no outbound proxy
// machinery anywhere, and the upstream server hard-blocks proxy/VPN/Tor
// egress — a proxy would only add ban risk.
func NewWithIndex(token string, tokenIndex int, cfg *config.Config) (*Client, error) {
	if token == "" {
		return nil, errors.New("upstream: empty token")
	}
	if cfg == nil {
		return nil, errors.New("upstream: nil config")
	}

	c := &Client{
		token:                 token,
		tokenIndex:            tokenIndex,
		baseURL:               cfg.UpstreamBaseURL,
		requestTimeout:        cfg.RequestTimeout,
		sessionCallTimeout:    cfg.SessionCallTimeout,
		requestJitter:         cfg.RequestJitter,
		costMode:              cfg.CostMode,
		userID:                cfg.ActingUserID,
		debugDump:             cfg.DebugDump,
		transientRetriesLimit: cfg.TransientRetries,
		http2Upstream:         cfg.HTTP2Upstream,
		risk:                  stealth.DefaultRiskEngine,
		rateLimitEvents:       make(map[string]*atomic.Int64),
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	var baseDial func(ctx context.Context, network, addr string) (net.Conn, error)

	var stealthProf *stealth.Profile
	if cfg.TLSFingerprint != "" {
		profile, ok := stealth.Lookup(cfg.TLSFingerprint)
		if !ok {
			return nil, fmt.Errorf("upstream: unknown TLS_FINGERPRINT %q", cfg.TLSFingerprint)
		}
		stealthProf = profile
	}

	// Direct egress only (no proxy support): this gateway spoofs the
	// official FreeBuff CLI, which has no proxy machinery, and the upstream
	// server hard-blocks proxy/VPN/Tor egress. The DefaultTransport clone
	// inherits http.ProxyFromEnvironment; disable it so an operator
	// HTTP_PROXY/HTTPS_PROXY env var never routes upstream traffic through a
	// proxy either (full egress control).
	transport.Proxy = nil

	if stealthProf != nil {
		// Resolve the profile per request (instead of capturing it) so a
		// transient retry can swap the pinned fingerprint without rebuilding
		// the transport: rotateStealthProfileForRetry swaps c.stealthProfile
		// and the next dial picks it up. For auto/random, newRequest resolves
		// a concrete profile and stashes it so the browser headers and the
		// ClientHello always match; dialProfileFor prefers that stash.
		// baseDial is nil on the direct-only path, so the stealth dialer
		// falls back to the default net.Dialer.
		// The ALPN list must match the transport that will speak next: h2
		// when the http2 transport below is registered, h1 otherwise.
		alpn := []string{"http/1.1"}
		if c.http2Upstream {
			alpn = h2ALPN()
		}
		transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return stealth.Dialer(c.dialProfileFor(ctx), baseDial, false, alpn)(ctx, network, addr)
		}
	}

	// HTTP/2 upstream (issue #51). Real browsers advertise "h2,http/1.1";
	// forcing h1-only at the TLS layer is itself a JA4 ALPN mismatch. With
	// the stealth profile the stdlib transport cannot dispatch HTTP/2 over a
	// *utls.UConn (its h2 path type-asserts the conn to *tls.Conn), so a
	// dedicated http2.Transport takes over the "https" scheme and dials with
	// the SAME utls dialer (which now advertises h2).
	//
	// KNOWN LIMITATION (documented): the standard http2 transport writes its
	// own SETTINGS/WINDOW_UPDATE frames (order EnablePush, InitialWindowSize,
	// MaxFrameSize, MaxHeaderListSize, HeaderTableSize) and no priority
	// frames — a real Chrome sends its own ordering plus priorities. The
	// values below approximate Chrome's SETTINGS (HEADER_TABLE_SIZE 65536,
	// INITIAL_WINDOW_SIZE 6291456, MAX_HEADER_LIST_SIZE 262144 per
	// reference/tls-client profiles), killing the JA4 ALPN mismatch; exact
	// per-profile SETTINGS-frame fingerprinting is not feasible with the
	// stdlib transport.
	//
	// HTTP2_UPSTREAM=false restores the previous h1-only behavior.
	if c.http2Upstream {
		if stealthProf != nil {
			h2t := &http2.Transport{
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					return stealth.Dialer(c.dialProfileFor(ctx), baseDial, false, h2ALPN())(ctx, network, addr)
				},
				MaxDecoderHeaderTableSize: 65536,   // Chrome SETTINGS_HEADER_TABLE_SIZE
				MaxHeaderListSize:         262_144, // Chrome SETTINGS_MAX_HEADER_LIST_SIZE
			}
			transport.RegisterProtocol("https", h2t)
		} else {
			// Plain Go transport: the stdlib already negotiates HTTP/2 by
			// default (the DefaultTransport clone carries
			// ForceAttemptHTTP2=true, and its bundled h2 transport handles
			// the ALPN dispatch because the TLS handshake is the stdlib's
			// own). HTTP2_UPSTREAM=false forces HTTP/1.1 instead — an empty
			// TLSNextProto map is the documented way to disable h2.
			if !c.http2Upstream {
				transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
			}
		}
	} else if stealthProf == nil {
		// HTTP2_UPSTREAM=false on the plain path: force HTTP/1.1 (the
		// stdlib would otherwise negotiate h2).
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	c.stealthProfile = stealthProf
	c.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			// Go strips Authorization/Cookie on cross-host redirects but not
			// x-codebuff-api-key, which carried the same raw token (defensive —
			// newRequest no longer sets it, #107). Drop both when the redirect
			// target is a different host OR downgrades the scheme https->http
			// (same host, plaintext) so the token never leaks to a redirect
			// target; same-scheme same-host redirects (e.g. CDN or bare-host
			// -> www) keep their credentials.
			if !strings.EqualFold(via[0].URL.Host, req.URL.Host) ||
				(strings.EqualFold(via[0].URL.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http")) {
				req.Header.Del("Authorization")
				req.Header.Del("x-codebuff-api-key")
			}
			return nil
		},
	}
	return c, nil
}

// reqIDKey carries the request correlation id (opts.RequestID) through the
// request context for the do()/retry log lines. The key type is unexported;
// the server threads the same id via ChatOptions.RequestID (its own
// unexported server-side key is separate).
type reqIDKey struct{}

// withReqID returns a context carrying the request correlation id.
func withReqID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey{}, id)
}

// ReqID returns the request correlation id carried in ctx, or "" when the
// call was not made through ChatCompletions with opts.RequestID set (e.g.
// session/run management calls).
func ReqID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

// requestProfileKey stashes the concrete stealth profile resolved for one
// request in its context, so the transport dialer builds the ClientHello
// from the SAME profile whose browser headers were applied (auto/random
// must not draw twice — headers and TLS fingerprint would mismatch).
type requestProfileKey struct{}

func withStealthProfile(ctx context.Context, p *stealth.Profile) context.Context {
	return context.WithValue(ctx, requestProfileKey{}, p)
}

func stealthProfileFrom(ctx context.Context) *stealth.Profile {
	if p, ok := ctx.Value(requestProfileKey{}).(*stealth.Profile); ok {
		return p
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, fmt.Errorf("upstream: build %s %s: %w", method, path, err)
	}
	// A bodyless POST/PUT/PATCH is trivially replayable on a transient
	// retry: give it a NoBody GetBody so do()'s TRANSIENT_RETRIES replay
	// works (a nil GetBody silently disables retries, which after #120
	// would break the bodyless session POST's transport-level retry). GETs
	// and DELETEs stay nil-GetBody (never retried — idempotent reads fail
	// fast and the poll loop's own backoff owns them).
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		if body == nil {
			req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if c.authOnly {
		// Token-less login-flow client (#62/#66): never send an empty
		// credential pair — the /api/auth/cli/* endpoints take the login
		// User-Agent instead (see authLoginRequest).
		req.Header.Del("Authorization")
	}
	// Content-Type only when a body is present (#120): the CLI sets it iff
	// body !== undefined (reference/freebuff codebuff-api.ts:344-346), so a
	// bodyless session POST must not carry it. Chat always has a body.
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// The official CLI sends the pinned llm-providers ai-sdk UA on chat and
	// NO browser headers on any API path (bare Bun fetch) (#108/#109 fix
	// option (a)): the utls ClientHello impersonation stays, the browser
	// header persona does not. x-codebuff-api-key is never sent — Bearer is
	// the only credential (#107, reference/freebuff codebuff-api.ts:337-345).
	req.Header.Set("User-Agent", cliUserAgent)
	ctx = req.Context()
	if profile := c.currentStealthProfile(); profile != nil {
		// Resolve the concrete profile ONCE per request and stash it: the
		// dialer reads the stash for the ClientHello, so the TLS fingerprint
		// matches the profile. Pinned profiles resolve to themselves;
		// auto/random get one concrete draw. Only SanitizeHeaders runs here
		// (protective strip of proxy-identifying headers) — the profile's
		// browser headers are deliberately NOT applied to upstream API calls.
		connProf := stealth.GetProfileForConnection(profile)
		ctx = withStealthProfile(ctx, connProf)
		stealth.SanitizeHeaders(req.Header)
	}
	if ctx != req.Context() {
		req = req.WithContext(ctx)
	}
	return req, nil
}

// do executes req, enforcing the given timeout unless ctx already carries an
// earlier deadline. The returned cancel must be released once the caller is
// done with the response BODY: canceling the request context aborts in-flight
// body reads, so it must outlive body streaming. cancel is nil when no
// timeout was applied. Failures are wrapped so errors.Is works both ways.
//
// When TRANSIENT_RETRIES > 0, transport-level failures (dial/TLS handshake/
// reset/EOF) are retried up to that many additional attempts: the body is
// replayed from GetBody on a fresh connection (req.Close), the pinned TLS
// fingerprint is rotated, and a randomized 200-600ms backoff precedes each
// retry. Classified upstream errors (429/403/401, session/run invalids,
// waiting room), any HTTP status >= 400, context cancellation, and requests
// whose body cannot be replayed are NEVER retried.
func (c *Client) do(req *http.Request, timeout time.Duration) (*http.Response, context.CancelFunc, error) {
	ctx := req.Context()
	start := time.Now()
	var cancel context.CancelFunc
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		// The caller already bound the request. The control-call timeout is
		// still honored as an upper bound when it is the TIGHTER of the two:
		// a long caller deadline (e.g. a 15m request timeout) must not
		// silently defeat SessionCallTimeout on session/run control calls.
		if timeout > 0 {
			if remaining := time.Until(deadline); timeout < remaining {
				ctx, cancel = context.WithTimeout(ctx, timeout)
				req = req.WithContext(ctx)
			}
		}
	} else if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		req = req.WithContext(ctx)
	}

	// Capture the body so a transient failure can replay an identical
	// request. nil bodies (GETs) and non-replayable bodies never retry.
	var replayBody func() (io.ReadCloser, error)
	if req.GetBody != nil {
		replayBody = req.GetBody
	}

	for attempt := 1; ; attempt++ {
		resp, err := c.http.Do(req)
		if err == nil {
			if werr := wrapDecompress(resp); werr != nil {
				_ = resp.Body.Close()
				if cancel != nil {
					cancel()
				}
				return nil, nil, fmt.Errorf("upstream: %s %s: %w", req.Method, req.URL.Path, werr)
			}
			if resp.StatusCode >= 400 {
				// T5 wire transparency: error responses are read (2KB cap),
				// logged as `upstream response` (redacted, ≤500 runes), and
				// re-wrapped so the caller's classification parses the same
				// body. Never logged as `upstream ok` — a transport-level
				// 200 and an upstream 429 are different classes of event.
				bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyRead))
				_ = resp.Body.Close()
				bodyText := telemetry.RedactSecrets(string(bodyBytes))
				class := errClassName(classifyError(resp.StatusCode, bodyText, resp.Header))
				attrs := []any{
					"method", req.Method, "path", req.URL.Path,
					"status", resp.StatusCode, "ms", time.Since(start).Milliseconds(),
					"class", class,
					"body", truncateRunes(bodyText, 500),
				}
				if reqID := ReqID(ctx); reqID != "" {
					attrs = append(attrs, "req_id", reqID)
				}
				slog.Debug("upstream response", attrs...)
				resp.Body = io.NopCloser(strings.NewReader(bodyText))
				return resp, cancel, nil
			}
			slog.Debug("upstream ok", "method", req.Method, "path", req.URL.Path,
				"status", resp.StatusCode, "ms", time.Since(start).Milliseconds(),
				"req_id", ReqID(ctx))
			return resp, cancel, nil
		}

		// Transient transport failure with attempts remaining: rotate the
		// pinned fingerprint, replay the body on a fresh connection, and
		// retry after a jittered backoff.
		if c.transientRetriesLimit > 0 && attempt <= c.transientRetriesLimit &&
			ctx.Err() == nil && replayBody != nil && isTransient(err) {
			c.rotateStealthProfileForRetry(req)
			body, bodyErr := replayBody()
			if bodyErr != nil {
				slog.Debug("upstream retry aborted: body replay failed",
					"token", c.tokenIndex+1, "attempt", attempt, "err", bodyErr,
					"req_id", ReqID(ctx))
			} else {
				// Count the retry only once the replay succeeded: the counter
				// reflects retries that actually fired, not aborted ones.
				c.transientRetries.Add(1)
				req.Body = body
				req.Close = true // fresh connection for the retry
				slog.Debug("upstream transient failure, retrying",
					"token", c.tokenIndex+1, "attempt", attempt, "reason", err.Error(),
					"path", req.URL.Path, "req_id", ReqID(ctx))
				timer := time.NewTimer(c.retryDelay())
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
				}
				if ctx.Err() == nil {
					continue
				}
				// Context died during the backoff: a retry would fail
				// instantly, surface the context error instead.
				err = ctx.Err()
			}
		}

		slog.Debug("upstream error", "method", req.Method, "path", req.URL.Path,
			"ms", time.Since(start).Milliseconds(), "err", err, "req_id", ReqID(ctx))
		if cancel != nil {
			cancel()
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, nil, context.Canceled
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("%w: %s %s", context.DeadlineExceeded, req.Method, req.URL.Path)
		}
		return nil, nil, fmt.Errorf("upstream: %s %s: %w", req.Method, req.URL.Path, err)
	}
}

// BaseURL returns the upstream base URL this client dials (used by the pool
// to build probe clients with the same upstream the pooled tokens use).
func (c *Client) BaseURL() string { return c.baseURL }

// currentStealthProfile returns the active stealth profile (nil = plain Go
// transport). Guarded by profileMu: the retry loop swaps the pinned profile
// to rotate the fingerprint, so readers must take the lock.
func (c *Client) currentStealthProfile() *stealth.Profile {
	c.profileMu.Lock()
	defer c.profileMu.Unlock()
	return c.stealthProfile
}

// dialProfileFor returns the stealth profile the transport dialer should use
// for a connection under ctx. For ProfileAuto/ProfileRandom the concrete
// profile stashed by newRequest wins, so the ClientHello matches the profile
// resolved for that request; a bare context (no stash) resolves per
// connection as before. For pinned profiles the current c.stealthProfile is
// authoritative: the retry loop swaps it ahead of a retry, so the stash
// would be stale.
func (c *Client) dialProfileFor(ctx context.Context) *stealth.Profile {
	profile := c.currentStealthProfile()
	if profile != nil && (profile.ID == stealth.ProfileIDAuto || profile.ID == stealth.ProfileIDRandom) {
		if stashed := stealthProfileFrom(ctx); stashed != nil {
			return stashed
		}
	}
	return profile
}

// h2ALPN returns the ALPN list a real browser advertises — the JA4-correct
// fingerprint for HTTP/2 upstreams (#51).
var _h2ALPN = [2]string{"h2", "http/1.1"}

func h2ALPN() []string { return _h2ALPN[:] }

// TransientRetries returns how many transient transport failures were
// retried by this client (pool snapshot /metrics aggregation).
func (c *Client) TransientRetries() int64 { return c.transientRetries.Load() }

// CapacityDeferredRetries returns how many free_mode_capacity_deferred
// retries this client served (same-session retries under the
// TRANSIENT_RETRIES budget, issue #75).
func (c *Client) CapacityDeferredRetries() int64 { return c.capacityDeferredRetries.Load() }

// PendingWaitingRoomChain reports whether the client last classified a 428
// waiting_room_required (issue #94) and the pre-session chain has not been
// fired/cleared yet. The pool consults it before a session create when
// WAITING_ROOM_CHAIN is enabled.
func (c *Client) PendingWaitingRoomChain() bool { return c.waitingRoomRequired.Load() }

// ConsumeWaitingRoomChain clears the 428 flag and reports whether it was
// set (so the caller fires the chain exactly once per 428).
func (c *Client) ConsumeWaitingRoomChain() bool { return c.waitingRoomRequired.Swap(false) }

// FingerprintRotations returns how many times the pinned TLS fingerprint was
// rotated ahead of a retry (pool snapshot /metrics aggregation).
func (c *Client) FingerprintRotations() int64 { return c.fingerprintRotations.Load() }

// SetTransport replaces the HTTP transport backing the client. Exported as a
// test seam for retry-injection tests (substituting a flaky RoundTripper);
// production code never calls it.
func (c *Client) SetTransport(rt http.RoundTripper) { c.http.Transport = rt }

// transientMarkers are transport-level failure signatures that are safe to
// retry: the request never reached the application layer, so no upstream
// quota/credits were burned and nothing was processed. Classified upstream
// errors (429/403/401, session/run invalids, waiting room) and any HTTP
// status >= 400 are handled at the response layer and never enter this path.
// Markers are lowercase: isTransient lowercases the wrapped error messages
// before matching. "tls: handshake failure" is Go's own alert string;
// "tls handshake failed" appears in wrapper libraries (e.g. stealth/uTLS).
var transientMarkers = []string{
	"tls handshake failed",
	"tls: handshake failure",
	"tls: internal error",
	"connection refused",
	"connection reset",
	"unexpected eof",
	"network is unreachable",
	"no route to host",
	"i/o timeout", // dial timeout
}

// isTransient reports whether err is a transient transport failure safe to
// retry. It walks the wrapped error chain and matches message fragments, so
// stealth-wrapped dial errors ("stealth: tcp dial failed: ...: connection
// refused") classify the same as the bare dial error.
//
// Bare "EOF" is matched on exact whole-message equality only: a substring
// match on "eof" would over-retry unrelated errors that merely mention the
// letters ("... eof marker ...").
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		msg := strings.ToLower(cur.Error())
		for _, marker := range transientMarkers {
			if strings.Contains(msg, marker) {
				return true
			}
		}
		if msg == "eof" {
			return true
		}
	}
	return false
}

// retryProfileRotation is the pinned-profile rotation order for transient
// retries: one entry per distinct ClientHelloID, so a retry presents a
// genuinely different JA3 (rotating chrome120 -> chrome126 would change only
// headers, not the TLS fingerprint). ProfileRandom/ProfileAuto are excluded:
// they already resolve a fresh fingerprint per connection.
var retryProfileRotation = []struct {
	ids  []stealth.ProfileID
	next *stealth.Profile
}{
	{ids: []stealth.ProfileID{stealth.ProfileIDChrome120, stealth.ProfileIDChrome126, stealth.ProfileIDEdge126}, next: stealth.ProfileSafari18},
	{ids: []stealth.ProfileID{stealth.ProfileIDSafari17, stealth.ProfileIDSafari18}, next: stealth.ProfileFirefox128},
	{ids: []stealth.ProfileID{stealth.ProfileIDFirefox120, stealth.ProfileIDFirefox128}, next: stealth.ProfileChrome126},
}

// rotateStealthProfileForRetry swaps the pinned TLS fingerprint to a
// different profile before a retry so the retried connection does not repeat
// the fingerprint that just failed. The request keeps its CLI headers —
// only proxy-identifying headers are re-stripped; no browser persona is
// applied on API paths (#109). random/auto already rotate per connection and
// are left alone. No-op when retries are disabled or no fingerprint is
// pinned.
func (c *Client) rotateStealthProfileForRetry(req *http.Request) {
	c.profileMu.Lock()
	defer c.profileMu.Unlock()
	if c.transientRetriesLimit <= 0 || c.stealthProfile == nil {
		return
	}
	id := c.stealthProfile.ID
	if id == stealth.ProfileIDRandom || id == stealth.ProfileIDAuto {
		return
	}
	next := nextStealthProfile(c.stealthProfile)
	if next.ID == id {
		return
	}
	c.stealthProfile = next
	c.fingerprintRotations.Add(1)
	stealth.SanitizeHeaders(req.Header)
}

// nextStealthProfile returns the profile to rotate to after cur: the next
// entry in the fixed rotation order whose ClientHelloID differs from cur's.
func nextStealthProfile(cur *stealth.Profile) *stealth.Profile {
	for _, entry := range retryProfileRotation {
		for _, id := range entry.ids {
			if id == cur.ID {
				return entry.next
			}
		}
	}
	return retryProfileRotation[0].next
}

// retryDelay returns the sleep before a transient retry: a randomized
// 200-600ms backoff using crypto/rand (matching the request-jitter pattern).
// Tests pin it via Client.retryBackoff.
func (c *Client) retryDelay() time.Duration {
	if c.retryBackoff != nil {
		return c.retryBackoff()
	}
	var b [8]byte
	_, _ = cryptoRand.Read(b[:])
	u := binary.BigEndian.Uint64(b[:])
	return 200*time.Millisecond + time.Duration(u%uint64(400*time.Millisecond))
}

// wrapDecompress replaces resp.Body with a transparent decompressing reader
// when the upstream compresses the response. This is REQUIRED with the
// stealth profile: the browser Accept-Encoding ("gzip, deflate, br") makes
// Go's transport skip its automatic gzip handling (that only kicks in when
// Go itself set the header), so compressed bodies would arrive as garbage.
// The plain transport sends no Accept-Encoding and is unaffected (Go
// decompresses its own gzip transparently and strips the header).
func wrapDecompress(resp *http.Response) error {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return nil
	}
	underlying := resp.Body
	switch enc {
	case "gzip":
		zr, err := gzip.NewReader(underlying)
		if err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
	case "deflate":
		// RFC 9110 §8.4.1.3 defines Content-Encoding: deflate as a
		// zlib-wrapped stream (RFC 1950), but some servers historically
		// send raw DEFLATE (RFC 1951). Sniff the zlib header (CMF/FLG:
		// CM=8, CINFO<=7, 16-bit header a multiple of 31) WITHOUT
		// consuming bytes — a consumed header would corrupt the raw
		// fallback — and decode accordingly. (Audit B1: the raw-only
		// reader broke mid-stream on conforming zlib responses.)
		br := bufio.NewReader(underlying)
		head, _ := br.Peek(2)
		if len(head) == 2 && head[0]&0x0f == 8 && head[0]>>4 <= 7 &&
			(uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return fmt.Errorf("deflate: %w", err)
			}
			resp.Body = &decompressCloser{Reader: zr, underlying: underlying}
		} else {
			resp.Body = &decompressCloser{Reader: flate.NewReader(br), underlying: underlying}
		}
	case "br":
		resp.Body = &decompressCloser{Reader: brotli.NewReader(underlying), underlying: underlying}
	case "zstd":
		// The stealth profiles advertise zstd in Accept-Encoding, so the
		// upstream may legitimately respond with it.
		zr, err := zstd.NewReader(underlying, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return fmt.Errorf("zstd: %w", err)
		}
		// zstd decoders are stateful (per-response buffers), unlike
		// gzip/brotli: Close must release the decoder's resources, not just
		// the underlying socket. (Audit B9.)
		resp.Body = &decompressCloser{Reader: zr, underlying: underlying, closeFn: func() error { zr.Close(); return nil }}
	default:
		return fmt.Errorf("unsupported Content-Encoding %q", enc)
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return nil
}

// decompressCloser bridges a decompressing reader back to the underlying
// response body so Close always reaches the socket. closeFn optionally
// releases decoder-local resources (e.g. a zstd decoder's buffers) that are
// distinct from the underlying stream.
type decompressCloser struct {
	io.Reader
	underlying io.ReadCloser
	closeFn    func() error
}

func (d *decompressCloser) Close() error {
	if d.closeFn != nil {
		_ = d.closeFn()
	}
	return d.underlying.Close()
}

// releaseCancel cancels a do() timeout context unless it is nil.
func releaseCancel(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
}

// cancelBody closes the underlying body and then releases the request
// context, so a streamed response body lives exactly as long as its reader.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	releaseCancel(b.cancel)
	return err
}

func getNumber(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch n := v.(type) {
			case float64:
				return n, true
			case int:
				return float64(n), true
			case int64:
				return float64(n), true
			case string:
				if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
					return f, true
				}
			}
		}
	}
	return 0, false
}

func getTime(m map[string]any, keys ...string) (time.Time, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			switch val := v.(type) {
			case string:
				val = strings.TrimSpace(val)
				if t, err := time.Parse(time.RFC3339Nano, val); err == nil {
					return t, true
				}
				if t, err := time.Parse(time.RFC3339, val); err == nil {
					return t, true
				}
			case float64:
				if val > 1e11 { // milliseconds
					return time.UnixMilli(int64(val)).UTC(), true
				} else if val > 0 {
					return time.Unix(int64(val), 0).UTC(), true
				}
			}
		}
	}
	return time.Time{}, false
}

func retryDetail(retryAfter time.Duration) string {
	if retryAfter > 0 {
		return fmt.Sprintf(" (Retry-After %s)", retryAfter)
	}
	return ""
}

func containsAny(lower string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func unixFrom(secs int64) time.Time {
	// Heuristic: milliseconds if 10^12 or larger, else seconds.
	if secs >= 100_000_000_000 {
		return time.Unix(0, secs*int64(time.Millisecond))
	}
	return time.Unix(secs, 0)
}

// generateClientID mints the SDK-faithful 13-char base36 client id
// (Math.random().toString(36).substring(2, 15)).
func generateClientID() string {
	var b [16]byte
	if _, err := cryptoRand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable in practice; fall back to a
		// time-seeded value rather than panicking mid-request. UnixNano in
		// base36 is only 12 digits today, so pad to the SDK's 13-char length
		// (the old [:13] slice panicked on short values).
		return padBase36(strconv.FormatInt(time.Now().UnixNano(), 36))
	}
	n := new(big.Int).SetBytes(b[:])
	mod := new(big.Int).Exp(big.NewInt(36), big.NewInt(13), nil)
	return padBase36(n.Mod(n, mod).Text(36))
}

// padBase36 left-pads a base36 string with '0' to the SDK-faithful 13-char
// client id length. Both the crypto/rand draw and the time-seeded fallback
// need it: the latter is 12 digits, which would otherwise come out shorter
// than the JS substring(2, 15) equivalent.
func padBase36(id string) string {
	for len(id) < 13 {
		id = "0" + id
	}
	return id
}

func drainBody(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, maxDumpRead))
	return string(data)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// truncateRunes truncates s to at most max runes without an ellipsis. The
// CLI's FINISH errorMessage cap is 5000 chars (truncateString in
// reference/freebuff/sdk/src/impl/database.ts), applied on the whole
// payload — a full Go stack trace must not blow the cap.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// dump writes a debug record to dump/ when enabled.
func (c *Client) dump(kind string, req *http.Request, status int, body string) {
	if !c.debugDump {
		return
	}
	name := fmt.Sprintf("%s-%d-%s.dump", kind, time.Now().UnixNano(), sanitizeName(req.URL.Path))
	path := filepath.Join("dump", name)
	_ = os.MkdirAll("dump", 0o755)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %s\n", req.Method, req.URL.String())
	for k, vs := range req.Header {
		for _, v := range vs {
			if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "x-codebuff-api-key") {
				v = "[redacted]"
			}
			fmt.Fprintf(&buf, "%s: %s\n", k, v)
		}
	}
	fmt.Fprintf(&buf, "\n[status %d]\n%s\n", status, truncate(body, 20000))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		// T18: the write was previously swallowed (`_ = os.WriteFile`) —
		// surface the failure so a broken dump dir is not silent.
		slog.Warn("debug dump write failed", "path", path, "err", err)
	}
}

func sanitizeName(p string) string {
	p = strings.ReplaceAll(p, "/", "_")
	p = strings.ReplaceAll(p, ".", "_")
	if len(p) > 60 {
		p = p[:60]
	}
	return p
}
