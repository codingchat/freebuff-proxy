#!/usr/bin/env bash
# install-freebuff-proxy.sh - interactive easy-mode installer for freebuff-proxy.
#
# Flow (curl | bash compatible): reads prompts from the controlling terminal
# (/dev/tty), so it works even when piped:
#     curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash
#
# Menu: (1) Easy install (recommended)  (2) Manual binary  (3) Docker Compose
#       (4) Bridge mode (clients bring their own FreeBuff token)
#
# Non-interactive flags (scripted use):
#   --dir=<path> / --dir <path>   target directory (default: current dir)
#   --skip-token                  do not touch AUTH_TOKENS
#   --no-env                      do not create/update .env
#   --force                       re-download the binary even if present
#   --env-file=<path>             write .env to <path>
#   --method=binary|docker|bridge skip the menu
#   --no-prompt                   safe defaults, never read the terminal
#   -h|--help                     this header
#
# What it does NOT do: modify system paths, install services, or touch your
# token except writing it into the local .env (gitignored).
set -euo pipefail

REPO="trefeon/freebuff-proxy"
DIR=""
SKIP_TOKEN=0
NO_ENV=0
FORCE=0
ENV_FILE=""
METHOD=""
NO_PROMPT=0

while [ $# -gt 0 ]; do
  case "$1" in
    --dir=*) DIR="${1#*=}"; shift ;;
    --dir) DIR="${2:-}"; shift 2 ;;
    --skip-token) SKIP_TOKEN=1; shift ;;
    --no-env) NO_ENV=1; shift ;;
    --force) FORCE=1; shift ;;
    --env-file=*) ENV_FILE="${1#*=}"; shift ;;
    --env-file) ENV_FILE="${2:-}"; shift 2 ;;
    --method=*) METHOD="${1#*=}"; shift ;;
    --method) METHOD="${2:-}"; shift 2 ;;
    --no-prompt) NO_PROMPT=1; shift ;;
    -h|--help) grep '^#' "$0" | head -40; exit 0 ;;
    *) echo "unknown arg: $1 (see header)" >&2; exit 1 ;;
  esac
done

c() { printf '\033[36m%s\033[0m\n' "$*"; }
ok() { printf '\033[32m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }
die() { printf '\033[31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

# --- interactive input -------------------------------------------------------
# curl|bash pipes the script on stdin, so prompts MUST come from /dev/tty.
TTY_FD=3
if ! (exec 3< /dev/tty) 2>/dev/null; then
  if [ "$NO_PROMPT" = "0" ]; then
    echo ""
    echo "ERROR: no interactive terminal detected. Options:" >&2
    echo "  1. Run the script from a real terminal (download it first):" >&2
    echo "       curl -sSL -o install.sh https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh" >&2
    echo "       bash install.sh" >&2
    echo "  2. Run non-interactively with defaults:" >&2
    echo "       curl -sSL https://raw.githubusercontent.com/trefeon/freebuff-proxy/main/scripts/install-freebuff-proxy.sh | bash -s -- --no-prompt" >&2
    exit 1
  fi
  TTY_FD=0
fi

# ask <var> <prompt> [<default>] — reads one line from the terminal.
ask() {
  local __var="$1" __prompt="$2" __default="${3:-}" __answer=""
  printf '%s' "$__prompt"
  [ -n "$__default" ] && printf ' [%s]' "$__default"
  printf ': '
  read -r -u "$TTY_FD" __answer || true
  __answer="${__answer:-$__default}"
  eval "$__var=\"\$__answer\""
}

# menu <var> <title> <opt1> <label1> <opt2> <label2> ... <default>
menu() {
  local __var="$1" __title="$2" __default="${!#}" __answer=""
  shift 2
  echo ""
  c "$__title"
  local __n=1
  while [ $# -gt 1 ]; do
    echo "  $__n) $2"
    __n=$((__n + 1)); shift 2
  done
  printf 'Choose (default %s): ' "$__default"
  read -r -u "$TTY_FD" __answer || true
  __answer="${__answer:-$__default}"
  eval "$__var=\"\$__answer\""
}

# --- 0. warning --------------------------------------------------------------
echo ""
echo "WARNING: using your FreeBuff token through this proxy conflicts with FreeBuff/Codebuff" >&2
echo "terms of service. Accounts get suspended or banned (403 account_banned, dashboard shows" >&2
echo "'suspended'). Bans are per account, usually permanent, and there is no self-service" >&2
echo "unban. Use ONE account, keep usage modest, do not run the proxy 24/7, and expect the" >&2
echo "account to be banned eventually. You accept this risk by continuing." >&2
echo "" >&2

# --- 1. deployment method ----------------------------------------------------
if [ -z "$METHOD" ]; then
  if [ "$NO_PROMPT" = "1" ]; then
    METHOD="easy"
  else
    menu METHOD "How do you want to install freebuff-proxy?" \
      1 "Easy install (recommended) - binary + token + safety defaults, one flow" \
      2 "Manual binary - download the latest release, fine-grained choices" \
      3 "Docker Compose - run in a container on this host" \
      4 "Bridge mode - proxy only; each client sends their own FreeBuff token" \
      "1"
  fi
fi
BRIDGE=0
case "$METHOD" in
  1|easy) METHOD="easy" ;;
  2|binary) METHOD="binary" ;;
  3|docker) METHOD="docker" ;;
  4|bridge) METHOD="binary"; BRIDGE=1 ;;
  *) die "unknown method: $METHOD (binary|docker|bridge)" ;;
