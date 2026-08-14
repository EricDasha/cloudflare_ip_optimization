$ErrorActionPreference = "Stop"

$root = Resolve-Path (Join-Path $PSScriptRoot "..")
$dist = Join-Path $root "dist"
$linux = Join-Path $dist "linux-amd64"
$cache = Join-Path $dist ".gocache"
$staleTmp = Join-Path $dist ".gotmp"
New-Item -ItemType Directory -Force -Path $linux | Out-Null
New-Item -ItemType Directory -Force -Path $cache | Out-Null
if (Test-Path $staleTmp) {
  Write-Host "==> stale dist/.gotmp detected; ignored by .dockerignore"
}

function Invoke-Native {
  param(
    [Parameter(Mandatory = $true)][string]$FilePath,
    [Parameter(Mandatory = $true)][string[]]$NativeArgs
  )
  & $FilePath @NativeArgs
  if ($LASTEXITCODE -ne 0) {
    throw "$FilePath failed with exit code $LASTEXITCODE"
  }
}

Write-Host "==> build linux/amd64 binaries"
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:GOTELEMETRY = "off"
$env:GOCACHE = $cache
Remove-Item Env:\GOTMPDIR -ErrorAction SilentlyContinue

Invoke-Native "go" @("build", "-trimpath", "-ldflags=-s -w", "-o", (Join-Path $linux "cfdata"), (Join-Path $root "cfdata.go"))
Invoke-Native "go" @("build", "-trimpath", "-ldflags=-s -w", "-o", (Join-Path $linux "cfnat"), (Join-Path $root "cfnat.go"))
Invoke-Native "go" @("build", "-trimpath", "-ldflags=-s -w", "-o", (Join-Path $linux "cloudflare-web"), (Join-Path $root "web"))

Write-Host "==> export Windows root CAs to dist/ca-certificates.crt"
$certOut = Join-Path $dist "ca-certificates.crt"
$stores = @("Cert:\LocalMachine\Root", "Cert:\CurrentUser\Root")
$seen = @{}
$pemBlocks = New-Object System.Collections.Generic.List[string]

foreach ($store in $stores) {
  if (-not (Test-Path $store)) { continue }
  Get-ChildItem $store | ForEach-Object {
    $cert = $_
    if ($seen.ContainsKey($cert.Thumbprint)) { return }
    $seen[$cert.Thumbprint] = $true
    $b64 = [Convert]::ToBase64String($cert.RawData, [Base64FormattingOptions]::InsertLineBreaks)
    $pemBlocks.Add("-----BEGIN CERTIFICATE-----`n$b64`n-----END CERTIFICATE-----`n")
  }
}

if ($pemBlocks.Count -eq 0) {
  throw "No root certificates exported; cannot build HTTPS-capable scratch image."
}

[IO.File]::WriteAllText($certOut, ($pemBlocks -join "`n"), [Text.UTF8Encoding]::new($false))

Write-Host "==> dist ready"
Get-ChildItem -File $linux | Select-Object FullName, Length, LastWriteTime
Get-Item $certOut | Select-Object FullName, Length, LastWriteTime
