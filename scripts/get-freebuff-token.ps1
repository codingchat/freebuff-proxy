# get-freebuff-token.ps1 — install the FreeBuff CLI, log in, extract the auth token
#
# Usage:
#   .\get-freebuff-token.ps1                # install + login (if needed) + write token into ../.env (or repo .env)
#   .\get-freebuff-token.ps1 -ToClipboard   # copy the token to the clipboard instead
#   .\get-freebuff-token.ps1 -Print         # print the RAW token (be careful — it is a credential)
#   .\get-freebuff-token.ps1 -Force         # force re-login even if a token already exists
#   .\get-freebuff-token.ps1 -EnvFile D:\path\.env   # explicit target .env
#
# Token sources (documented by the community proxies):
#   - Web:        https://freebuff.llm.pm  (login → token shown on the page; no install)
#   - CLI:        this script — installs `freebuff`, runs the interactive login,
#                 then reads the authToken from the credentials file the CLI writes:
#                     %USERPROFILE%\.config\manicode\credentials.json
#                     ~/.config/manicode/credentials.json          (WSL/bash)
#
# The token is a 36-char UUID (or user_... form) and is used WITHOUT any "Bearer "
# prefix — the proxy adds it upstream itself.
param(
  [switch]$Print,
  [switch]$ToClipboard,
  [switch]$Force,
  [string]$EnvFile = ""
)

$ErrorActionPreference = "Stop"

function Find-CredentialsFile {
  $candidates = @(
    (Join-Path $env:USERPROFILE ".config\manicode\credentials.json"),
    (Join-Path $env:USERPROFILE ".config\codebuff\credentials.json"),
    (Join-Path $HOME ".config/manicode\credentials.json")
  )
  foreach ($p in $candidates) { if (Test-Path $p) { return $p } }
  return $null
}

function Get-AuthToken([string]$path) {
  # Parse with node (guaranteed present — npm was required to install the CLI).
  $script = @'
const fs = require("fs");
const data = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
const acct = data.default || Object.values(data)[0];
if (acct && acct.authToken) { process.stdout.write(acct.authToken); }
'@
  $token = node -e $script $path
  return $token
}

# --- 1. node/npm -----------------------------------------------------------
if (-not (Get-Command node -ErrorAction SilentlyContinue)) {
  Write-Host "ERROR: node/npm not found. Install Node.js >= 22 first (https://nodejs.org)." -ForegroundColor Red
  exit 1
}

# --- 2. install the FreeBuff CLI -------------------------------------------
if (-not (Get-Command freebuff -ErrorAction SilentlyContinue)) {
  Write-Host "Installing the FreeBuff CLI (npm i -g freebuff)..." -ForegroundColor Cyan
  npm install -g freebuff
  if ($LASTEXITCODE -ne 0) { Write-Host "npm install failed." -ForegroundColor Red; exit 1 }
}

$existing = Find-CredentialsFile
if (-not $Force -and $existing -and (Get-AuthToken $existing)) {
  Write-Host "Already logged in — reusing the existing token ($existing). Use -Force to re-login." -ForegroundColor Green
} else {
  Write-Host "Starting the FreeBuff login — complete it in the browser/terminal that opens, then come back." -ForegroundColor Yellow
  Write-Host "(If it asks you to paste a URL code, do so. The CLI saves the token when done.)" -ForegroundColor Yellow
  & freebuff
  if ($LASTEXITCODE -ne 0) { Write-Host "freebuff exited with code $LASTEXITCODE — login may have failed." -ForegroundColor Red; exit 1 }
}

# --- 3. extract the token ---------------------------------------------------
$creds = Find-CredentialsFile
if (-not $creds) {
  Write-Host "ERROR: credentials file not found after login. Looked in ~/.config/manicode|codebuff/credentials.json" -ForegroundColor Red
  Write-Host "Fallback: log in at https://freebuff.llm.pm and copy the token from the page." -ForegroundColor Yellow
  exit 1
}
$token = Get-AuthToken $creds
if (-not $token) {
  Write-Host "ERROR: no authToken field in $creds" -ForegroundColor Red
  exit 1
}

$masked = if ($token.Length -gt 12) { $token.Substring(0, 8) + "..." + $token.Substring($token.Length - 4) } else { "***" }
Write-Host "Token found ($($token.Length) chars): $masked" -ForegroundColor Green

# --- 4. deliver --------------------------------------------------------------
if ($Print) {
  Write-Host $token
  exit 0
}
if ($ToClipboard) {
  Set-Clipboard -Value $token
  Write-Host "Token copied to clipboard." -ForegroundColor Green
  exit 0
}
# Default: write AUTH_TOKENS into a freebuff-proxy .env
if (-not $EnvFile) {
  $repoEnv = Join-Path (Split-Path $PSScriptRoot -Parent) ".env"   # scripts/.. -> repo root
  if (Test-Path (Join-Path (Split-Path $PSScriptRoot -Parent) ".env.example")) { $EnvFile = $repoEnv }
}
if ($EnvFile) {
  if (Test-Path $EnvFile) {
    $content = Get-Content $EnvFile -Raw
    if ($content -match '(?m)^AUTH_TOKENS=.*$') {
      $content = $content -replace '(?m)^AUTH_TOKENS=.*$', "AUTH_TOKENS=$token"
    } else {
      $content = $content.TrimEnd() + "`nAUTH_TOKENS=$token`n"
    }
    Set-Content -Path $EnvFile -Value $content -NoNewline -Encoding utf8
  } else {
    Set-Content -Path $EnvFile -Value "AUTH_TOKENS=$token`n" -Encoding utf8
  }
  Write-Host "AUTH_TOKENS written to $EnvFile (gitignored)." -ForegroundColor Green
} else {
  Write-Host "Token: $token" -ForegroundColor Yellow
  Write-Host "Run with -ToClipboard or -Print to see it again; or pass -EnvFile <path>." -ForegroundColor Yellow
}
