#!/usr/bin/env bash
# install-freebuff-proxy.sh - download the latest freebuff-proxy release for
# this machine, verify it, set up .env, and print the next steps.
#
# Zero-knowledge user flow:
#   1. Open a terminal
#   2. Run:
#        curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash
#   3. Read what it prints. It downloads the binary, creates .env from the
#      example, and either pulls your token from the freebuff CLI login or
#      asks you to paste it (a login URL or just the auth_code).
#
# What it does NOT do: modify system paths, install services, or touch your
# token except writing it into the local .env (gitignored).
set -euo pipefail

REPO="trefeon/freebuff-proxy"
DIR=""
SKIP_TOKEN=0
NO_ENV=0
ENV_FILE=""

for arg in "$@"; do
  case "$arg" in
    --dir=*) DIR="${arg#*=}" ;;
    --dir) DIR="${2:-}"; shift ;;
    --skip-token) SKIP_TOKEN=1 ;;
    --no-env) NO_ENV=1 ;;
    --env-file=*) ENV_FILE="${arg#*=}" ;;
    --env-file) ENV_FILE="${2:-}"; shift ;;
    -h|--help) grep '^#' "$0" | head -30; exit 0 ;;
    *) echo "unknown arg: $arg (see header)" >&2; exit 1 ;;
  esac
  shift
done

c() { printf '\033[36m%s\033[0m\n' "$*"; }
ok() { printf '\033[32m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }

c "Installing freebuff-proxy (latest release)..."
echo ""

# --- 0. warning -------------------------------------------------------------
echo ""
echo "WARNING: using your FreeBuff token through this proxy conflicts with FreeBuff/Codebuff" >&2
echo "terms of service. Accounts get suspended or banned (403 account_banned, dashboard shows" >&2
echo "'suspended'). Bans are per account, usually permanent, and there is no self-service" >&2
echo "unban. Use ONE account, keep usage modest, do not run the proxy 24/7, and expect the" >&2
echo "account to be banned eventually. You accept this risk by continuing." >&2
echo "" >&2

# --- 1. target directory -----------------------------------------------------
if [ -z "$DIR" ]; then DIR="$(pwd)"; fi
mkdir -p "$DIR"

# --- 2. resolve the latest release ------------------------------------------
RELEASE="$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest")"
VERSION="$(printf '%s' "$RELEASE" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1 | sed 's/^v//')"  # assets use 0.1.1, tag is v0.1.1
if [ -z "$VERSION" ]; then
  echo "ERROR: could not resolve the latest release from GitHub." >&2
  exit 1
fi
ok "Latest release: v$VERSION"

OS="$(uname -s)"
case "$OS" in
  Linux*) GOOS="linux" ;;
  Darwin*) GOOS="darwin" ;;
  *) echo "ERROR: unsupported OS: $OS" >&2; exit 1 ;;
esac
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) GOARCH="amd64" ;;
  aarch64|arm64) GOARCH="arm64" ;;
  *) echo "ERROR: unsupported arch: $ARCH" >&2; exit 1 ;;
esac

ASSET="freebuff-proxy_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
ok "Asset: $ASSET"

