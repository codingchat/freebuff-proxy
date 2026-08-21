package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/dashboard"
	"freebuff-proxy/internal/upstream"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxEnvSize = 64 << 10

func readBounded(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return buf[:got], nil
}

type envUpdate struct {
	Key   string
	Value string
}

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

func updateAuthTokensEnv(tokens []string) ([]byte, error) {
	return updateEnvKeys([]envUpdate{{Key: "AUTH_TOKENS", Value: strings.Join(tokens, ",")}})
}

func restoreEnvFile(old []byte, oldErr error) {
	switch {
	case oldErr == nil:
		_ = writeFileAtomic(".env", old)
	case errors.Is(oldErr, os.ErrNotExist):
		_ = os.Remove(".env")
	}
}

func dialTarget(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, "443")
}

func (s *Server) handleDiag(w http.ResponseWriter, r *http.Request) {
	checks := []dashboard.DiagCheck{}

	cfg := s.cfg.Load()
	switch cfg.EffectiveMode() {
	case "bridge":
		checks = append(checks, dashboard.DiagCheck{OK: true, Message: "Configuration: bridge mode (clients relay their own token)"})
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

	// Per-token validity probes (pooled mode only). Each
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
	if keys := envOverrideKeys(content); len(keys) > 0 {
		// The file was written and the config reloaded, but a real process
		// environment variable outranks the .env file (precedence
		// env > .env > JSON), so those values are in force from the
		// environment, NOT from the saved file. Report fail-loud but keep
		// the 200 status: the write itself succeeded and rolling back could
		// not beat the environment (matching the token-path semantics).
		message := fmt.Sprintf("saved, but these keys are overridden by the process environment and will only apply after restart: %s",
			strings.Join(keys, ", "))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "message": message})
		return
	}
	s.dash.RenderConfigResult(w, r, true, "Saved and reloaded — effective configuration updated.")
}

// parseEnvContent is a lenient dotenv parser: the dashboard's textarea posts
// raw .env text, and we re-parse the SAME bytes just written to report which
// keys the process environment silently overrides. BOM, blank lines,
// comments, single/double quotes, and inline # comments are handled; export
// prefixes and interpolated vars (foo=$bar) are intentionally not resolved —
// a literal-of-a-variable is not an override worth reporting.
func parseEnvContent(data []byte) map[string]string {
	out := make(map[string]string)
	// Strip a leading UTF-8 BOM so the first key is not corrupted.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		quoted := false
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
			if end := strings.IndexByte(value[1:], value[0]); end >= 0 {
				value = value[1 : 1+end]
				quoted = true
			}
		}
		if !quoted {
			if idx := strings.IndexByte(value, '#'); idx >= 0 {
				value = strings.TrimSpace(value[:idx])
			}
		}
		out[key] = value
	}
	return out
}

// envOverrideKeys reports which keys in just-written .env content are
// silently overridden by the actual process environment (precedence
// env > .env > JSON). A key counts as overridden only when the environment
// value differs from the written value — or, for AUTH_TOKENS, simply when a
// real env var exists at all (presence wins regardless of emptiness under
// the config loader).
func envOverrideKeys(content []byte) []string {
	written := parseEnvContent(content)
	keys := make([]string, 0, len(written))
	for key, writtenVal := range written {
		envVal, ok := os.LookupEnv(key)
		if !ok {
			continue
		}
		envVal = strings.TrimSpace(envVal)
		writtenVal = strings.TrimSpace(writtenVal)
		switch {
		case key == "AUTH_TOKENS":
			// Presence wins even with an empty value; equality means the
			// written list landed and there is nothing to report.
			if envVal != writtenVal {
				keys = append(keys, key)
			}
		case envVal != "" && envVal != writtenVal:
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

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
		"MODEL_UNAVAILABLE_CACHE_TTL":           cfg.ModelUnavailableCacheTTL.String(),
		"WEBHOOK_URL":                           boolWord(cfg.WebhookURL != ""),
		"FALLBACK_AFTER_MS":                     cfg.FallbackAfter.String(),
		"FALLBACK_MODEL":                        strconv.Itoa(len(cfg.FallbackModels)),
		"ADOPT_CLI_SESSION":                     strconv.FormatBool(cfg.AdoptCLISession),
		"WAITING_ROOM_CHAIN":                    strconv.FormatBool(cfg.WaitingRoomChain),
		"SCARCE_SESSION_MODELS":                 strings.Join(cfg.ScarceSessionModels, ","),
		"QUOTA_FALLBACK_MODELS":                 strconv.Itoa(len(cfg.QuotaFallbackModels)),
	}
}

func boolWord(v bool) string {
	if v {
		return "set"
	}
	return "unset"
}

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
	// Rename-over-existing fails transiently on Windows when the target
	// has a briefly-open handle (antivirus scanning the file we just
	// wrote). Without retries, a transient lock turns into a rejected
	// dashboard save or a lost token add (rollback after a failed .env
	// write). The remove-then-rename fallback stays, but as a last
	// resort after the plain rename has had a chance to clear the lock.
	const renameAttempts = 5
	var lastErr error
	for i := range renameAttempts {
		// NOTE: capture the rename error inside the if — the statement-scoped
		// err shadows the function-level err (from CreateTemp), so assigning
		// it outside the if would store nil.
		if err := os.Rename(tmpName, path); err != nil {
			lastErr = err
		} else {
			return nil
		}
		if _, statErr := os.Stat(path); statErr == nil {
			// The target exists but rename-over-existing failed: remove it
			// first, then rename. The next loop iteration re-tries the
			// plain rename if the fallback itself was transiently locked.
			if os.Remove(path) == nil {
				if err := os.Rename(tmpName, path); err != nil {
					lastErr = err
				} else {
					return nil
				}
			}
		}
		time.Sleep(time.Duration(i+1) * 20 * time.Millisecond)
	}
	// The temp file may itself be transiently locked (antivirus scan of a
	// file we just wrote); retry its removal so a failed write cannot leak
	// a .env.tmp* file into the directory.
	for range 3 {
		if os.Remove(tmpName) == nil {
			return lastErr
		}
		time.Sleep(20 * time.Millisecond)
	}
	return lastErr
}
