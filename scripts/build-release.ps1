# Cross-compile platform-bridge-wecom for win/mac/linux x amd64/arm64 (native PowerShell).
# Output: dist\<version>\<name>-<os>-<arch>[.exe] + archives (zip for windows, tar.gz otherwise).
#
# Usage:
#   scripts\build-release.ps1                      # use VERSION file, all targets
#   $env:VERSION="1.2.3"; scripts\build-release.ps1
#   scripts\build-release.ps1 linux/amd64          # single target

[CmdletBinding()]
param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Targets)

$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

$BinaryName = "platform-bridge-wecom"
if (-not $env:VERSION) {
    if (Test-Path "VERSION") { $Version = (Get-Content "VERSION" -Raw).Trim() } else { $Version = "0.0.0-dev" }
} else {
    $Version = $env:VERSION
}
try { $Commit = (git rev-parse --short HEAD 2>$null).Trim() } catch { $Commit = "unknown" }
if (-not $Commit) { $Commit = "unknown" }
$BuildTime = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$DefaultTargets = @(
    "windows/amd64", "windows/arm64",
    "darwin/amd64",  "darwin/arm64",
    "linux/amd64",   "linux/arm64"
)
if (-not $Targets -or $Targets.Count -eq 0) { $Targets = $DefaultTargets }

$OutRoot = Join-Path "dist" $Version
if (Test-Path $OutRoot) { Remove-Item -Recurse -Force $OutRoot }
New-Item -ItemType Directory -Path $OutRoot | Out-Null

$Ldflags = "-s -w -X main.version=$Version -X main.commit=$Commit -X main.buildTime=$BuildTime"
Write-Host "==> building $BinaryName $Version ($Commit)"

function Get-Sha256 {
    param([string]$Path)
    (Get-FileHash -Algorithm SHA256 $Path).Hash.ToLower()
}

$ChecksumLines = @()

foreach ($target in $Targets) {
    $parts = $target.Split("/")
    $goos = $parts[0]; $goarch = $parts[1]
    $suffix = if ($goos -eq "windows") { ".exe" } else { "" }

    $pkgName = "$BinaryName-$Version-$goos-$goarch"
    $pkgDir = Join-Path $OutRoot $pkgName
    New-Item -ItemType Directory -Path $pkgDir | Out-Null

    $binPath = Join-Path $pkgDir "$BinaryName$suffix"
    Write-Host "--> $goos/$goarch -> $binPath"

    $env:GOOS = $goos; $env:GOARCH = $goarch; $env:CGO_ENABLED = "0"
    & go build -trimpath -ldflags $Ldflags -o $binPath ./cmd/bridge
    if ($LASTEXITCODE -ne 0) { throw "go build failed for $target" }

    foreach ($f in @("README.md", "SPEC.md", "VERSION", ".env.example")) {
        if (Test-Path $f) { Copy-Item $f $pkgDir }
    }

    Push-Location $OutRoot
    try {
        if ($goos -eq "windows") {
            $zipPath = "$pkgName.zip"
            if (Test-Path $zipPath) { Remove-Item $zipPath }
            Compress-Archive -Path $pkgName -DestinationPath $zipPath
            $ChecksumLines += "$((Get-Sha256 $zipPath))  $zipPath"
        } else {
            $tarPath = "$pkgName.tar.gz"
            & tar -czf $tarPath $pkgName
            if ($LASTEXITCODE -ne 0) { throw "tar failed for $pkgName (install bsdtar/gnutar on Windows 10+)" }
            $ChecksumLines += "$((Get-Sha256 $tarPath))  $tarPath"
        }
    } finally { Pop-Location }
}

Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "==> artifacts in ${OutRoot}:"
Get-ChildItem $OutRoot | ForEach-Object { Write-Host "    $($_.Name)" }

$sumsPath = Join-Path $OutRoot "SHA256SUMS.txt"
$ChecksumLines | Set-Content -Encoding ASCII $sumsPath
Write-Host ""
Write-Host "==> SHA256SUMS:"
$ChecksumLines | ForEach-Object { Write-Host "    $_" }
