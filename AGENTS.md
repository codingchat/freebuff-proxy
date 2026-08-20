# AGENTS.md — freebuff-proxy

## What this is

A Go 1.26 OpenAI-compatible gateway that fronts the FreeBuff/Codebuff CLI wire
protocol: it owns a pool of FreeBuff auth tokens and serves `/v1/chat/completions`
(plus an Anthropic-compatible surface) by admitting sessions, translating models,
and relaying chat streams upstream. A Svelte 5 SPA dashboard (login, token
management, config editor, diagnostics, playground) is embedded via `go:embed`
in `internal/dashboard` and served under `/admin`. Reverse-engineering notes on
the upstream CLI are distilled in `devdocs/guides/freebuff-cli-internals.md` (the
vendored `reference/` clones were removed 2026-08-20 after the RE completed).

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
  registry, create gates. Split: `acquire.go`, `bridge.go`, `cooldown.go`,
  `create_gate.go`, `lifecycle.go`, `quota.go`, `snapshot.go`, `spend.go`,
  `unfit.go` (+ slim `pool.go` core).
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
- `internal/dashboard` — embedded Svelte 5 SPA (`go:embed`), Tailwind CSS,
  Geist font, JSON API endpoints (config, tokens, smoke test, diagnostics).
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
- **Anti-ban checklist**: `devdocs/guides/freebuff-cli-internals.md` §10 is the
  contract the gateway must replicate (session headers per method, no heartbeat,
  no chat model header, fresh random `client_id` per call, honest FINISH, grace
  ride). Changes to the wire client must be checked against it.
- **`reference/` removed 2026-08-20** — the gitignored vendored clones (774MB:
  `CodebuffAI/codebuff`, `proxy-freebuff`, 9router, community proxies) were
  deleted after the RE completed; that knowledge is distilled in `devdocs/`
  (local-only) and promulgated in `docs/`, and pinned in
  `internal/registry/testdata/upstream/`. Re-clone the public
  repos if a cited line number needs re-verification.
- **File-size budget**: 1400 lines per non-test `.go` file, enforced in CI
  (`.github/workflows/ci.yml`). `internal/convert/convert.go` and
  `internal/runs/runs.go` are tracked exceptions with shrink follow-ups — do
  not add to the allowlist without a documented reason. (`pool.go` was removed
  from the exceptions after the split into `acquire.go`/`bridge.go`/
  `snapshot.go`.)

## Doc pointers

- `devdocs/guides/freebuff-cli-internals.md` — upstream CLI wire protocol RE (session,
  chat, quota/spend, anti-ban) — local-only, never committed.
- `docs/model-translation-layer.md` — model id mapping, effort ladders,
  translation decisions.
- `docs/9router-integration.md` — multi-key 9router fallback/priority.
- `docs/getting-started.md` — build, run, configure.

## Repo policy — public repo

- This repository is **public**. Only public-facing content may ever be committed.
- Local/dev-only material stays **gitignored and uncommitted**, never `git add -f`:
  RE study and reverse-engineering notes, dev study docs, research/product/plan
  docs (`docs/` outside the public top-level pages), handoff notes (`HANDOFF.md`,
  `*.handoff.md`),
  dev tooling (`.codegraph/`, `uicheck/`), and reference clones (`reference/`).
- Only `docs/` top-level `.md` pages are committed documentation; the private RE
  notes live in `devdocs/` (gitignored) — never commit them.
- NEVER commit secrets: `.env*` (except `.env.example`), `config*.json` (except
  `config.example.json`), `*.pem`/`*.key`, or `.freebuff-session-state.json`
  (SHA-256 token keys + instance state).
- When dev knowledge matures into something public-safe, promote it as a new
  public `docs/` page deliberately — never by lifting private notes wholesale.

## Workflow

- Work directly in the main checkout; it may be shared with other sessions —
  coordinate on files, never destroy uncommitted work, and verify `origin/main`
  before pushing.
- Commit convention: conventional subjects (`refactor(...)`, `fix(...)`,
  `docs+ci(...)`), CI runs build + vet + `-race` tests.
- Release flow: bump the patch/stage version for minor changes, tag after
  pushing main, verify the goreleaser assets.
