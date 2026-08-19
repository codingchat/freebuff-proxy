package server

import (
	"bytes"
	"context"
	"encoding/json"
	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/dashboard"
	"freebuff-proxy/internal/phasetiming"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/upstream"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type smokeRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Token  string `json:"token"` // bridge mode: client token to relay upstream
}

const maxSmokeBytes = 32 << 10

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
	// (second tab) must not both proceed to addTokenPersist —
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
