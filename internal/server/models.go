package server

import (
	"encoding/json"
	"net/http"

	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/session"
)

// probeModel returns the safest model to default a smoke test to: the
// fallback default (deepseek-v4-flash — the model every account gets, incl.
// limited tier) when it is in the catalog, else the first catalog model.
// Alphabetical models[0] would otherwise pick anthropic/claude-fable-5, a
// capacity-gated offer model that makes smoke tests fail on most accounts.
func probeModel(reg *registry.Registry) string {
	models := reg.Models()
	if len(models) == 0 {
		return ""
	}
	for _, id := range models {
		if id == session.DefaultFallbackModel {
			return id
		}
	}
	return models[0]
}

// modelAllowed reports whether a model may be served. Every model must first
// pass the hardcoded SmartToyModels gate (9router smart_toy component); then,
// when MODELS_ALLOW is non-empty, the RESOLVED model id (after registry alias
// resolution and -max upgrades) must be listed exactly — OR, when
// PREFER_MAX_MODELS is enabled, the resolved id may be the -max variant of
// an allowlisted base model.
func (s *Server) modelAllowed(model string) bool {
	if !registry.SmartToyModels[model] {
		return false
	}
	cfg := s.cfg.Load()
	allow := cfg.ModelsAllow
	if len(allow) == 0 {
		return true
	}
	for _, id := range allow {
		if id == model {
			return true
		}
		if cfg.PreferMaxModels {
			if upgraded, ok := registry.MaxVariantOf(id); ok && upgraded == model {
				return true
			}
		}
	}
	return false
}

// modelListed is the strict listing filter for /v1/models: like modelAllowed
// it gates on SmartToyModels first; unlike modelAllowed it does NOT expand
// base ids to their -max variants, so PREFER_MAX_MODELS keeps the catalog
// surface exactly the MODELS_ALLOW list (clients request the base id; the
// proxy serves the extended-context variant invisibly).
func (s *Server) modelListed(model string) bool {
	if !registry.SmartToyModels[model] {
		return false
	}
	allow := s.cfg.Load().ModelsAllow
	if len(allow) == 0 {
		return true
	}
	for _, id := range allow {
		if id == model {
			return true
		}
	}
	return false
}

// handleModels serves the OpenAI model-list shape with the registry's
// current models; created is pinned to server start so every entry matches.
// Each row carries an advisory availability annotation derived from the pool
// token snapshots (available/status/current_access_tier) so clients can
// surface quota or lock signals without probing.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	created := s.started.Unix()
	snaps := s.pool.Snapshot()
	models := s.reg.Models()
	if len(models) == 0 {
		// T16: an empty registry is an operational anomaly (the fallback
		// table should always populate at boot) — surface it when a client
		// actually asks, not at startup.
		s.logger.Warn("model list requested with empty registry", "path", r.URL.Path, "remote", remoteHost(r), "model_count", 0)
	}
	hideUnavailable := s.cfg.Load().ModelsHideUnavailable
	data := make([]map[string]any, 0, len(models))
	for _, id := range models {
		available, status, tier := modelAvailability(id, snaps)
		if hideUnavailable && !available {
			// MODELS_HIDE_UNAVAILABLE=true: prune region/tier/quota-locked
			// models so picker clients never auto-select one. Off by default
			// because a stale signal could hide a working model.
			continue
		}
		if !s.modelListed(id) {
			// MODELS_ALLOW: prune ids outside the operator allowlist so
			// picker clients never auto-select a model that would 404. Uses
			// the strict list (base ids only), so PREFER_MAX_MODELS -max
			// variants stay invisible on the catalog surface.
			continue
		}
		data = append(data, map[string]any{
			"id":                  id,
			"object":              "model",
			"created":             created,
			"owned_by":            "freebuff",
			"available":           available,
			"status":              status,
			"current_access_tier": tier,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// handleModelRetrieve answers GET /v1/models/{model} for clients querying a single model.
func (s *Server) handleModelRetrieve(w http.ResponseWriter, r *http.Request) {
	modelName := r.PathValue("model")
	if modelName == "" {
		s.writeJSONError(w, http.StatusBadRequest, "missing model name in path", "invalid_request_error", "model_not_found", 0)
		return
	}
	model := s.reg.ResolveModel(modelName)
	if !s.modelAllowed(model) {
		s.writeJSONError(w, http.StatusNotFound, "The model '"+modelName+"' does not exist", "invalid_request_error", "model_not_found", 0)
		return
	}
	created := s.started.Unix()
	snaps := s.pool.Snapshot()
	available, status, tier := modelAvailability(model, snaps)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":                  modelName,
		"object":              "model",
		"created":             created,
		"owned_by":            "freebuff",
		"available":           available,
		"status":              status,
		"current_access_tier": tier,
	})
}

// modelAvailability derives the advisory per-model annotation from the pool
// token snapshots. The snapshot does not carry the model of a live session,
// so the signal set is: quotaByModel presence (the session admitted this
// model), quota exhaustion (recent >= limit), session-level locks, and the
// access tier. A token demoted to the 'limited' tier (region/privacy
// demotion) can only use LimitedTierModels — every other model is marked
// unavailable with status "region_limited" (kept in the list, never hidden,
// so a stale tier can't strand a working model). available defaults to true
// when no signal exists, so a working model is never hidden.
func modelAvailability(id string, snaps []pool.TokenSnapshot) (available bool, status, tier string) {
	available = true
	status = "unknown"
	quotaHit := false
	quotaExhausted := false
	locked := false
	for _, snap := range snaps {
		if tier == "" {
			tier = snap.TierAccess
		}
		switch snap.SessionStatus {
		case "model_locked", "disabled":
			locked = true
		}
		if q, ok := snap.QuotaByModel[id]; ok {
			quotaHit = true
			if q.Limit > 0 && q.RecentCount >= q.Limit {
				quotaExhausted = true
			}
		}
	}
	switch {
	case quotaExhausted:
		status = "quota_exhausted"
	case locked:
		status = "locked"
	case quotaHit:
		status = "available"
	}
	if status == "unknown" && tier == "limited" && !registry.LimitedTierModels[id] {
		// Region/privacy demotion: the model is not on the limited tier's
		// allowlist and the session never admitted it. Keep it listed but
		// honest — clients that auto-pick on the available flag skip it,
		// and a stale tier can never hide a model the session admitted
		// (admission is ground truth, handled above).
		return false, "region_limited", tier
	}
	return available, status, tier
}
