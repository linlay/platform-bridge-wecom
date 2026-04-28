# Start platform-bridge-wecom in background.
# Reads .env before launching so env vars are available to the process.

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

if (-not (Test-Path ".env")) {
    Write-Error "missing .env file"
}

# Load .env into process environment
Get-Content ".env" | ForEach-Object {
    $line = $_.Trim()
    if ($line -and -not $line.StartsWith("#")) {
        $idx = $line.IndexOf("=")
        if ($idx -gt 0) {
            $key = $line.Substring(0, $idx).Trim()
            $val = $line.Substring($idx + 1).Trim()
            [System.Environment]::SetEnvironmentVariable($key, $val, "Process")
        }
    }
}

$exe = ".\platform-bridge-wecom.exe"
if (-not (Test-Path $exe)) {
    Write-Error "platform-bridge-wecom.exe not found in $PWD"
}

$runDir = "run"
if (-not (Test-Path $runDir)) {
    New-Item -ItemType Directory -Path $runDir | Out-Null
}

$logFile = Join-Path $runDir "bridge.log"
$errFile = Join-Path $runDir "bridge.stderr.log"
$pidFile = Join-Path $runDir "bridge.pid"

$proc = Start-Process -FilePath $exe -PassThru -NoNewWindow -RedirectStandardOutput $logFile -RedirectStandardError $errFile
$proc.Id | Out-File -FilePath $pidFile -Encoding ASCII
Write-Host "started: pid=$($proc.Id)"
