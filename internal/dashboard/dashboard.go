// Package dashboard serves the embedded admin web UI: a single-binary,
// modern Svelte 5 + Tailwind control panel for the proxy (health, config, tokens, logs,
// metrics). Static assets and templates are vendored and embedded via go:embed —
// no runtime CDN, no runtime Node.js dependency, and zero external network calls.
package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/updatecheck"
)

// Dashboard renders the admin UI over the live pool, registry, and config.
type Dashboard struct {
	cfg     func() *config.Config // returns the current (hot-reloadable) config
	pool    *pool.Pool
	reg     *registry.Registry
	logger  *slog.Logger
	logs    *logring.Handler // dashboard log viewer source (nil = disabled)
	started time.Time

	// version is the running release tag ("" / "dev" for dev builds) and
	// updates is the release-update indicator (issue #50b); the layout
	// shows a badge when a newer GitHub release exists. Both may be left
	// unset (no badge).
	version string
	updates *updatecheck.Checker

	// metricHist is the rolling counter history sampled by the metrics page
	// (UI-poll-driven, not a background goroutine). Per-instance so multiple
	// dashboards never share one window.
	metricsMu  sync.Mutex
	metricHist []metricSample
}

// Option configures optional Dashboard features (version + update checker).
type Option func(*Dashboard)

// WithVersion wires the running release tag and the update checker for the
// header badge (issue #50b). Nil checker disables the badge.
func WithVersion(version string, updates *updatecheck.Checker) Option {
	return func(d *Dashboard) {
		d.version = version
		d.updates = updates
	}
}

// New builds the dashboard. cfg must return the current configuration — the
// server passes its atomic pointer loader so /admin/reload is reflected
// immediately. A nil logger falls back to slog.Default(). Template parse
// failures panic: the templates are embedded, so a parse error is a build
// invariant violation, not a runtime condition. logs is the optional log
// viewer ring (nil hides the /admin/logs page data).
func New(cfg func() *config.Config, p *pool.Pool, reg *registry.Registry, logger *slog.Logger, logs *logring.Handler, opts ...Option) *Dashboard {
	if logger == nil {
		logger = slog.Default()
	}
	d := &Dashboard{cfg: cfg, pool: p, reg: reg, logger: logger, started: time.Now(), logs: logs}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// releaseURL is where the update badge points (the releases page).
const releaseURL = "https://github.com/trefeon/freebuff-proxy/releases"

// pickDefaultModel selects deepseek/deepseek-v4-flash when present, or the first available model.
func pickDefaultModel(models []string) string {
	if len(models) == 0 {
		return ""
	}
	const preferred = "deepseek/deepseek-v4-flash"
	for _, m := range models {
		if m == preferred {
			return preferred
		}
	}
	for _, m := range models {
		if strings.Contains(m, "deepseek-v4-flash") {
			return m
		}
	}
	return models[0]
}

// APIHandler returns a handler that writes the named view model as JSON.
func (d *Dashboard) APIHandler(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		data := d.dataFor(name, r)
		_ = json.NewEncoder(w).Encode(data)
	}
}

// APIVersion returns the running version and update check result as JSON.
func (d *Dashboard) APIVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"current_version": d.version,
		"has_update":      false,
		"latest_version":  "",
		"update_url":      releaseURL,
	}
	if d.version != "" && d.updates != nil && r.Context() != nil {
		if latest, err := d.updates.Latest(r.Context()); err == nil && latest != "" && updatecheck.UpdateAvailable(d.version, latest) {
			resp["has_update"] = true
			resp["latest_version"] = latest
		}
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// dataFor resolves the page data for a named content template.
func (d *Dashboard) dataFor(name string, r *http.Request) any {
	switch name {
	case "overview":
		return d.overviewData()
	case "config":
		return d.configData()
	case "tokens":
		return d.tokensData()
	case "models":
		return d.modelsData()
	case "logs":
		return d.logsData(r)
	case "traces":
		return d.tracesData()
	case "setup":
		return d.setupData()
	case "metrics":
		return d.metricsData()
	default:
		return nil
	}
}
