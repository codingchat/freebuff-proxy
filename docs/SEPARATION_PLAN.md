# Project Separation Plan: Core CLI/Backend vs Dashboard

## Executive Summary

This plan separates the freebuff-proxy project into two independent modules:
1. **Core CLI/Backend** (`freebuff-proxy-core`) - The main proxy engine, CLI, and API server
2. **Dashboard** (`freebuff-proxy-dashboard`) - The Svelte-based admin UI

**Priority**: Core CLI/Backend first, then dashboard as a separate module.

---

## Current Architecture Analysis

### Current Structure
```
freebuff-proxy/
├── cmd/freebuff-proxy/          # CLI entry point
│   ├── main.go                  # Main entry, flags, server setup
│   ├── doctor.go                # Diagnostics
│   ├── setup.go                 # Interactive setup
│   ├── service.go               # Service install/uninstall
│   └── ...
├── internal/
│   ├── config/                  # Configuration loading
│   ├── pool/                    # Token pool management
│   ├── session/                 # Session handling
│   ├── upstream/                # Wire protocol client
│   ├── convert/                 # Model translation
│   ├── server/                  # HTTP gateway (includes admin routes)
│   ├── dashboard/               # Admin UI (Svelte embed)
│   ├── runs/                    # Agent run lifecycle
│   ├── stealth/                 # TLS fingerprinting
│   ├── ratelimit/               # Rate limiting
│   ├── telemetry/               # Prometheus metrics
│   ├── registry/                # Model registry
│   ├── logring/                 # Log ring buffer
│   ├── notify/                  # Webhook notifications
│   ├── phasetiming/             # Phase timing
│   ├── tokenestimate/           # Token estimation
│   └── updatecheck/             # Update checking
└── frontend/                    # Svelte 5 SPA source
```

### Coupling Analysis

**Dashboard depends on Core:**
- `config.Config` - Configuration access
- `pool.Pool` - Token pool state
- `registry.Registry` - Model registry
- `logring.Handler` - Log viewer
- `phasetiming` - Phase timing data
- `updatecheck.Checker` - Update checking

**Core depends on Dashboard:**
- `server` package includes admin routes (`/admin/*`)
- `main.go` creates dashboard and passes to server

**Shared Dependencies:**
- Both use `config`, `pool`, `registry` packages
- Both use `telemetry` for logging

---

## Target Architecture

### Option A: Monorepo with Separate Modules (Recommended)

```
freebuff-proxy/
├── core/                        # Core module
│   ├── go.mod                   # Independent module
│   ├── cmd/freebuff-proxy/      # CLI entry point
│   ├── internal/
│   │   ├── config/
│   │   ├── pool/
│   │   ├── session/
│   │   ├── upstream/
│   │   ├── convert/
│   │   ├── server/              # Core HTTP (no admin routes)
│   │   ├── runs/
│   │   ├── stealth/
│   │   ├── ratelimit/
│   │   ├── telemetry/
│   │   ├── registry/
│   │   ├── logring/
│   │   ├── notify/
│   │   ├── phasetiming/
│   │   ├── tokenestimate/
│   │   └── updatecheck/
│   └── api/                     # Public API interfaces
│       └── interfaces.go
├── dashboard/                   # Dashboard module
│   ├── go.mod                   # Independent module
│   ├── internal/
│   │   └── dashboard/           # Dashboard logic
│   ├── frontend/                # Svelte 5 SPA
│   └── api/                     # Dashboard API handlers
├── go.work                      # Go workspace
└── docs/
```

### Option B: Separate Repositories

```
freebuff-proxy-core/              # Core repo
freebuff-proxy-dashboard/         # Dashboard repo (imports core)
```

**Recommendation**: Option A (Monorepo) for easier development and testing.

---

## Core Module Design

### Package Structure

```go
// core/api/interfaces.go
package api

// PoolSnapshot provides read-only access to pool state.
type PoolSnapshot interface {
    Tokens() []TokenSnapshot
    PoolStats() PoolStats
    BridgeCount() int
    TokenCount() int
}

// Registry provides read-only access to model registry.
type Registry interface {
    Models() []string
    ModelCount() int
    AgentForModel(model string) (string, error)
    AgentIDs() []string
}

// Config provides read-only access to configuration.
type Config interface {
    ListenAddr() string
    UpstreamBaseURL() string
    EffectiveMode() string
    // ... other config fields
}

// LogRing provides access to log entries.
type LogRing interface {
    Recent(n int) []LogEntry
}

// Dashboard optionally provides admin UI.
type Dashboard interface {
    ServeHTTP(w http.ResponseWriter, r *http.Request)
}
```

