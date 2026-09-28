# EdgeLite AI Sidecar startup script (PowerShell)
# Usage: .\scripts\start_sidecar.ps1 [-Dev]
param(
  [switch]$Dev
)

$ErrorActionPreference = "Stop"
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$ProjectRoot = Split-Path -Parent $ScriptDir
$SidecarDir = Join-Path $ProjectRoot "ai_sidecar"

# Default configuration
$Host = $env:AI_SIDECAR_HOST
if (-not $Host) { $Host = "0.0.0.0" }
$Port = $env:AI_SIDECAR_PORT
if (-not $Port) { $Port = "50052" }
$ModelsDir = $env:AI_MODELS_DIR
if (-not $ModelsDir) { $ModelsDir = Join-Path $SidecarDir "models" }
$LogLevel = "INFO"

if ($Dev) {
  $LogLevel = "DEBUG"
  Write-Host "Starting in DEVELOPMENT mode..." -ForegroundColor Yellow
}

# Create models directory
if (-not (Test-Path $ModelsDir)) {
  New-Item -ItemType Directory -Path $ModelsDir -Force | Out-Null
}

# Check Python
$pythonCmd = Get-Command python -ErrorAction SilentlyContinue
if (-not $pythonCmd) {
  Write-Host "ERROR: python is not installed or not in PATH" -ForegroundColor Red
  exit 1
}

# Check dependencies
$depCheck = & python -c "import aiohttp, onnxruntime, numpy" 2>&1
if ($LASTEXITCODE -ne 0) {
  Write-Host "Installing dependencies..." -ForegroundColor Cyan
  Push-Location $SidecarDir
  & python -m pip install -r requirements.txt
  Pop-Location
}

# Start server
Write-Host "Starting EdgeLite AI Sidecar on $Host`:$Port" -ForegroundColor Green
Write-Host "  Models dir: $ModelsDir"
Write-Host "  Log level:  $LogLevel"

Push-Location $SidecarDir
$args = @(
  "server.py",
  "--host", $Host,
  "--port", $Port,
  "--models-dir", $ModelsDir,
  "--log-level", $LogLevel
)
if ($Dev) { $args += "--json-logs" }

& python @args
Pop-Location
