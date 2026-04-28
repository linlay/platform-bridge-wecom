# Stop platform-bridge-wecom running in background.

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

$pidFile = "run\bridge.pid"
if (-not (Test-Path $pidFile)) {
    Write-Error "bridge.pid not found; is the service running?"
}

$pid = [int](Get-Content $pidFile -Raw).Trim()
if ($pid -eq 0) {
    Write-Error "invalid pid in $pidFile"
}

$proc = Get-Process -Id $pid -ErrorAction SilentlyContinue
if ($proc) {
    Stop-Process -Id $pid -Force
    Write-Host "stopped: pid=$pid"
} else {
    Write-Host "process $pid already gone"
}

Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