### Server Package Refactoring

**Current**: `server` package includes both core API and admin routes.

**Target**: Split into:
- `server/core.go` - Core API routes (`/v1/*`, `/healthz`)
- `server/admin.go` - Admin routes (`/admin/*`) - optional, provided by dashboard

```go
// server/server.go
type Server struct {
    cfg       *config.Config
    pool      api.PoolSnapshot
    reg       api.Registry
    dashboard api.Dashboard  // optional, nil = no admin UI
    // ...
}

func (s *Server) Handler() http.Handler {
    mux := http.NewServeMux()
    
    // Core API routes (always enabled)
    mux.HandleFunc("/v1/chat/completions", s.handleChat)
    mux.HandleFunc("/v1/models", s.handleModels)
    mux.HandleFunc("/healthz", s.handleHealth)
    
    // Admin routes (optional, provided by dashboard)
    if s.dashboard != nil {
        mux.Handle("/admin/", s.dashboard)
    }
    
    return mux
}
```

### CLI Entry Point

```go
// core/cmd/freebuff-proxy/main.go
func main() {
    // ... flag parsing, config loading
    
    // Create core components
    cfg := config.Load(...)
    pool := pool.New(...)
    reg := registry.New(...)
    
    // Optionally attach dashboard
    var dash api.Dashboard
    if cfg.DashboardEnabled {
        dash = dashboard.New(pool, reg, ...)
    }
    
    // Create server with optional dashboard
    srv := server.New(cfg, pool, reg, dash)
    
    // Start server
    httpServer := &http.Server{
        Addr:    cfg.ListenAddr,
        Handler: srv.Handler(),
    }
    httpServer.ListenAndServe()
}
```

---

## Dashboard Module Design

### Package Structure

```go
// dashboard/internal/dashboard/dashboard.go
package dashboard

type Dashboard struct {
    pool      api.PoolSnapshot
    reg       api.Registry
    logs      api.LogRing
    updates   api.UpdateChecker
    // ...
}

func New(pool api.PoolSnapshot, reg api.Registry, ...) *Dashboard {
    return &Dashboard{pool: pool, reg: reg, ...}
}

func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // Serve admin UI
}
```

### Frontend Integration

The `frontend/` directory moves to `dashboard/frontend/` and builds independently.

```yaml
# dashboard/frontend/package.json
{
  "scripts": {
    "dev": "vite dev",
    "build": "vite build",
    "preview": "vite preview"
  }
}
```

### Go Embed

```go
// dashboard/internal/dashboard/assets.go
package dashboard

//go:embed frontend/dist/*
var assets embed.FS
```

---

## Interface Contracts

### Core Exports

The core module exports these interfaces for the dashboard:

```go
// PoolSnapshot interface
type PoolSnapshot interface {
    Tokens() []TokenSnapshot
    PoolStats() PoolStats
    BridgeCount() int
    TokenCount() int
}

// TokenSnapshot struct
type TokenSnapshot struct {
    Index            int
    SessionStatus    string
    QueuePosition    int
    QueueDepth       int
    ActiveRuns       int
    Requests         int
    Messages24h      int
    DailyLimit       int
    UsagePct         int
    RiskLevel        string
    CooldownUntil    time.Time
    TransientRetries int64
    Standing         *StandingInfo
    QuotaByModel     map[string]QuotaInfo
}

// Registry interface
type Registry interface {
    Models() []string
    ModelCount() int
    AgentForModel(model string) (string, error)
    AgentIDs() []string
}

// LogRing interface
type LogRing interface {
    Recent(n int) []LogEntry
}

// LogEntry struct
type LogEntry struct {
    Time    string
    Level   string
    Message string
    Fields  []string
}
```

### Dashboard Imports

The dashboard module imports core interfaces:

```go
import (
    "freebuff-proxy-core/api"
)

type Dashboard struct {
    pool api.PoolSnapshot
    reg  api.Registry
    logs api.LogRing
}
```

