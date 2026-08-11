package stealth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Dialer returns a DialTLSContext function for http.Transport that uses
// utls to impersonate a specific browser's TLS fingerprint (JA3).
//
// By sending a ClientHello matching a real browser (Chrome, Safari, Firefox),
// the connection is indistinguishable from a genuine browser session at the
// TLS layer — defeating JA3/JA3S fingerprinting deployed by CDN/WAF
// infrastructure.
//
// baseDial provides the underlying TCP dial (e.g. SOCKS5). When nil, a
// default net.Dialer with 30s timeout is used.
func Dialer(profile *Profile, baseDial func(ctx context.Context, network, addr string) (net.Conn, error), insecureSkipVerify bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if profile == nil {
		profile = DefaultProfile()
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialFN := baseDial
		if dialFN == nil {
			dialFN = (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext
		}

		rawConn, err := dialFN(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("stealth: tcp dial failed: %w", err)
		}

		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("stealth: invalid address %q: %w", addr, err)
		}

		helloID := profile.ClientHelloID

		uConn := utls.UClient(rawConn, &utls.Config{
			ServerName:         host,
			InsecureSkipVerify: insecureSkipVerify,
			MinVersion:         tls.VersionTLS12,
		}, helloID)

		if profile.CustomSpec != nil {
			if err := uConn.ApplyPreset(profile.CustomSpec); err != nil {
				rawConn.Close()
				return nil, fmt.Errorf("stealth: apply custom spec failed: %w", err)
			}
		}

		if err := uConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("stealth: tls handshake failed: %w", err)
		}

		return uConn, nil
	}
}
