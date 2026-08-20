# AGENTS.md — freebuff-proxy

## What this is

A Go 1.26 OpenAI-compatible gateway that fronts the FreeBuff/Codebuff CLI wire
protocol: it owns a pool of FreeBuff auth tokens and serves `/v1/chat/completions`
(plus an Anthropic-compatible surface) by admitting sessions, translating models,
and relaying chat streams upstream. A Svelte 5 SPA dashboard (login, token
management, config editor, diagnostics, playground) is embedded via `go:embed`
in `internal/dashboard` and served under `/admin`. Reverse-engineering notes on
the upstream CLI live in `reference/` (see invariants below).

## Package map

- `internal/config` — typed config surface: `Config` struct, `Load`/`Validate`,
  `.env` + `-config` JSON precedence, atomic swap via `s.cfg`.
- `internal/registry` — model registry synced from upstream; `ResolveModel`
  (suffix/alias/`-max` upgrade) + `AgentForModel` (`base2-free-*` root);
  `parse.go` mirrors `reference/proxy-freebuff/lib/registry.js`.
- `internal/convert` — model-translation layer (OpenAI → FreeBuff wire shape,
  reasoning-effort ladders, DeepSeek `medium→high`, accumulation). Monolith;
  split into `normalize/accumulator/schemacache` is planned (R8, blocked on PR
  #141) and is a CI file-size allowlist exception.
- `internal/pool` — token pool: `Acquire`/`Chat`/`Lease`, admission coercion
  (lease model is authoritative), cooldowns, quota windows, spend buckets, unfit
  registry, create gates. Split: `cooldown.go`, `quota.go`, `lifecycle.go`,
  `spend.go`, `unfit.go`, `create_gate.go` (`pool.go` itself is a CI exception).
- `internal/session` — upstream session store + on-disk persistence
  (`store.go`), session lifecycle and re-admission.
- `internal/upstream` — the FreeBuff wire client: `client.go` core plus
  `session.go` (session create/poll), `chat.go` (chat relay), `ads.go`, `errors.go`
  (typed sentinels), `ratelimit.go` (429 taxonomy), `auth_login.go` (device-code
  login wizard).
- `internal/server` — HTTP gateway: `server.go` (routes/harness), `models.go`,
  `errors.go`, `health.go`, `chat.go` (chat completions pipeline), `anthropic.go`
  (Anthropic-compatible surface), `responses.go`, and the admin dashboard split
  across `admin.go`, `admin_auth.go`, `admin_tokens.go`, `admin_env.go`.
- `internal/dashboard` — embedded Svelte 5 SPA (`go:embed`), render helpers
  (fragments, config-result rows, diag checks), htmx + Pico styling.
- `internal/runs` — agent-run lifecycle: START/FINISH, steps, drain queue,
  honest `cancelled/failed/completed` status.
- `internal/reasoningcache` — cache for reasoning-effort computations.
- `internal/ratelimit` — per-IP request limiter.
- `internal/egress` — optional egress probe (one-shot at startup; opt-in
  jittered loop) for connectivity health.
- `internal/stealth` — utls TLS fingerprint profiles, header sanitization,
  per-GOOS risk defaults (anti-ban stealth).
- `internal/telemetry` — Prometheus metrics and redaction-safe logging hooks.
- `internal/logring` — bounded in-memory ring buffer wrapping `slog` for the
  dashboard log viewer.
- `internal/notify` — webhook notifications for lifecycle events.
- `internal/phasetiming` — per-request phase timing (acquire/upstream TTFB/total)
  surfaced in the dashboard smoke test.
- `internal/tokenestimate` — tiktoken-based token estimation.
- `internal/updatecheck` — self-update version check.

## Request data flow

`client model` → `registry.ResolveModel` (suffix → alias → `-max` upgrade) →
`server.modelAllowed` (MODELS_ALLOW gate) → `registry.AgentForModel`
(`base2-free-*` root for session admission) → `convert` effort clamp
(per-model reasoning ladders; DeepSeek `medium→high`) → `pool.Acquire`
(admission coercion: the lease model is authoritative over the requested one) →
upstream wire (`x-freebuff-model` on session POST only; chat requests carry NO
model header — never add one).

## Load-bearing invariants

- **Hermetic tests**: `AUTH_TOKENS` and `ADMIN_TOKEN` MUST be UNSET when running
  the test suite — an ambient `AUTH_TOKENS` silently flips bridge-mode tests.
  Use `env -u AUTH_TOKENS -u ADMIN_TOKEN go test ./...`.
- **Port-parity with `reference/proxy-freebuff/lib/registry.js`**: Go's registry
  intentionally mirrors its quirks. Never "fix" the JS behavior into Go without
  re-verifying against a live capture.
- **Anti-ban checklist**: `docs/guides/freebuff-cli-internals.md` §10 is the
  contract the gateway must replicate (session headers per method, no heartbeat,
  no chat model header, fresh random `client_id` per call, honest FINISH, grace
  ride). Changes to the wire client must be checked against it.
- **`reference/` is read-only RE source** — a vendored upstream clone. Never
  edit it, never build from it, never treat it as an edit target.
- **File-size budget**: 1400 lines per non-test `.go` file, enforced in CI
  (`.github/workflows/ci.yml`). `internal/convert/convert.go`,
  `internal/pool/pool.go`, and `internal/runs/runs.go` are tracked exceptions
  with shrink follow-ups — do not add to the allowlist without a documented
  reason.

## Doc pointers

- `docs/guides/freebuff-cli-internals.md` — upstream CLI wire protocol RE (session,
  chat, quota/spend, anti-ban).
- `docs/guides/model-translation-layer.md` — model id mapping, effort ladders,
  translation decisions.
- `docs/guides/9router-integration.md` — multi-key 9router fallback/priority.
- `docs/guides/getting-started.md` — build, run, configure.

## Workflow

- Work in git worktrees (`.worktrees/`); the main checkout at the repo root may
  be shared with other sessions — never read-edit it or run git there.
- Commit convention: conventional subjects (`refactor(...)`, `fix(...)`,
  `docs+ci(...)`), CI runs build + vet + `-race` tests.
- Release flow: bump the patch/stage version for minor changes, tag after
  pushing main, verify the goreleaser assets.
