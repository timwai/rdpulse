# Build script for RDPulse Agent and Relay
$ErrorActionPreference = "Stop"

Write-Host "[1/3] Checking/Generating Windows Manifest Resource & Icon..." -ForegroundColor Cyan
if (Test-Path "assets/icon.png") {
    go run scripts/generate_icon.go
} elseif (!(Test-Path "assets/icon.ico")) {
    Write-Error "assets/icon.png is required to generate the application icon"
}
if (Get-Command "rsrc" -ErrorAction SilentlyContinue) {
    rsrc -manifest cmd/agent/agent.manifest -ico assets/icon.ico -o cmd/agent/rsrc.syso
} elseif (Test-Path "$env:USERPROFILE\go\bin\rsrc.exe") {
    & "$env:USERPROFILE\go\bin\rsrc.exe" -manifest cmd/agent/agent.manifest -ico assets/icon.ico -o cmd/agent/rsrc.syso
}

Write-Host "[2/4] Building Windows rdp-agent (CLI + GUI) & rdp-agent-gui (No Console)..." -ForegroundColor Cyan
go build -o bin/rdp-agent.exe ./cmd/agent
go build -ldflags="-H=windowsgui" -o bin/rdp-agent-gui.exe ./cmd/agent

Write-Host "[3/4] Building Windows rdp-relay.exe..." -ForegroundColor Cyan
go build -o bin/rdp-relay.exe ./cmd/relay

Write-Host "[4/4] Cross-compiling Linux rdp-relay (amd64 + arm64)..." -ForegroundColor Cyan
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -ldflags="-s -w" -o bin/rdp-relay-linux-amd64 ./cmd/relay
$env:GOARCH = "arm64"
go build -ldflags="-s -w" -o bin/rdp-relay-linux-arm64 ./cmd/relay
$env:GOOS = "windows"
$env:GOARCH = "amd64"

Write-Host "Build finished successfully! Binaries are in .\bin\" -ForegroundColor Green