esac
if [ "$BRIDGE" = "1" ]; then
  warn "Bridge mode: the proxy will NOT store any token. Clients must send their own"
  warn "FreeBuff token as 'Authorization: Bearer <token>' on every chat request."
fi

# --- 2. target directory ------------------------------------------------------
if [ -z "$DIR" ]; then DIR="$(pwd)"; fi
mkdir -p "$DIR"

# --- 3. dependencies (binary path only; docker needs docker + compose) --------
if [ "$METHOD" = "docker" ]; then
  for bin in docker; do
    command -v "$bin" >/dev/null 2>&1 || die "$bin not found — install Docker first (https://docs.docker.com/engine/install/)."
  done
  docker compose version >/dev/null 2>&1 || die "docker compose v2 not found — install the compose plugin."
  ok "Docker + Compose present."
else
  NEEDED=""
  for bin in curl tar sha256sum; do
    command -v "$bin" >/dev/null 2>&1 || NEEDED="$NEEDED $bin"
  done
  if [ -n "$NEEDED" ]; then
    warn "Missing dependencies:$NEEDED — installing via your package manager..."
    if command -v apt-get >/dev/null 2>&1; then
      sudo apt-get update -qq && sudo apt-get install -y -qq curl tar coreutils
    elif command -v dnf >/dev/null 2>&1; then
      sudo dnf install -y curl tar coreutils
    elif command -v yum >/dev/null 2>&1; then
      sudo yum install -y curl tar coreutils
    elif command -v apk >/dev/null 2>&1; then
      sudo apk add --no-cache curl tar
    elif command -v brew >/dev/null 2>&1; then
      brew install curl gnu-tar coreutils
    else
      die "no supported package manager found. Install curl, tar and coreutils manually, then re-run."
    fi
    for bin in curl tar sha256sum; do
      command -v "$bin" >/dev/null 2>&1 || die "$bin still missing after install."
    done
  fi
  ok "Dependencies OK (curl, tar, sha256sum)"
  if ! command -v node >/dev/null 2>&1; then
    warn "node not found — token auto-extraction from the freebuff CLI login will fall back to python3 or manual paste (fine for most users)."
  fi
fi

# --- 4. .env (used by both binary and docker paths) ---------------------------
ENVPATH="$ENV_FILE"
if [ -z "$ENVPATH" ]; then ENVPATH="$DIR/.env"; fi
env_setup() {
  [ "$NO_ENV" = "1" ] && return
  if [ -f "$ENVPATH" ]; then
    if [ "$FORCE" = "1" ]; then
      warn "Overwriting $ENVPATH (--force)."
      cp "$DIR/.env.example" "$ENVPATH" 2>/dev/null || printf '# freebuff-proxy config\n' > "$ENVPATH"
    else
      ok ".env already exists, leaving it as is"
      return
    fi
  elif [ -f "$DIR/.env.example" ]; then
    cp "$DIR/.env.example" "$ENVPATH"
    ok ".env created from .env.example"
  else
    printf '# freebuff-proxy config\n' > "$ENVPATH"
    ok ".env created (minimal)"
  fi
}
env_setup

# set_env <key> <value> — replace or append a key in the .env file.
set_env() {
  local key="$1" val="$2"
  if grep -q "^$key=" "$ENVPATH" 2>/dev/null; then
    sed -i "s|^$key=.*|$key=$val|" "$ENVPATH"
  else
    printf '%s=%s\n' "$key" "$val" >> "$ENVPATH"
  fi
}

# --- 5. token (skipped in bridge mode / --skip-token) -------------------------
if [ "$BRIDGE" = "1" ]; then
  if grep -q '^AUTH_TOKENS=' "$ENVPATH" 2>/dev/null; then
    warn "Bridge mode: emptying AUTH_TOKENS in $ENVPATH (clients send their own token)."
    sed -i 's|^AUTH_TOKENS=.*|AUTH_TOKENS=|' "$ENVPATH"
  else
    printf '# Bridge mode: empty AUTH_TOKENS — clients send their own FreeBuff token\nAUTH_TOKENS=\n' >> "$ENVPATH"
  fi
  ok "Bridge mode configured: AUTH_TOKENS empty."
