# fp-bridge (Free-Agent AI Proxy Gateway)

[![CI](https://img.shields.io/github/actions/workflow/status/trefeon/freebuff-proxy/ci.yml)](https://github.com/trefeon/freebuff-proxy/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/releases)
[![License](https://img.shields.io/github/license/trefeon/freebuff-proxy)](https://github.com/trefeon/freebuff-proxy/blob/main/LICENSE)

An OpenAI-compatible high-performance gateway and bridge for coding assistant backends. Connect any standard OpenAI client (Cursor, Continue, aider, OpenCode, 9router, OmniRouter, LiteLLM) to upstream free AI agent models with built-in token pooling, session lifecycle management, and TLS stealth.

> **Universal Coding Gateway Architecture.**
> The proxy replicates the official CLI request envelope (including system identity headers, metadata context, model-bound sessions, tool schema normalization, and browser JA3 TLS stealth). Direct OpenAI chat completions and SSE streaming are supported end-to-end.

---

## Features

- **OpenAI-Compatible API**: Serves `/v1/chat/completions`, `/v1/models`, `/healthz`, and Prometheus `/metrics` on `127.0.0.1:3457`.
- **Dynamic Reasoning Effort**: Full support for OpenAI `reasoning_effort` (`low`, `medium`, `high`, `max`) mapped directly to upstream reasoning engines.
- **Session & Run Lifecycle**: Manages upstream session handshakes, model locking recovery (`DELETE` → re-`POST`), and grace draining automatically.
- **Token Pooling & Bridge Mode**:
  - **Pooled Mode**: Comma-separate tokens in `AUTH_TOKENS=tok1,tok2` with automatic round-robin and error failover.
  - **Bridge Mode**: Zero-storage relay when `AUTH_TOKENS` is empty — each client or router sends its own token via `Authorization: Bearer <token>`.
- **TLS Stealth & Egress Proxies**: Supports `HTTP_PROXY`, `SOCKS5_PROXY`, per-token SOCKS5 routing, and browser TLS fingerprinting (Chrome, Firefox, Safari).
- **Subagent-Ready Concurrency**: Single-flight session refresh prevents race conditions during high-volume tool-calling loops.
- **Safe Mode**: Built-in rate limiting and jitter presets to protect upstream account standing.

---

## Architecture Overview

```mermaid
graph TD
    Client[AI Client / Router<br/>OpenCode, 9router, Continue, Cursor] -->|POST /v1/chat/completions| Proxy[fp-bridge<br/>localhost:3457]
    Proxy -->|1. Session & Run Lifecycle| Pool[Token Pool & Session Cache]
    Proxy -->|2. Inject Envelope + Stealth| Upstream[Upstream Backend API]
    Upstream -->|3. SSE Stream| Proxy
    Proxy -->|4. OpenAI SSE Chunks| Client
```

---

## Quick Start

### 1. Configure

Copy the example configuration:

```bash
cp .env.example .env
```

### 2. Obtain an Auth Token

Generate a token using the headless authentication helper (opens a browser login, prints the token without saving):

**Windows (PowerShell):**
```powershell
.\scripts\gen-freebuff-token.ps1 -ToClipboard
```

**Linux / macOS (bash):**
```bash
./scripts/gen-freebuff-token.sh --clipboard
```

Paste the token into `.env` under `AUTH_TOKENS=...` (or add it directly to your router as a bearer token).

### 3. Run with Docker Compose

```bash
docker compose up -d
```

Check health:
```bash
curl http://127.0.0.1:3457/healthz
```

---

## Client Integration Examples

### OpenCode + 9Router Integration (`~/.config/opencode/opencode.jsonc`)

When routing OpenCode through **9router** to access `fp-bridge`:

```json
{
  "model": "9router/freebuff/deepseek/deepseek-v4-flash",
  "small_model": "9router/freebuff/deepseek/deepseek-v4-flash",
  "provider": {
    "9router": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "9Router",
      "options": {
        "baseURL": "http://127.0.0.1:20128/v1"
      },
      "models": {
        "freebuff/deepseek/deepseek-v4-flash": {
          "id": "freebuff/deepseek/deepseek-v4-flash",
          "name": "Freebuff | DeepSeek V4 Flash",
          "reasoning": true,
          "tool_call": true,
          "cost": { "input": 0, "output": 0 },
          "limit": { "context": 1000000, "output": 384000 },
          "modalities": { "input": ["text"], "output": ["text"] },
          "variants": {
            "low": { "reasoningEffort": "low" },
            "high": { "reasoningEffort": "high" },
            "max": { "reasoningEffort": "max" }
          }
        },
        "freebuff/mimo/mimo-v2.5": {
          "id": "freebuff/mimo/mimo-v2.5",
          "name": "Freebuff | MiMo 2.5",
          "reasoning": true,
          "tool_call": true,
          "cost": { "input": 0, "output": 0 },
          "limit": { "context": 1000000, "output": 128000 },
          "modalities": {
            "input": ["text", "image", "video", "audio"],
            "output": ["text"]
          },
          "variants": {
            "none": { "reasoningEffort": "none" },
            "high": { "reasoningEffort": "high" }
          }
        }
      }
    }
  }
}
```

### 9router Provider Setup (Bridge Mode)

1. In the **9router Dashboard** (`http://127.0.0.1:20128`):
   - Go to **Providers** $\rightarrow$ **Add Provider** $\rightarrow$ select **OpenAI Compatible**.
   - **Name**: `freebuff`
   - **Prefix**: `freebuff`
   - **Base URL**: `http://127.0.0.1:3457/v1` (or your Docker host IP)
   - **API Keys**: Add your upstream auth token(s) as keys (each key acts as a pooled upstream account in bridge mode).
2. OpenCode connects to 9router at `http://127.0.0.1:20128/v1`, and 9router load-balances across tokens on `fp-bridge`.
---

## Configuration Reference

| Environment Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:3457` | Port and host address to bind |
| `AUTH_TOKENS` | `""` | Comma-separated list of upstream tokens (empty = bridge mode) |
| `SAFE_MODE` | `true` | Enables conservative message limits and request jitter |
| `MAX_MESSAGES_PER_DAY` | `150` | Daily request ceiling per token |
| `SOCKS5_PROXY` | `""` | Outbound proxy for upstream requests |
| `TLS_FINGERPRINT` | `auto` | TLS profile: `auto`, `chrome126`, `firefox128`, `safari18` |
| `LOG_LEVEL` | `info` | Log verbosity: `debug`, `info`, `warn`, `error` |

---

## License

[MIT](LICENSE)
