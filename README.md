# freebuff-proxy

[![CI](https://img.shields.io/github/actions/workflow/status/trefeon/freebuff-proxy/ci.yml)](https://github.com/trefeon/freebuff-proxy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/releases)
[![License](https://img.shields.io/github/license/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/blob/main/LICENSE)

An OpenAI-compatible proxy bridge for the FreeBuff free tier. Point any OpenAI client at it and it talks to FreeBuff for you, with token pooling and session management built in.

FreeBuff (Codebuff's free coding agent) exposes its models only through the official CLI. The backend fingerprints CLI traffic and rejects direct API calls with `403 free_mode_cli_required`. freebuff-proxy replicates the CLI request envelope, manages the free-session and agent-run lifecycle upstream, and pools multiple tokens. Clients see a plain OpenAI-compatible API.

What this is not: an official FreeBuff or Codebuff product. It is a community bridge for an unofficial service. See the FAQ and Terms of use at the bottom.

> ## WARNING: your account can get suspended or banned
>
> This project works by making FreeBuff believe it is talking to the official CLI. The
> upstream service detects this and **does suspend and ban accounts**.
>
> - Suspended/banned accounts fail with `403 account_banned` / `{"status":"banned"}`, and
>   the web dashboard shows **"suspended"**. Your FreeBuff/Codebuff account, tokens, and
>   free-tier access are on the line.
> - The ban is **per account and effectively terminal**. The official source code flags
>   the account as banned ("terminal", returned from every endpoint). Unbanning is an
>   internal admin operation; there is no self-service path. Community proxies have seen
>   `resumes_at` timestamps in ban responses, which would mean temporary bans, but this is
>   **not confirmed in any official source** and may just be the account being gone.
> - Bans are scored by a public abuse-detection pipeline: heavy continuous usage (hundreds
>   of messages a day, many distinct active hours, long unattended sessions), automation
>   patterns, fresh GitHub accounts (under a few weeks old), throwaway email addresses,
>   and clusters of new accounts created close together all raise the score.
> - Codebuff's terms allow one account per person and explicitly prohibit wrappers,
>   proxies, and non-human sessions. Using this proxy already conflicts with them.
>
> **Use at your own risk, and assume a ban is permanent.**
>
> - Use one modest account; do not run 24/7, do not leave sessions running unattended,
>   stop when you see `429 rate_limited`.
> - If you are banned: the token is dead. Wait and re-probe once (cheap to try), then get
>   a **new account with an established GitHub login (months old, not fresh) and a clean
>   IP, without a VPN**. That is the only realistic recovery.
> - Appeals go to support@codebuff.com and realistically only succeed for false positives,
>   not for proxy use. The maintainers have had accounts suspended while building and
>   testing this project. This is not a toy warning.

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

### Option 1: one-command installer (recommended)

Downloads the **latest** release binary for your platform, verifies its checksum, sets up
`.env`, asks for your token, and prints the next steps. No version to look up, no manual
downloads.

**Windows (PowerShell):**

```powershell
irm https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.ps1 | iex
```

**Linux / macOS (bash):**

```bash
curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash
```

Both scripts install into the current directory (`--dir <path>` to change it), create
`AUTH_TOKENS` in `.env` from your freebuff CLI login, or prompt you to paste a login URL
(`https://freebuff.com/login?auth_code=...`), and print the run and smoke-test commands.

### Option 2: manual download

Download the archive for your platform from the [latest release](https://github.com/trefeon/freebuff-proxy/releases). Assets are named `freebuff-proxy_<version>_<os>_<arch>.tar.gz` (`zip` on Windows), and every release ships `checksums.txt`.

| Platform | Archive |
|---|---|
| linux / amd64 | `freebuff-proxy_<version>_linux_amd64.tar.gz` |
| linux / arm64 | `freebuff-proxy_<version>_linux_arm64.tar.gz` |
| macOS / amd64 | `freebuff-proxy_<version>_darwin_amd64.tar.gz` |
| macOS / arm64 | `freebuff-proxy_<version>_darwin_arm64.tar.gz` |
| windows / amd64 | `freebuff-proxy_<version>_windows_amd64.zip` |
| windows / arm64 | `freebuff-proxy_<version>_windows_arm64.zip` |

Example on linux amd64 (replace `<version>`, e.g. `0.1.1`):

```bash
curl -sSL -o freebuff-proxy.tar.gz https://github.com/trefeon/freebuff-proxy/releases/latest/download/freebuff-proxy_<version>_linux_amd64.tar.gz
tar xzf freebuff-proxy.tar.gz
sha256sum -c checksums.txt --ignore-missing 2>/dev/null || echo "download checksums.txt from the release to verify"
./freebuff-proxy
```

Windows: extract the zip, then run `freebuff-proxy.exe` in a terminal.

### Option 3: Docker

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

- **Web (no install):** log in at [freebuff.llm.pm](https://freebuff.llm.pm). Under
  **Freebuff Auth** click **Generate login URL**, then **copy** the URL it shows
  (`https://freebuff.com/login?auth_code=...`). The token is the `auth_code` value from
  that link, e.g. from `...?auth_code=4v2G-8dmPXNjgZvbCvhIcA` the token is
  `4v2G-8dmPXNjgZvbCvhIcA`. Pasting the whole URL also works in the scripts below.
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
| `COST_MODE` | `free` | Mode sent upstream with chat requests. Must be `free`: the upstream 402 balance check runs only when `cost_mode != "free"`, so omitting it makes fresh free-tier accounts fail with `402 "Out of credits. Please add credits at codebuff.com/usage"`. |
| `DEBUG_DUMP` | `false` | Dump raw upstream traffic into `./dump` (sensitive headers redacted). |
| `LOG_FILE` | empty | Append logs to a file in addition to stderr. |
| `LOG_LEVEL` | info | `debug`, `info`, `warn`, or `error`. `-v` implies debug; `LOG_LEVEL` wins. |
| `TLS_FINGERPRINT` | empty | Outbound JA3 fingerprint: `chrome120`, `safari17`, `firefox120`, or `random`. |
| `MAX_MESSAGES_PER_DAY` | `0` | Per-token rolling 24h message cap. At the cap the proxy answers `429 rate_limited` with `Retry-After` instead of hitting upstream, keeping the account far under FreeBuff's abuse thresholds (~500 msgs/24h). `0` = unlimited. |
| `IDLE_ROTATION_TIMEOUT` | `0` | Pause background work after this long without traffic (e.g. `30m`): runs are FINISHed and maintenance stops until the next request, so the account is not kept artificially active 24/7. `0` = always maintain. |

## 9router integration

Add freebuff-proxy as an OpenAI-compatible custom provider in 9router. The step-by-step guide covers the dashboard form, model catalog, verification, and troubleshooting: [docs/guides/9router-integration.md](docs/guides/9router-integration.md).

Quick version: Dashboard, Providers, Add OpenAI Compatible. Base URL `http://localhost:3457/v1`, API Type Chat Completions, any non-empty API key, and the model ids come from `/v1/models`. Model combos become `freebuff/<model-id>`.

## Docs

- [9router integration guide](docs/guides/9router-integration.md): full wiring, model catalog, troubleshooting.
- The other project docs (PRD, research notes, delivery tasks, security notes) are local-only dev docs, gitignored on purpose. They do not ship with the repo.

## FAQ

**The proxy returns `403 free_mode_cli_required`.**

The CLI envelope did not satisfy the anti-bot gate. Check `COST_MODE` (must be `free`, the default) and the `client_id` format (13-char base36). If it started failing after a FreeBuff update, open an issue with the debug log (`LOG_LEVEL=debug`).

**I get `402` / "Out of credits. Please add credits at codebuff.com/usage".**

The request went down the paid path. Upstream runs its balance check only when `cost_mode != "free"`, so a fresh free account (balance 0) always gets 402 unless `COST_MODE=free` is sent. Check your `.env`: `COST_MODE` must be `free` (the default and the value in `.env.example`). If it is empty, the proxy bills the request as paid. Old configs copied before v0.2.0 that set `COST_MODE=` empty need the value restored.

**I get `429` with `rate_limited` in the body.**

The token's daily session quota is exhausted (6 sessions per day on the limited tier, resets at Pacific midnight). The proxy returns `429` with the upstream `resetAt` so clients back off. Add another `AUTH_TOKENS` or wait for the reset.

**I get `403` with `account_banned` / `{"status":"banned"}`.**

Your FreeBuff account was banned or suspended upstream. See the **WARNING** at the top of
this file: the ban is per account and effectively permanent, the token is dead, and no
setting will revive it. The proxy stops using the token during the ban window (upstream
`resumes-at`, or 24h if none) and then re-probes once, which is cheap to try; if it still
fails, get a new account with an established GitHub login and a clean IP (no VPN). Appeals
go to support@codebuff.com but realistically only succeed for false positives.

**I get `503` with `waiting_room_queued`.**

Normal. The free session is queued in the waiting room. The `Retry-After` header tells the client when to retry; 9router and opencode retry automatically.

**Windows Defender or Kaspersky flags the binary or test executables.**

Go binaries trip AV heuristics; this is a validated false positive, not malware. For local `go test`, add the `go-build*` cache path to AV exclusions or run the compiled test binary directly. Details in the repo history; open an issue if you see a signature match (we have never seen one).

**Is this against FreeBuff's terms?**

FreeBuff is intended to be used through the official CLI only. This proxy uses undocumented endpoints and replicates CLI fingerprints, which conflicts with the letter of the service terms. Account bans are possible. Use it for personal and educational experimentation, keep usage modest, at your own risk.

**How do I keep my account from getting banned?**

Use less, use it like a human, and let the proxy do the same. Set
`MAX_MESSAGES_PER_DAY` (well under the ~500 msgs/24h threshold, e.g. `150`) and
`IDLE_ROTATION_TIMEOUT` (e.g. `30m`) so the proxy stops background work when you are not
using it; do not run it 24/7, stop when you see `429 rate_limited`, and never share a
token between the proxy, the official CLI, and the web dashboard at the same time. See
the WARNING at the top of this file and the Terms of use.

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
