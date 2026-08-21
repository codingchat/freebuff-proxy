package upstream

// Tests for the Wave-5 egress/stealth features that survive the proxy
// removal: HTTP/2 upstream wiring (#51). Stable-egress pinning and its
// dial-fallback tests were deleted with the outbound-proxy machinery (the
// official CLI has no proxy support and the upstream hard-blocks proxied
// egress). Kept in their own file so concurrent work on client_test.go
// does not collide.

import (
	"net/http"
	"strings"
	"testing"

	"freebuff-proxy/internal/config"
)

// TestHTTP2UpstreamWiring guards the HTTP2_UPSTREAM wiring (issue #51):
// stealth clients register an http2.Transport for the https scheme (dials
// with the same utls dialer advertising the browser ALPN), plain clients
// leave the stdlib h2 default on, and HTTP2_UPSTREAM=false forces h1 on the
// plain path (empty TLSNextProto map — the documented h2 kill switch).
//
// Registration is asserted behaviorally: with the stealth h2 transport
// registered, an https request is dispatched to it BEFORE any stdlib dial,
// so its dial failure carries the "stealth: tcp dial failed" wrapper; the
// h1 paths fail with a plain stdlib dial error. No external network is
// touched — 127.0.0.1:1 refuses instantly.
func TestHTTP2UpstreamWiring(t *testing.T) {
	roundTripErr := func(c *Client) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "https://127.0.0.1:1/", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.http.Transport.RoundTrip(req)
		if err == nil {
			t.Fatal("RoundTrip to a refused port succeeded")
		}
		return err.Error()
	}

	t.Run("plain default off in direct-config tests", func(t *testing.T) {
		// testConfig leaves HTTP2Upstream=false (zero value); production
		// defaults it true via config.Load. The false path must pin h1.
		plain, err := New("tok", testConfig("", nil))
		if err != nil {
			t.Fatal(err)
		}
		tr := plain.http.Transport.(*http.Transport)
		if tr.TLSNextProto == nil || tr.TLSNextProto["h2"] != nil {
			t.Errorf("HTTP2_UPSTREAM=false must disable h2 (empty TLSNextProto map), got %v", tr.TLSNextProto)
		}
		if msg := roundTripErr(plain); strings.Contains(msg, "stealth:") {
			t.Errorf("plain h1 client dial error = %q, want a plain stdlib error", msg)
		}
	})

	t.Run("plain enabled keeps stdlib h2", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) { cfg.HTTP2Upstream = true }))
		if err != nil {
			t.Fatal(err)
		}
		tr := c.http.Transport.(*http.Transport)
		// The stdlib registers h2 lazily on first use; the wiring contract is
		// that we did NOT disable it and that ForceAttemptHTTP2 stays on.
		if tr.TLSNextProto != nil {
			t.Errorf("HTTP2_UPSTREAM=true must leave TLSNextProto nil (stdlib h2 default), got %v", tr.TLSNextProto)
		}
		if !tr.ForceAttemptHTTP2 {
			t.Error("ForceAttemptHTTP2 must stay true for the stdlib h2 path")
		}
		if msg := roundTripErr(c); strings.Contains(msg, "stealth:") {
			t.Errorf("plain h2 client dial error = %q, want a plain stdlib error", msg)
		}
	})

	t.Run("stealth enabled routes https through the h2 transport", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) {
			cfg.HTTP2Upstream = true
			cfg.TLSFingerprint = "chrome126"
		}))
		if err != nil {
			t.Fatal(err)
		}
		// The dial failure must carry the stealth wrapper: proof the https
		// request was dispatched to the registered http2.Transport (whose
		// DialTLSContext is the utls dialer) rather than the h1 transport.
		msg := roundTripErr(c)
		if !strings.Contains(msg, "stealth: tcp dial failed") {
			t.Errorf("stealth h2 dial error = %q, want the stealth wrapper (https dispatched to the utls dialer)", msg)
		}
	})

	t.Run("stealth disabled keeps h1", func(t *testing.T) {
		c, err := New("tok", testConfig("", func(cfg *config.Config) {
			cfg.HTTP2Upstream = false
			cfg.TLSFingerprint = "chrome126"
		}))
		if err != nil {
			t.Fatal(err)
		}
		if c.http2Upstream {
			t.Error("HTTP2_UPSTREAM=false must leave http2Upstream false")
		}
		// The h1 path still dials through the stealth DialTLSContext — the
		// wrapper is expected; the point is that no https h2 transport took
		// over the request (that would need HTTP2_UPSTREAM=true).
		if msg := roundTripErr(c); !strings.Contains(msg, "tcp dial failed") {
			t.Errorf("h1 stealth dial error = %q, want the tcp dial wrapper", msg)
		}
	})
}
