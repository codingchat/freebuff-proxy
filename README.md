# freebuff-proxy

[![CI](https://img.shields.io/github/actions/workflow/status/trefeon/freebuff-proxy/ci.yml)](https://github.com/trefeon/freebuff-proxy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/releases)
[![License](https://img.shields.io/github/license/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/blob/main/LICENSE)

An OpenAI-compatible proxy bridge for the FreeBuff free tier. Point any OpenAI client at it and it talks to FreeBuff for you, with token pooling and session management built in.

FreeBuff (Codebuff's free coding agent) exposes its models only through the official CLI. The backend fingerprints CLI traffic and rejects direct API calls with `403 free_mode_cli_required`. freebuff-proxy replicates the CLI request envelope, manages the free-session and agent-run lifecycle upstream, and pools multiple tokens. Clients see a plain OpenAI-compatible API.

What this is not: an official FreeBuff or Codebuff product. It is a community bridge for an unofficial service. See the FAQ and Terms of use at the bottom.

## What it does

- Serves `/v1/chat/completions`, `/v1/models`, and `/healthz` on `127.0.0.1:3457` by default.
- Pools tokens: `AUTH_TOKENS` accepts comma-separated values, round-robins across them, and cools a token down for 30 minutes after a 401.
- Keeps free sessions alive: single-flight session create/poll/end, runs prewarmed at boot, rotated every `ROTATION_INTERVAL` (default 6h).
- Refreshes the model catalog every 6h from the Codebuff sources (15 models at boot, served by `/v1/models`).
- Sends outbound traffic through `HTTP_PROXY` or `SOCKS5_PROXY`, or impersonates a browser TLS fingerprint with `TLS_FINGERPRINT`.

## Requirements

- One FreeBuff auth token. The proxy refuses to start without at least one. See Getting a token.
- Release binaries run standalone. Building from source needs Go 1.26+ (see `go.mod`).

## Install

### Option 1: release binaries (recommended)

Download the archive for your platform from the [latest release](https://github.com/trefeon/freebuff-proxy/releases). Assets are named `freebuff-proxy_<version>_<os>_<arch>.tar.gz` (`zip` on Windows), and every release ships `checksums.txt`.

| Platform | Archive |
|---|---|
| linux / amd64 | `freebuff-proxy_<version>_linux_amd64.tar.gz` |
| linux / arm64 | `freebuff-proxy_<version>_linux_arm64.tar.gz` |
| macOS / amd64 | `freebuff-proxy_<version>_darwin_amd64.tar.gz` |
| macOS / arm64 | `freebuff-proxy_<version>_darwin_arm64.tar.gz` |
| windows / amd64 | `freebuff-proxy_<version>_windows_amd64.zip` |
| windows / arm64 | `freebuff-proxy_<version>_windows_arm64.zip` |

Example on linux amd64 (replace the version):

```bash
curl -sSL -o freebuff-proxy.tar.gz https://github.com/trefeon/freebuff-proxy/releases/latest/download/freebuff-proxy_0.1.1_linux_amd64.tar.gz
tar xzf freebuff-proxy.tar.gz
sha256sum -c checksums.txt --ignore-missing 2>/dev/null || echo "download checksums.txt from the release to verify"
./freebuff-proxy
```

Windows: extract the zip, then run `freebuff-proxy.exe` in a terminal.

### Option 2: Docker

Copy `.env.example` to `.env` and set `AUTH_TOKENS` first, then:

```bash
docker compose up --build
```

The compose file publishes port 3457 and runs a healthcheck against `/healthz`. For a one-shot setup on Linux, `scripts/setup-proxy-docker.sh` clones the repo, grabs the token, starts the container, and prints the 9router config with the right Docker gateway IP.

### Option 3: build from source

```bash
go build -o freebuff-proxy ./cmd/freebuff-proxy
```

Windows builds: `go build -o freebuff-proxy.exe ./cmd/freebuff-proxy`.

## Getting a token

Two ways, plus scripts that automate the CLI path:

- **Web (no install):** log in at [freebuff.llm.pm](https://freebuff.llm.pm) and copy the token shown on the page.
- **Official CLI:** `npm i -g freebuff`, run `freebuff` once to log in, then read `authToken` from `~/.config/manicode/credentials.json` (Windows: `C:\Users\<you>\.config\manicode\credentials.json`).
- **Scripts:** `scripts/get-freebuff-token.sh` (bash) or `scripts/get-freebuff-token.ps1` (PowerShell) install the CLI, log in, and write `AUTH_TOKENS` into `.env` for you.

Use the token without any `Bearer ` prefix; the proxy adds it upstream itself. For higher throughput, log in with several accounts and comma-separate the tokens: `AUTH_TOKENS=tok1,tok2`.

## Quick start

1. Copy the example config:

   ```bash
   cp .env.example .env
   ```

   (Windows PowerShell: `Copy-Item .env.example .env`)

2. Edit `.env` and set `AUTH_TOKENS`. This key is required.

3. Run the proxy:

   ```bash
   ./freebuff-proxy
   ```

4. Smoke test:

   ```bash
   curl http://localhost:3457/healthz
   curl http://localhost:3457/v1/models
   curl -N http://localhost:3457/v1/chat/completions \
     -H "Content-Type: application/json" \
     -d '{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"Say hello in one short sentence."}],"stream":true}'
   ```

`/healthz` returns uptime, model count, and the token pool snapshot. `/v1/models` returns the model list from the registry. The chat call streams SSE tokens back; with a dummy token you get an upstream error, which is expected.

## Configuration

Every key is read from the environment and overrides the JSON config file passed with `-config` (see `config.example.json`; keys mirror the env names). `-v` enables verbose logging.

| Key | Default | Description |
|---|---|---|
| `AUTH_TOKENS` | none, required | FreeBuff token(s), comma-separated. Round-robin + failover across tokens. |
| `LISTEN_ADDR` | `127.0.0.1:3457` | Listen address. Loopback only by default; use `:3457` in containers or behind a firewall. |
| `UPSTREAM_BASE_URL` | `https://codebuff.com` | Upstream base URL (host normalized to `www.codebuff.com`). |
| `ROTATION_INTERVAL` | `6h` | How long an agent run lives upstream before rotation (FINISH + restart). |
| `REQUEST_TIMEOUT` | `15m` | Timeout for one chat-completions request, stream included. |
| `SESSION_CALL_TIMEOUT` | `30s` | Timeout for individual session/run API calls. |
| `REGISTRY_REFRESH` | `6h` | How often the model registry re-fetches the Codebuff sources. |
| `API_KEYS` | empty | Optional client auth. Comma-separated keys clients must present. Empty means no client auth. |
| `HTTP_PROXY` | empty | Outbound HTTP/HTTPS proxy (CONNECT tunneling). |
| `SOCKS5_PROXY` | empty | Outbound SOCKS5 proxy, e.g. `socks5://127.0.0.1:1080`. |
| `COST_MODE` | empty | `cost_mode` sent upstream: omit or `"free"`. A/B testing pending (PRD section 8). |
| `DEBUG_DUMP` | `false` | Dump raw upstream traffic into `./dump` (sensitive headers redacted). |
| `LOG_FILE` | empty | Append logs to a file in addition to stderr. |
| `LOG_LEVEL` | info | `debug`, `info`, `warn`, or `error`. `-v` implies debug; `LOG_LEVEL` wins. |
| `TLS_FINGERPRINT` | empty | Outbound JA3 fingerprint: `chrome120`, `safari17`, `firefox120`, or `random`. |

## 9router integration

Add freebuff-proxy as an OpenAI-compatible custom provider in 9router. The step-by-step guide covers the dashboard form, model catalog, verification, and troubleshooting: [docs/guides/9router-integration.md](docs/guides/9router-integration.md).

Quick version: Dashboard, Providers, Add OpenAI Compatible. Base URL `http://localhost:3457/v1`, API Type Chat Completions, any non-empty API key, and the model ids come from `/v1/models`. Model combos become `freebuff/<model-id>`.

## Docs

- [9router integration guide](docs/guides/9router-integration.md): full wiring, model catalog, troubleshooting.
- The other project docs (PRD, research notes, delivery tasks, security notes) are local-only dev docs, gitignored on purpose. They do not ship with the repo.

## FAQ

**The proxy returns `403 free_mode_cli_required`.**

The CLI envelope did not satisfy the anti-bot gate. Check `COST_MODE` and the `client_id` format (13-char base36). If it started failing after a FreeBuff update, open an issue with the debug log (`LOG_LEVEL=debug`).

**I get `429` with `rate_limited` in the body.**

The token's daily session quota is exhausted (6 sessions per day on the limited tier, resets at Pacific midnight). The proxy returns `429` with the upstream `resetAt` so clients back off. Add another `AUTH_TOKENS` or wait for the reset.

**I get `403` with `account_banned` / `{"status":"banned"}`.**

Your FreeBuff account was banned upstream. This is the ToS risk this project documents; the token is dead and no setting will unban it. Get a fresh account and token. The proxy remembers the ban, stops hammering the upstream, and surfaces `403` with the upstream `resumes-at` until the window passes (24h if upstream sends no timestamp), then automatically re-probes: still banned means another window, unbanned means it just works.

**I get `503` with `waiting_room_queued`.**

Normal. The free session is queued in the waiting room. The `Retry-After` header tells the client when to retry; 9router and opencode retry automatically.

**Windows Defender or Kaspersky flags the binary or test executables.**

Go binaries trip AV heuristics; this is a validated false positive, not malware. For local `go test`, add the `go-build*` cache path to AV exclusions or run the compiled test binary directly. Details in the repo history; open an issue if you see a signature match (we have never seen one).

**Is this against FreeBuff's terms?**

FreeBuff is intended to be used through the official CLI only. This proxy uses undocumented endpoints and replicates CLI fingerprints, which conflicts with the letter of the service terms. Account bans are possible. Use it for personal and educational experimentation, keep usage modest, at your own risk.

**Still stuck?** Open an issue with the proxy version, your client, and `LOG_LEVEL=debug` output (redact tokens).

## Development

```bash
go build ./...
go vet ./...
go test ./...          # runs against the mock upstream, no token needed
golangci-lint run ./...  # lint config in .golangci.yml
```

CI runs `go test -race ./...` and `go mod verify` on Linux. Windows note: some AVs quarantine freshly linked test binaries out of the go-build cache (`fork/exec ... Access is denied`); that is the false positive above, use `go test -c -o out\convert.test.exe ./internal/convert` and run it directly as a workaround.

See [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.

## Terms of use

This project is not affiliated with or endorsed by Codebuff. FreeBuff free tier is an unofficial, moving target: quota, models, and endpoints change without notice, and the proxy may break at any time. Use at your own risk.

## License

[MIT](LICENSE).
