// Package stealth provides JA3 TLS fingerprint impersonation and browser
// header sanitization. It makes upstream connections indistinguishable from
// real browsers at the TLS layer by using utls ClientHello presets.
package stealth

import (
	"math/rand"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ProfileID uniquely identifies a browser fingerprint profile.
type ProfileID string

const (
	ProfileIDChrome120  ProfileID = "chrome120"
	ProfileIDSafari17   ProfileID = "safari17"
	ProfileIDFirefox120 ProfileID = "firefox120"
)

// Profile defines a complete browser TLS fingerprint including the utls
// ClientHelloID and matching HTTP headers.
type Profile struct {
	ID              ProfileID
	ClientHelloID   utls.ClientHelloID
	CustomSpec      *utls.ClientHelloSpec
	UserAgent       string
	SecChUA         string
	SecChUAPlatform string
	AcceptLanguage  string
	AcceptEncoding  string
}

// Pre-built browser profiles.
var (
	// ProfileChrome120 mimics Chrome 120 on Windows.
	ProfileChrome120 = &Profile{
		ID:              ProfileIDChrome120,
		ClientHelloID:   utls.HelloChrome_120,
		UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		SecChUA:         `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`,
		SecChUAPlatform: `"Windows"`,
		AcceptLanguage:  "en-US,en;q=0.9",
		AcceptEncoding:  "gzip, deflate, br",
	}

	// ProfileSafari17 mimics Safari 17 on macOS. Uses HelloCustom with a
	// precise ClientHelloSpec since utls has no dedicated Safari preset.
	ProfileSafari17 = &Profile{
		ID:             ProfileIDSafari17,
		ClientHelloID:  utls.HelloCustom,
		CustomSpec:     safari17Spec(),
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
	}

	// ProfileFirefox120 mimics Firefox 120 on Linux.
	ProfileFirefox120 = &Profile{
		ID:             ProfileIDFirefox120,
		ClientHelloID:  utls.HelloFirefox_120,
		UserAgent:      "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0",
		AcceptLanguage: "en-US,en;q=0.5",
		AcceptEncoding: "gzip, deflate, br",
	}

	// ProfileRandom picks a random fingerprint on each connection.
	ProfileRandom = &Profile{
		ID:             "random",
		ClientHelloID:  utls.HelloRandomized,
		UserAgent:      randomUserAgent(),
		AcceptLanguage: "en-US,en;q=0.9",
		AcceptEncoding: "gzip, deflate, br",
	}
)

// DefaultProfile returns Chrome 120 as the default profile.
func DefaultProfile() *Profile { return ProfileChrome120 }

// Lookup returns the profile matching the given name (case-insensitive)
// and true, or nil, false for unknown names.
func Lookup(name string) (*Profile, bool) {
	switch strings.ToLower(name) {
	case "chrome120":
		return ProfileChrome120, true
	case "safari17":
		return ProfileSafari17, true
	case "firefox120":
		return ProfileFirefox120, true
	case "random":
		return ProfileRandom, true
	default:
		return nil, false
	}
}

// randomUserAgent generates a randomized browser User-Agent by picking a
// known browser version string.
func randomUserAgent() string {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	agents := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:120.0) Gecko/20100101 Firefox/120.0",
		"Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15",
	}
	return agents[rng.Intn(len(agents))]
}

// safari17Spec returns the custom ClientHelloSpec for Safari 17 on macOS.
// Ported exactly from the reference implementation.
func safari17Spec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,
		},
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519,
				utls.CurveP256,
				utls.CurveP384,
				utls.CurveP521,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				utls.ECDSAWithSHA1,
				utls.PSSWithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA384,
				utls.PKCS1WithSHA512,
				utls.PKCS1WithSHA1,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519},
			}},
			&utls.SupportedVersionsExtension{Versions: []uint16{
				utls.GREASE_PLACEHOLDER,
				utls.VersionTLS13,
				utls.VersionTLS12,
			}},
			&utls.UtlsGREASEExtension{},
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}
}