ASSET_URL="$(printf '%s' "$RELEASE" | tr ',' '\n' | sed -n "s/.*\"browser_download_url\": *\"\([^\"]*${ASSET}\"\).*/\1/p" | head -1 | tr -d '"')"
SUMS_URL="$(printf '%s' "$RELEASE" | tr ',' '\n' | sed -n 's/.*"browser_download_url": *"\([^"]*checksums.txt"\).*/\1/p' | head -1)"
if [ -z "$ASSET_URL" ]; then
  echo "ERROR: asset $ASSET not found in the release." >&2
  exit 1
fi

# --- 3. download + verify ----------------------------------------------------
TMP="$(mktemp -d)"
echo "Downloading..."
curl -sSL -o "$TMP/$ASSET" "$ASSET_URL"
curl -sSL -o "$TMP/checksums.txt" "$SUMS_URL"

HASH_FILE="$(sha256sum "$TMP/$ASSET" | awk '{print $1}')"
EXPECTED="$(sed -n "s/^\([a-f0-9]*\)  .*${ASSET}$/\1/p" "$TMP/checksums.txt" | head -1)"
if [ -z "$EXPECTED" ]; then
  EXPECTED="$(awk -v a="$ASSET" '$2==a {print $1}' "$TMP/checksums.txt" | head -1)"
fi
if [ "$HASH_FILE" != "$EXPECTED" ]; then
  echo "ERROR: checksum mismatch for $ASSET" >&2
  echo "  expected: $EXPECTED" >&2
  echo "  actual:   $HASH_FILE" >&2
  exit 1
fi
ok "Checksum OK."

# --- 4. extract --------------------------------------------------------------
tar xzf "$TMP/$ASSET" -C "$DIR"
BIN="$DIR/freebuff-proxy"
if [ ! -x "$BIN" ]; then
  # goreleaser wraps into a subdir when not set; find it.
  BIN="$(find "$DIR" -type f -name freebuff-proxy | head -1)"
fi
if [ -n "$BIN" ]; then chmod +x "$BIN"; fi
c "Binary: $BIN"

# --- 5. .env -----------------------------------------------------------------
ENVPATH="$ENV_FILE"
if [ -z "$ENVPATH" ]; then ENVPATH="$DIR/.env"; fi
if [ "$NO_ENV" = "0" ]; then
  EXAMPLE="$DIR/.env.example"
  if [ ! -f "$ENVPATH" ]; then
    if [ -f "$EXAMPLE" ]; then
      cp "$EXAMPLE" "$ENVPATH"
      ok ".env created from .env.example"
    else
      printf '# freebuff-proxy config\n' > "$ENVPATH"
      ok ".env created (minimal)"
    fi
  else
    ok ".env already exists, leaving it as is"
  fi
fi

# --- 6. token ----------------------------------------------------------------
if [ "$SKIP_TOKEN" = "0" ]; then
  TOKEN=""
  CREDS=""
  for p in "$HOME/.config/manicode/credentials.json" "$HOME/.config/codebuff/credentials.json"; do
    [ -f "$p" ] && CREDS="$p" && break
  done
  if [ -n "$CREDS" ]; then
    if command -v node >/dev/null 2>&1; then
      TOKEN="$(node -e '
        const fs = require("fs");
        const data = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
        const acct = data.default || Object.values(data)[0];
        if (acct && acct.authToken) process.stdout.write(acct.authToken);
      ' "$CREDS")"
    fi
    if [ -z "$TOKEN" ] && command -v python3 >/dev/null 2>&1; then
      TOKEN="$(python3 -c '
        import json, sys
        d = json.load(open(sys.argv[1], encoding="utf-8"))
        acct = d.get("default") or next(iter(d.values()), {})
        t = acct.get("authToken") if isinstance(acct, dict) else None
        if t: sys.stdout.write(t)
      ' "$CREDS")"
    fi
  fi
  if [ -n "$TOKEN" ] && [ "${#TOKEN}" -gt 12 ]; then
    if grep -q '^AUTH_TOKENS=' "$ENVPATH" 2>/dev/null; then
      sed -i "s|^AUTH_TOKENS=.*|AUTH_TOKENS=$TOKEN|" "$ENVPATH"
    else
      printf 'AUTH_TOKENS=%s\n' "$TOKEN" >> "$ENVPATH"
    fi
    ok "Token found from your freebuff CLI login; AUTH_TOKENS written to $ENVPATH"
  else
    warn "No freebuff CLI token found."
    warn "Get one from the web page, then paste it here (login URL or just the auth_code):"
    printf '> '
    read -r PASTED
    if [ -n "$PASTED" ]; then
      if [[ "$PASTED" =~ auth_code=([^&[:space:]]+) ]]; then
        PASTED="${BASH_REMATCH[1]}"
      fi
      if [ "${#PASTED}" -gt 8 ]; then
        if grep -q '^AUTH_TOKENS=' "$ENVPATH" 2>/dev/null; then
          sed -i "s|^AUTH_TOKENS=.*|AUTH_TOKENS=$PASTED|" "$ENVPATH"
        else
          printf 'AUTH_TOKENS=%s\n' "$PASTED" >> "$ENVPATH"
        fi
        ok "AUTH_TOKENS written to $ENVPATH"
      else
        warn "That does not look like a token; skipped."
      fi
    else
      warn "Skipped. To add the token manually:"
      warn "  1. Log in at https://freebuff.llm.pm, Freebuff Auth -> Generate login URL"
      warn "  2. Copy the URL it shows (https://freebuff.com/login?auth_code=...)"
      warn "  3. Edit $ENVPATH and set:  AUTH_TOKENS=<the auth_code value from that link>"
    fi
  fi
fi

# --- 7. next steps -----------------------------------------------------------
echo ""
c "Done. Next:"
echo "  cd $DIR"
echo "  ./$BIN"
echo ""
c "Then check it:"
echo "  curl http://localhost:3457/healthz"
echo "  curl http://localhost:3457/v1/models"
echo ""
c "See the README (in the archive) for the full guide and 9router wiring."