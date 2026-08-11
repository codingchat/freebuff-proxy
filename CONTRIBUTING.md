# Contributing

Thanks for wanting to help! This project is a Go single-binary proxy; the
barrier to a good first contribution is low.

## Before you start

- Read the [README](README.md) first — it explains what the proxy does and
  how it is configured.
- The [9router integration guide](docs/guides/9router-integration.md) is the
  main public document. PRD, research notes and delivery tasks are
  **local-only dev docs**, gitignored on purpose — do not add new ones under
  `docs/` except public guides under `docs/guides/`.
- The proxy talks to an undocumented upstream API. Anything that changes the
  request envelope (`internal/stealth`, `internal/upstream`) deserves extra
  caution and tests against the mock upstream (`internal/testutil`).

## Setting up

- Go 1.26+ (see `go.mod`)
- No other runtime dependencies for the proxy itself. The token helper
  scripts (`scripts/`) need Node.js >= 22.

## Development loop

```bash
go build ./...          # compiles the binary
go vet ./...            # static checks
go test ./...           # tests run against the mock upstream, no token needed
```

CI runs additionally `go test -race ./...`, `go mod verify`, and (once
enabled) lint — make sure your change passes those before pushing.

> Note for Windows devs: Kaspersky (and some other AVs) can quarantine
> freshly linked test binaries out of the go-build cache, failing `go test`
> with `fork/exec ... Access is denied`. That is a known false positive; use
> `go test -c -o <out>.exe ./internal/convert` and run the binary directly as
> a workaround.

## Style

- Run `gofmt` (or `gofmt -w`) on everything you touch. Go code uses tabs.
- Keep functions small and testable; this codebase values simple, deep
  modules over abstractions.
- Tests live next to the package: `package_test.go` → `package.go`.

## Commit convention

Conventional commits, matching the repo history:

- `fix(scope): ...` — bug fix
- `feat(scope): ...` — new functionality
- `chore(scope): ...` — maintenance, tooling, deps, docs meta
- `docs: ...` — documentation changes only

Keep commits focused; one logical change per commit.

## Pull requests

- Describe what and why — link an issue when one exists.
- Make sure the checklist in the PR template is complete.
- Never put real FreeBuff auth tokens, `.env` files, or `config.json` content
  in a PR. These are gitignored — keep them that way.
- CI must be green. If CI is red for a reason unrelated to your change, say
  so in the PR description.

## Ideas welcome

Open an issue for feature requests, or a discussion for questions. Before
implementing something large, ask — the upstream API changes often, and some
ideas (e.g. quota-related behavior) land better as config options than as
default behavior.