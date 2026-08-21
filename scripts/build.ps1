<#
.SYNOPSIS
    Compiles the freebuff-proxy binary.
.DESCRIPTION
    1. Compiles the Go binary to bin/freebuff-proxy.exe.
#>

[CmdletBinding()]
param (
    [string]$OutputPath = "bin/freebuff-proxy.exe"
)

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot

Write-Host "==> Starting build from $RepoRoot" -ForegroundColor Cyan

$OutDir = Split-Path -Parent (Join-Path $RepoRoot $OutputPath)
if (-not (Test-Path $OutDir)) {
    New-Item -ItemType Directory -Path $OutDir -Force | Out-Null
}

Write-Host "==> Compiling Go binary to $OutputPath..." -ForegroundColor Yellow
Push-Location $RepoRoot
try {
    go build -o $OutputPath ./cmd/freebuff-proxy
    Write-Host "==> Build successful: $OutputPath" -ForegroundColor Green
} finally {
    Pop-Location
}
