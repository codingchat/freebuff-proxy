# freebuff-proxy

[![CI](https://img.shields.io/github/actions/workflow/status/trefeon/freebuff-proxy/ci.yml)](https://github.com/trefeon/freebuff-proxy/actions/workflows/ci.yml)

freebuff-proxy is an OpenAI-compatible proxy bridge that turns FreeBuff (Codebuff's free coding agent) into a standard API. FreeBuff's backend fingerprints official-CLI traffic and rejects direct calls with `403 free_mode_cli_required`, so the proxy replicates the CLI request envelope, manages the free-session and agent-run lifecycle upstream, and pools multiple tokens — exposing plain `/v1/chat/completions`, `/v1/models`, and `/healthz` endpoints that 9router, opencode, claude-code, or any OpenAI client can wire to as an ordinary provider.

**TL;DR:** copy `.env.example` to `.env`, set `AUTH_TOKENS` to your FreeBuff token, run `./freebuff-proxy` (or `docker compose up --build`), then point any OpenAI-compatible client at `http://localhost:3457/v1`. Full 9router wiring: [docs/guides/9router-integration.md](docs/guides/9router-integration.md).

Docs: [PRD](docs/product/prd.md) · [Reference analysis](docs/research/freebuff-reference-analysis.md) · [FreeBuff limitations & quota research](docs/research/freebuff-limitations.md) · [Delivery tasks](docs/delivery/tasks.md) · [Kaspersky false-positive notes](docs/security/av-kaspersky-false-positive.md)

## How it works

- **CLI-envelope fingerprint injection** — each upstream request carries the `x-freebuff-model` / `x-freebuff-instance-id` headers, a `codebuff_metadata {run_id, client_id, freebuff_instance_id}` body block, and a rotating user agent, so the upstream accepts it as CLI traffic.
- **Free-session lifecycle** — per-token single-flight session create/poll/end with automatic recreate on `ended`/`superseded`/`expired`; the waiting room is surfaced to clients as `503` + `Retry-After`.
- **Per-agent run lifecycle** — runs are prewarmed at boot, STARTed on demand, maintained every 60s, rotated every `ROTATION_INTERVAL`, and FINISHed on rotation and shutdown.
- **Multi-token pool** — `AUTH_TOKENS` (comma-separated) round-robin with linear failover; a token cools down for 30 minutes after a 401; when every token is queued, the token with the best waiting-room position is chosen.
- **Live model registry** — the agent-to-model map is parsed from the `CodebuffAI/codebuff` TypeScript sources every `REGISTRY_REFRESH` (6h by default, hardcoded fallback at boot) and served via `/v1/models`.

## Getting a token

The proxy needs one or more FreeBuff **auth tokens** (`user_...` or UUID format) to talk to the upstream. There are two ways to obtain one (documented by the community proxies this project is based on), plus a script that automates the CLI path:

- **Script (recommended):** `scripts/get-freebuff-token.ps1` (PowerShell) or
  `scripts/get-freebuff-token.sh` (bash) — installs the official CLI, walks you through the
  login, extracts the token from the credentials file, and writes `AUTH_TOKENS` into `.env`
  (or `-ToClipboard` / `-Print`).

**Method 1 — Web (no install):** visit **[https://freebuff.llm.pm](https://freebuff.llm.pm)**, log in with your FreeBuff/Codebuff account, and the auth token is displayed directly on the page. Copy it. (Alternative: log in on the FreeBuff site, open DevTools → Application → Local Storage, and copy the auth token from there.)

**Method 2 — Official CLI:** install and log in once — the CLI saves the token to a local credentials file:

```bash
npm i -g freebuff     # or the codebuff CLI, whichever you have
freebuff              # first launch walks you through login
```

| OS | Credentials path |
|---|---|
| Windows | `C:\Users\<username>\.config\manicode\credentials.json` |
| Linux / macOS | `~/.config/manicode/credentials.json` |

The file looks like this — only the `authToken` value is needed:

```json
{
  "default": {
    "id": "user_10293847",
    "name": "you",
    "authToken": "fa82b5c1-e39d-4c7a-961f-d2b3c4e5f6a7",
    "...": "..."
  }
}
```

**Rules:**
- Copy the token **without** any `Bearer ` prefix — the proxy adds it upstream itself.
- `.env` and `config.json` are gitignored: tokens never get committed.
- For higher throughput, log in with multiple accounts and comma-separate all their tokens (`AUTH_TOKENS=tok1,tok2`) — the pool round-robins across them and fails over on 401 (30-min cooldown per token).
- A token that persistently gets `401` upstream is expired/revoked — get a fresh one with the same steps.

## Quick start (binary)

Requires Go 1.26+ (see `go.mod`) or a prebuilt release binary.

1. Build:

   ```
   go build -o freebuff-proxy.exe ./cmd/freebuff-proxy
   ```

   (Linux/macOS: `go build -o freebuff-proxy ./cmd/freebuff-proxy`.)

2. Create the config file and set the upstream token:

   ```
   Copy-Item .env.example .env      # PowerShell
   cp .env.example .env             # bash
   ```

   Edit `.env` and set `AUTH_TOKENS` — **REQUIRED**: a FreeBuff/Codebuff token, comma-separated for multiple accounts. The proxy refuses to start without at least one token.

3. Run:

   ```
   .\freebuff-proxy.exe
   ```

   It listens on `127.0.0.1:3457` (loopback only — the proxy holds FreeBuff tokens) by default; set `LISTEN_ADDR=:3457` to expose it on all interfaces (e.g. inside a container).

4. Smoke test (Windows PowerShell: use `curl.exe`):

   ```
   curl http://localhost:3457/healthz
   curl http://localhost:3457/v1/models
   curl -N http://localhost:3457/v1/chat/completions -H "Content-Type: application/json" -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"Say hello in one short sentence."}],"stream":true}'
   ```

   `/healthz` returns uptime, model count, and the per-token pool snapshot; `/v1/models` returns the OpenAI model-list shape from the registry. The chat call streams SSE tokens back (a real token needs to be reachable upstream; a dummy `AUTH_TOKENS` fails with an upstream error, which is also expected behavior).

## Quick start (Docker)

1. Create `.env` from `.env.example` and set `AUTH_TOKENS` (required) — or run
   `scripts/get-freebuff-token.ps1/.sh` to obtain one automatically.
2. Build and start:

   ```
   docker compose up --build
   ```

   **Linux one-shot installer:** `scripts/setup-proxy-docker.sh` clones the repo (if needed),
   grabs the token, builds/starts the container, waits for the healthcheck, and then prints
   the exact 9router form values — including the correct Base URL for the case where 9router
   itself runs in Docker (it auto-detects the Docker bridge gateway, e.g. `172.18.0.1`).
3. Confirm the healthcheck passes, then smoke test as in the binary quick start:

   ```
   docker compose ps
   curl http://localhost:3457/healthz
   ```

   The compose example publishes port `3457:3457` and runs a busybox `wget` healthcheck against `/healthz` (30s interval, 10s start period). `LISTEN_ADDR` stays `:3457` inside the container — the port is published on the host. If you enable `DEBUG_DUMP=true`, mount `./dump` at `/dump` to persist captured traffic (see the commented `volumes` entry in `docker-compose.yml`).

## Configuration

All keys are read from the environment and override the JSON config file passed via `-config`; keys in the JSON file mirror these names. `-v` enables verbose logging.

| Key | Default | Description |
|---|---|---|
| `AUTH_TOKENS` | _(none — REQUIRED)_ | FreeBuff auth token(s) for the upstream Codebuff API. Comma-separated for multiple accounts (round-robin + failover across tokens). |
| `LISTEN_ADDR` | `127.0.0.1:3457` | Listen address for the OpenAI-compatible API surface (`/v1/chat/completions`, `/v1/models`, `/healthz`). Loopback by default — the proxy holds FreeBuff tokens; use `:3457` only when firewalled/containerized. |
| `UPSTREAM_BASE_URL` | `https://codebuff.com` | Upstream Codebuff base URL. The host is normalized to `www.codebuff.com`. |
| `ROTATION_INTERVAL` | `6h` | How long an agent run lives upstream before it is rotated (FINISH + restart). |
| `REQUEST_TIMEOUT` | `15m` | Timeout for a single upstream chat-completions request (stream included). |
| `SESSION_CALL_TIMEOUT` | `30s` | Timeout for individual session / agent-run API calls (create, poll, start...). |
| `REGISTRY_REFRESH` | `6h` | How often the model registry re-fetches the Codebuff TS sources (`free-agents.ts`, `freebuff-models.ts`, `gemini.ts`, ...). Failures keep the previous mapping (or the hardcoded fallback at boot). |
| `API_KEYS` | _(empty — no client auth)_ | Optional: API keys clients must present (`Authorization: Bearer` or `x-api-key`). Comma-separated. Empty = no client auth. |
| `HTTP_PROXY` | _(empty)_ | Outbound HTTP/HTTPS proxy (CONNECT tunneling). Empty = direct. |
| `SOCKS5_PROXY` | _(empty)_ | Outbound SOCKS5 proxy (e.g. `socks5://127.0.0.1:1080`). Empty = direct. |
| `COST_MODE` | _(empty — omit)_ | `cost_mode` sent upstream with chat requests: "" (omit — proxy-freebuff's 2026-08 behavior) or "free". Empirical A/B pending, see PRD §8. |
| `DEBUG_DUMP` | `false` | Dump raw upstream request/response traffic into `./dump` for debugging. |
| `LOG_FILE` | _(empty — stderr only)_ | Optional log file path (in addition to stderr). Empty = stderr only. |
| `LOG_LEVEL` | _(empty — info)_ | Log verbosity: `debug`, `info`, `warn`, `error`. Empty = `info` (or `debug` with `-v`); `LOG_LEVEL` wins over `-v`. |

## 9router integration

Add freebuff-proxy as an **OpenAI-compatible custom provider** in 9router — the full step-by-step guide (install 9router, dashboard form fields, model catalog, verification, troubleshooting) is in **[docs/guides/9router-integration.md](docs/guides/9router-integration.md)**.

Quick reference:

```json
{ "freebuff": { "base_url": "http://localhost:3457/v1", "api_key": "user_...", "models": ["deepseek/deepseek-v4-flash"] } }
```

- Dashboard → **Providers → Add OpenAI Compatible** → `base_url http://localhost:3457/v1`
- `api_key`: any value when the proxy has no `API_KEYS` set; otherwise one of your `API_KEYS`
- Models: the live list from `/v1/models` (12 models, refreshed every 6h from upstream sources)
- Model combo ids become `freebuff/<model-id>` (e.g. `freebuff/deepseek-v4-flash`)

## Testing against the mock upstream

The test suite runs against a mock Codebuff upstream (`internal/testutil`) — no real token needed:

```
go test ./...
```

CI runs `go vet` and `go test -race ./...` on Linux (the race detector needs a C toolchain, hence CI-only). On this Windows dev machine, Kaspersky may quarantine freshly linked test binaries out of the go-build cache (`fork/exec ... Access is denied`); that is a validated false positive — see [docs/security/av-kaspersky-false-positive.md](docs/security/av-kaspersky-false-positive.md) for workarounds (add a Kaspersky exclusion for the `go-build*` cache path, or `go test -c -o out\convert.test.exe ./internal/convert` + run it directly).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `403 free_mode_cli_required` | The CLI envelope did not satisfy the anti-bot gate. Check `COST_MODE` (omit vs `"free"` — A/B pending, see PRD §8) and the `client_id` format (13-char base36). |
| `402 Out of credits` / `403 country_blocked` | Geo-gating: blocked-country / VPN / datacenter IPs are rejected upstream. Use `HTTP_PROXY` or `SOCKS5_PROXY` with a clean residential egress. |
| `503` with `waiting_room_queued` | Normal: the free session is queued in the waiting room. The `Retry-After` header tells the client when to retry; 9router and opencode retry automatically. |
| `429` with `rate_limited` in the body | The token's daily session quota is exhausted (6/day on the limited tier, resets at Pacific midnight). The proxy now returns `429 + Retry-After` with the upstream `resetAt` so clients back off instead of hammering. Add another `AUTH_TOKENS` or wait for the reset. |
| `502 upstream_unavailable` | Every token failed or is in cooldown. Check token validity — a 401 puts a token in a 30-minute cooldown. |
| Logs too quiet or too noisy | Set `LOG_LEVEL=debug` (or run with `-v`) for full visibility: access log, upstream calls, session/run lifecycle. `LOG_LEVEL=warn` silences chat noise. |
| Need raw upstream traffic dumps | Set `DEBUG_DUMP=true` — requests/responses land in `./dump/` (sensitive headers redacted). |
| Logs scroll away in terminals / containers | Set `LOG_FILE=/path/to/proxy.log` — the same lines are appended to the file (colors disabled there). |
| Antivirus (e.g. Kaspersky) flags test binaries or the `.exe` | False positive; Go binaries trip AV heuristics. See [docs/security/av-kaspersky-false-positive.md](docs/security/av-kaspersky-false-positive.md). |

## Terms of use

FreeBuff is only intended to be used through the official CLI. This proxy uses undocumented upstream endpoints and replicates CLI fingerprints, which conflicts with the letter of the service terms; account bans are possible. Use it for educational and personal experimentation at your own risk, and keep usage modest. This project is not affiliated with or endorsed by Codebuff.