elif [ "$SKIP_TOKEN" = "0" ] && [ "$NO_ENV" = "0" ]; then
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
    set_env "AUTH_TOKENS" "$TOKEN"
    ok "Token found from your freebuff CLI login; AUTH_TOKENS written to $ENVPATH"
  else
    if [ "$NO_PROMPT" = "1" ]; then
      warn "No token and --no-prompt: leaving AUTH_TOKENS as-is. Chat will not work until it is set."
    else
      warn "No freebuff CLI token found."
      warn "Get one from the web page, then paste it here (login URL or just the auth_code):"
      printf '> '
      read -r -u "$TTY_FD" PASTED || true
      if [ -n "$PASTED" ]; then
        if [[ "$PASTED" =~ auth_code=([^&[:space:]]+) ]]; then
          PASTED="${BASH_REMATCH[1]}"
        fi
        if [ "${#PASTED}" -gt 8 ]; then
          set_env "AUTH_TOKENS" "$PASTED"
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
fi

# --- 6. safety knobs (easy mode default; others ask too, Enter = keep) --------
if [ "$NO_ENV" = "0" ]; then
  KNOB_MAX="${MAX_KNOB:-}"
  KNOB_IDLE="${IDLE_KNOB:-}"
  if [ "$NO_PROMPT" = "0" ]; then
    if [ "$METHOD" = "easy" ] || [ "$METHOD" = "binary" ]; then
      echo ""
      c "Account-safety knobs (recommended to keep your account alive):"
      ask KNOB_MAX "Max messages per token per 24h" "150"
      ask KNOB_IDLE "Pause background work after idle (e.g. 30m, 0 = never)" "30m"
    fi
  else
    KNOB_MAX="150"; KNOB_IDLE="30m"
  fi
  if [ -n "$KNOB_MAX" ]; then
    set_env "MAX_MESSAGES_PER_DAY" "$KNOB_MAX"
    ok "MAX_MESSAGES_PER_DAY=$KNOB_MAX"
  fi
  if [ -n "$KNOB_IDLE" ]; then
    set_env "IDLE_ROTATION_TIMEOUT" "$KNOB_IDLE"
    ok "IDLE_ROTATION_TIMEOUT=$KNOB_IDLE"
  fi
fi

# --- 7. install ---------------------------------------------------------------
if [ "$METHOD" = "docker" ]; then
  # --- docker compose path (mirrors scripts/setup-proxy-docker.sh) ------------
  SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
  if [ -f "$SCRIPT_DIR/../docker-compose.yml" ]; then
    REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
  else
    REPO_DIR="$HOME/freebuff-proxy"
  fi
  if [ ! -f "$REPO_DIR/docker-compose.yml" ]; then
    c "Cloning freebuff-proxy into $REPO_DIR..."
    git clone --quiet https://github.com/$REPO.git "$REPO_DIR"
  fi
  cd "$REPO_DIR"
  [ -f .env ] || cp "$ENVPATH" .env 2>/dev/null || true
  c "Building the proxy image (this takes a minute the first time)..."
  docker compose up --build -d
  c "Waiting for the container to become healthy..."
  for i in $(seq 1 30); do
    STATUS="$(docker compose ps --format '{{.Status}}' 2>/dev/null | head -1)"
    case "$STATUS" in
      *healthy*) ok "Container healthy: $STATUS"; break ;;
      *) sleep 2 ;;
    esac
    [ "$i" = "30" ] && { echo "ERROR: container not healthy after 60s" >&2; docker compose logs --tail 20; exit 1; }
  done
  BRIDGE_GW="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || echo 172.17.0.1)"
  GATEWAY=""
  for net in $(docker network ls --format '{{.Name}}' | grep -i freebuff | head -3); do
    GATEWAY="$(docker network inspect "$net" --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null || true)"
    [ -n "$GATEWAY" ] && break
  done
  [ -z "$GATEWAY" ] && GATEWAY="$BRIDGE_GW"

  echo ""
  echo "============================================================"
  echo "  9router -> freebuff-proxy — fill the 'Add OpenAI Compatible'"
  echo "  form with these values (Dashboard -> Providers -> Add)"
  echo "============================================================"
  echo ""
  echo "  Name          : freebuff"
  echo "  Prefix        : freebuff"
  echo "  API Type      : Chat Completions          (NOT Responses API)"
  echo "  Base URL      : see below — depends on where 9router runs"
  echo "  API Key       : any non-empty value"
  echo "  Model ID      : (leave empty — the proxy has /v1/models)"
  echo ""
