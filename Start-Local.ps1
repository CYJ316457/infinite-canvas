param(
    [int]$ApiPort = 8080,
    [int]$WebPort = 3000,
    [switch]$InstallDeps
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$webRoot = Join-Path $repoRoot "web"
$envFile = Join-Path $repoRoot ".env"
$envExample = Join-Path $repoRoot ".env.example"
$dataDir = Join-Path $repoRoot "data"

function Require-Command($name, $installHint) {
    if (-not (Get-Command $name -ErrorAction SilentlyContinue)) {
        throw "$name not found. $installHint"
    }
}

Require-Command "go" "Install Go 1.25+ and reopen PowerShell."
Require-Command "bun" "Install Bun and reopen PowerShell: powershell -c \"irm bun.sh/install.ps1 | iex\""

if (-not (Test-Path -LiteralPath $envFile)) {
    if (-not (Test-Path -LiteralPath $envExample)) {
        throw ".env.example not found at $envExample"
    }
    Copy-Item -LiteralPath $envExample -Destination $envFile
    Write-Host "Created .env from .env.example"
}

New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

if ($InstallDeps -or -not (Test-Path -LiteralPath (Join-Path $webRoot "node_modules"))) {
    Write-Host "Installing frontend dependencies with bun install..."
    Push-Location $webRoot
    try {
        bun install --frozen-lockfile
    } finally {
        Pop-Location
    }
}

$apiCommand = @"
`$ErrorActionPreference = 'Stop'
Set-Location '$repoRoot'
`$env:PORT = '$ApiPort'
go run .
Read-Host 'API stopped. Press Enter to close'
"@

$webCommand = @"
`$ErrorActionPreference = 'Stop'
Set-Location '$webRoot'
`$env:PORT = '$WebPort'
`$env:API_BASE_URL = 'http://127.0.0.1:$ApiPort'
bun run dev
Read-Host 'Web stopped. Press Enter to close'
"@

Start-Process powershell -ArgumentList @("-NoExit", "-ExecutionPolicy", "Bypass", "-Command", $apiCommand) -WorkingDirectory $repoRoot
Start-Sleep -Seconds 2
Start-Process powershell -ArgumentList @("-NoExit", "-ExecutionPolicy", "Bypass", "-Command", $webCommand) -WorkingDirectory $webRoot

Write-Host "Local services are starting:"
Write-Host "  API: http://127.0.0.1:$ApiPort"
Write-Host "  Web: http://localhost:$WebPort"
Write-Host "Close the two PowerShell windows to stop the services."