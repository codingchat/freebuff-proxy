# Security Policy

## Supported versions

This project is a single-binary tool developed as a community proxy for an
unofficial API. Only the latest commit on `main` (and its published release,
when one exists) is supported. There are no long-term support (LTS)
guarantees.

## Reporting a vulnerability

**Please do not open a public issue for security problems.**

- Preferred: use GitHub's **Private vulnerability reporting**
  (Security tab → "Report a vulnerability") — public repos have this enabled
  by default on github.com. It creates a private draft advisory.
- Alternative: email the maintainer if an email address is listed on your
  dashboard profile.

What to include:
- Affected surface (e.g. HTTP endpoint, config parsing, upstream request
  handling)
- Steps to reproduce (keep any FreeBuff auth tokens out of the report — use
  placeholders)
- Impact and, if known, a suggested fix

## Scope

This proxy intentionally holds **FreeBuff auth tokens** (`AUTH_TOKENS`) and
may proxy arbitrary client requests to an upstream service. Treat anything
that could leak tokens, bypass client auth (`API_KEYS`), or be abused as an
open proxy as in scope.

Out of scope: the FreeBuff/Codebuff upstream service itself, and behavioral
"abuse" of the free tier (quota bypassing) — report those upstream.