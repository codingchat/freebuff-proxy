package dashboard

import (
	"encoding/json"
	"net/http"

	"freebuff-proxy/internal/phasetiming"
)

// RenderLogin renders the login page with an optional error message.
func (d *Dashboard) RenderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	if errMsg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": errMsg})
}

// RenderRestricted renders the access-denied page as JSON.
func (d *Dashboard) RenderRestricted(w http.ResponseWriter, r *http.Request, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg})
}

// RenderConfigResult renders the response after a config save or token action.
func (d *Dashboard) RenderConfigResult(w http.ResponseWriter, r *http.Request, ok bool, message string) {
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": ok, "message": message})
}

// RenderTestResult appends one per-token outcome.
func (d *Dashboard) RenderTestResult(w http.ResponseWriter, r *http.Request, token int, ok bool, message, instanceID string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":       token,
		"ok":          ok,
		"message":     message,
		"instance_id": shortID(instanceID),
	})
}

// PhaseKV is one rendered latency phase.
type PhaseKV struct {
	Name string `json:"name"`
	Ms   int64  `json:"ms"`
}

// PhaseList orders a phase map for rendering.
func PhaseList(phases map[string]int64) []PhaseKV {
	order := []string{
		phasetiming.AcquireMS,
		phasetiming.SessionRefreshMS,
		phasetiming.RunAcquireMS,
		phasetiming.UpstreamTTFBMS,
		phasetiming.TotalMS,
	}
	out := make([]PhaseKV, 0, len(order))
	for _, name := range order {
		if v, ok := phases[name]; ok {
			out = append(out, PhaseKV{Name: name, Ms: v})
		}
	}
	return out
}

// RenderSmokeResult renders the smoke-test outcome.
func (d *Dashboard) RenderSmokeResult(w http.ResponseWriter, r *http.Request, model, token string, ms int64, preview []byte, phases []PhaseKV) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"model":   model,
		"token":   token,
		"ms":      ms,
		"preview": string(preview),
		"phases":  phases,
	})
}

func (d *Dashboard) RenderDiag(w http.ResponseWriter, r *http.Request, checks []DiagCheck) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"checks": checks})
}

type DiagCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Warn    bool   `json:"warn"`
	Message string `json:"message"`
}
