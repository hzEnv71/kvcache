param(
  [string]$Etcd = "127.0.0.1:2379",
  [string]$Svc = "kv-cache",
  [string]$Group = "test"
)

$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Wait-PortReady {
  param(
    [int]$Port,
    [int]$Retry = 20,
    [int]$SleepMs = 500
  )

  for ($i = 1; $i -le $Retry; $i++) {
    $ok = Test-NetConnection 127.0.0.1 -Port $Port -InformationLevel Quiet -WarningAction SilentlyContinue
    if ($ok) {
      Write-Host "Port ready: 127.0.0.1:$Port"
      return $true
    }
    Start-Sleep -Milliseconds $SleepMs
  }

  return $false
}

function Invoke-GetWithRetry {
  param(
    [string]$Addr,
    [string]$Group,
    [string]$Key,
    [int]$Retry = 15,
    [int]$SleepSec = 2
  )

  for ($i = 1; $i -le $Retry; $i++) {
    $output = go run ./cmd/client --op get --addr $Addr --group $Group --key $Key --timeout 8s 2>&1
    if ($LASTEXITCODE -eq 0) {
      Write-Host $output
      return $true
    }

    Write-Host "get attempt $i/$Retry failed: $($output | Out-String)"
    Start-Sleep -Seconds $SleepSec
  }

  return $false
}

Write-Host "[1/7] Build binaries..."
go build ./cmd/server
if ($LASTEXITCODE -ne 0) { throw "build cmd/server failed" }
go build ./cmd/client
if ($LASTEXITCODE -ne 0) { throw "build cmd/client failed" }

Write-Host "[2/7] Start 3 servers (same as manual order)..."
$procs = @()
$procs += Start-Process -FilePath "go" -ArgumentList @("run", "./cmd/server", "--addr", "127.0.0.1:8001", "--svc", $Svc, "--group", $Group, "--etcd", $Etcd) -PassThru
Start-Sleep -Milliseconds 800
$procs += Start-Process -FilePath "go" -ArgumentList @("run", "./cmd/server", "--addr", "127.0.0.1:8002", "--svc", $Svc, "--group", $Group, "--etcd", $Etcd) -PassThru
Start-Sleep -Milliseconds 800
$procs += Start-Process -FilePath "go" -ArgumentList @("run", "./cmd/server", "--addr", "127.0.0.1:8003", "--svc", $Svc, "--group", $Group, "--etcd", $Etcd) -PassThru

try {
  Write-Host "[3/7] Wait ports ready..."
  if (-not (Wait-PortReady -Port 8001)) { throw "port 8001 not ready" }
  if (-not (Wait-PortReady -Port 8002)) { throw "port 8002 not ready" }
  if (-not (Wait-PortReady -Port 8003)) { throw "port 8003 not ready" }

  Write-Host "[4/7] Wait discovery stabilize..."
  Start-Sleep -Seconds 3

  Write-Host "[5/7] set on 8001 (same as manual command)..."
  go run ./cmd/client --op set --addr 127.0.0.1:8001 --group $Group --key k1 --value v1 --timeout 8s
  if ($LASTEXITCODE -ne 0) { throw "set failed" }

  Write-Host "[6/7] get on 8002 with retry (wait for discovery converge)..."
  $ok = Invoke-GetWithRetry -Addr "127.0.0.1:8002" -Group $Group -Key "k1" -Retry 15 -SleepSec 2
  if (-not $ok) { throw "get from 8002 failed after retries" }

  Write-Host "[7/7] Success."
}
finally {
  Write-Host "Cleaning up servers..."
  foreach ($p in $procs) {
    if ($null -ne $p -and -not $p.HasExited) {
      Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
    }
  }
}
