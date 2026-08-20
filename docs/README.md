# Documentation

Public documentation for freebuff-proxy. This folder is committed to the
repository; local-only development study (reverse-engineering notes, plans,
research) lives in the ignored `devdocs/` directory and is not part of the
public repo.

## Guides

| Guide | What it covers |
|---|---|
| [Getting Started](getting-started.md) | 5-minute onboarding: get a FreeBuff token, install, pooled vs bridge mode, verify the proxy, connect your AI client, troubleshoot common 403/502 errors |
| [Client Integration](client-integration.md) | Config snippets for OpenAI-compatible clients: opencode, pi, Python/Node SDKs, Cursor, VS Code extensions, chat UIs, API routers |
| [9router Integration](9router-integration.md) | Wire the proxy into 9router as a custom OpenAI-compatible provider |
| [Model Translation Layer](model-translation-layer.md) | The `/v1/models` catalog and how model ids and reasoning effort translate to the upstream FreeBuff wire protocol |
| [Dashboard Guide](dashboard.md) | The embedded admin web UI: access, pages, Docker caveats, hardening |
| [Manual Testing](testing.md) | Step-by-step verification runbook (Linux and Windows), mirroring the CI checks |
| [Version Stability & Ban Findings](version-stability-and-ban-findings.md) | **Read before upgrading** — which versions are proven-stable in production and what caused account bans |

## Related

- [README](../README.md): overview, quick start, full config reference
- [CONTRIBUTING](../CONTRIBUTING.md): how to contribute to this repository