---

## Phased Implementation

### Phase 1: Core Module Extraction (Priority)

**Goal**: Create standalone core module with all backend functionality.

**Steps**:
1. Create `core/` directory structure
2. Move core packages to `core/internal/`
3. Create `core/api/interfaces.go` with exported interfaces
4. Refactor `server` to accept optional dashboard
5. Update CLI entry point
6. Create `core/go.mod`
7. Update tests
8. Verify core works independently

**Deliverables**:
- `core/` directory with standalone module
- Core can run without dashboard
- All tests pass
- CLI flags work: `-doctor`, `-setup`, `-test-token`, etc.

### Phase 2: Dashboard Module Extraction

**Goal**: Create standalone dashboard module that imports core interfaces.

**Steps**:
1. Create `dashboard/` directory structure
2. Move dashboard package to `dashboard/internal/`
3. Move `frontend/` to `dashboard/frontend/`
4. Update dashboard to use core interfaces
5. Create `dashboard/go.mod`
6. Update build process
7. Verify dashboard works with core

**Deliverables**:
- `dashboard/` directory with standalone module
- Dashboard can be built and run separately
- Frontend builds independently

### Phase 3: Integration

**Goal**: Wire core and dashboard together.

**Steps**:
1. Create `go.work` for workspace
2. Update main entry point to use both modules
3. Update CI/CD pipeline
4. Update documentation
5. Test full integration

**Deliverables**:
- Working monorepo with both modules
- CI/CD builds both modules
- Documentation updated

---

## Migration Strategy

### Backward Compatibility

- Core module maintains same CLI flags and behavior
- Dashboard module provides same admin UI
- No breaking changes for users

### Testing Strategy

- Core module: unit tests + integration tests
- Dashboard module: unit tests + E2E tests
- Integration: full system tests

### Build Process

```bash
# Build core
cd core && go build ./cmd/freebuff-proxy

# Build dashboard
cd dashboard && go build ./...

# Build everything (from root)
go work sync
go build ./...
```

---

## Risk Assessment

### High Risk
- **Circular dependencies**: Must ensure clean interface boundaries
- **Test coverage**: Must maintain test coverage during refactoring

### Medium Risk
- **Build complexity**: Two modules increase build complexity
- **Version management**: Must coordinate versions between modules

### Low Risk
- **Performance**: No performance impact expected
- **User impact**: No breaking changes for users

---

## Success Criteria

- [ ] Core module builds and runs independently
- [ ] Dashboard module builds and runs independently
- [ ] All existing tests pass
- [ ] No breaking changes for users
- [ ] Documentation updated
- [ ] CI/CD pipeline works

---

## Timeline

- **Phase 1**: 2-3 days (Core extraction)
- **Phase 2**: 1-2 days (Dashboard extraction)
- **Phase 3**: 1 day (Integration)

**Total**: 4-6 days

---

## Next Steps

1. Review this plan with team
2. Approve target architecture
3. Begin Phase 1 implementation
4. Create tracking issues
5. Start coding

---

## Appendix: File Movement Map

### Core Module
```
cmd/freebuff-proxy/ → core/cmd/freebuff-proxy/
internal/config/ → core/internal/config/
internal/pool/ → core/internal/pool/
internal/session/ → core/internal/session/
internal/upstream/ → core/internal/upstream/
internal/convert/ → core/internal/convert/
internal/server/ → core/internal/server/
internal/runs/ → core/internal/runs/
internal/stealth/ → core/internal/stealth/
internal/ratelimit/ → core/internal/ratelimit/
internal/telemetry/ → core/internal/telemetry/
internal/registry/ → core/internal/registry/
internal/logring/ → core/internal/logring/
internal/notify/ → core/internal/notify/
internal/phasetiming/ → core/internal/phasetiming/
internal/tokenestimate/ → core/internal/tokenestimate/
internal/updatecheck/ → core/internal/updatecheck/
```

### Dashboard Module
```
internal/dashboard/ → dashboard/internal/dashboard/
frontend/ → dashboard/frontend/
```

### Shared
```
docs/ → docs/ (stays at root)
scripts/ → scripts/ (stays at root)
.github/ → .github/ (stays at root)
```