if [ "$BRIDGE" = "1" ] && [ "$NO_ENV" = "0" ]; then
    echo "  Bridge mode  : API Key = YOUR FreeBuff token (sent upstream as-is)"
  fi
  echo "  Base URL — pick ONE:"
  echo "    A) 9router as a process on this host: http://127.0.0.1:3457/v1"
  echo "    B) 9router in a container on this host: http://${GATEWAY}:3457/v1"
  echo ""
  echo "  Verify:  curl http://localhost:3457/v1/models"
  echo "============================================================"
else
  # --- release binary path -----------------------------------------------------
  c "Resolving the latest release..."
  RELEASE="$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest")"
  VERSION="$(printf '%s' "$RELEASE" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1 | sed 's/^v//')"
  [ -n "$VERSION" ] || die "could not resolve the latest release from GitHub (offline? blocked?)."
  ok "Latest release: v$VERSION"

  OS="$(uname -s)"
  case "$OS" in
    Linux*) GOOS="linux" ;;
    Darwin*) GOOS="darwin" ;;
    MINGW*|MSYS*|CYGWIN*) GOOS="windows" ;;
    *) die "unsupported OS: $OS" ;;
  esac
  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64|amd64) GOARCH="amd64" ;;
    aarch64|arm64) GOARCH="arm64" ;;
    *) die "unsupported arch: $ARCH" ;;
  esac
  if [ "$GOOS" = "windows" ]; then
    ASSET="freebuff-proxy_${VERSION}_${GOOS}_${GOARCH}.zip"
    [ -n "$(command -v unzip)" ] || die "unzip not found — install it (e.g. 'pacman -S unzip' in git-bash) or use the PowerShell installer."
  else
    ASSET="freebuff-proxy_${VERSION}_${GOOS}_${GOARCH}.tar.gz"
    [ -n "$(command -v tar)" ] || die "tar not found."
  fi
  ok "Asset: $ASSET"

  EXISTING_BIN="$(find "$DIR" -maxdepth 2 -type f -name 'freebuff-proxy*' -perm -u+x 2>/dev/null | head -1)"
  if [ -n "$EXISTING_BIN" ] && [ "$FORCE" = "0" ] && [ "$GOOS" != "windows" ]; then
    warn "freebuff-proxy already exists: $EXISTING_BIN"
    warn "Skipping the download (re-run with --force to update)."
    BIN="$EXISTING_BIN"
  else
    ASSET_URL="$(printf '%s' "$RELEASE" | tr ',' '\n' | sed -n "s/.*\"browser_download_url\": *\"\([^\"]*${ASSET}\"\).*/\1/p" | head -1 | tr -d '"')"
    SUMS_URL="$(printf '%s' "$RELEASE" | tr ',' '\n' | sed -n 's/.*"browser_download_url": *"\([^"]*checksums.txt"\).*/\1/p' | head -1 | tr -d '"')"
    [ -n "$ASSET_URL" ] || die "asset $ASSET not found in the release."

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
    if [ "$GOOS" = "windows" ]; then
      unzip -o -q "$TMP/$ASSET" -d "$DIR"
      BIN="$DIR/freebuff-proxy.exe"
      if [ ! -f "$BIN" ]; then
        BIN="$(find "$DIR" -maxdepth 2 -type f -name 'freebuff-proxy*.exe' | head -1)"
      fi
    else
      tar xzf "$TMP/$ASSET" -C "$DIR"
      BIN="$DIR/freebuff-proxy"
      if [ ! -x "$BIN" ]; then
        BIN="$(find "$DIR" -type f -name freebuff-proxy | head -1)"
      fi
      if [ -n "$BIN" ]; then chmod +x "$BIN"; fi
    fi
    c "Binary: $BIN"
  fi
fi

# --- 8. next steps ------------------------------------------------------------
echo ""
c "Done. Next:"
if [ "$METHOD" = "docker" ]; then
  echo "  docker compose ps          # confirm it is healthy"
else
  echo "  cd $DIR"
  echo "  ./$BIN"
fi
echo ""
c "Then check it:"
echo "  curl http://localhost:3457/healthz"
echo "  curl http://localhost:3457/v1/models"
if [ "$BRIDGE" = "1" ]; then
  echo ""
  c "Bridge mode — chat with your own token (no proxy token stored):"
  echo '  curl -N http://localhost:3457/v1/chat/completions -H "Content-Type: application/json" \'
  echo '    -H "Authorization: Bearer <your-freebuff-token>" \'
  echo '    -d '\''{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'\'''
fi
echo ""
c "See the README for the full guide and 9router wiring."
