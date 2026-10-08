param(
  [double]$Rps = 60,
  [int]$MaxInflight = 0,
  [int]$MaxQueue = 32,
  [string]$QueueWait = "1s",
  [string]$Duration = "40s",
  [string]$Warmup = "10s",
  [int]$Seed = 1,
  [double]$HighFrac = 0.2,
  [string]$SendPriority = ""
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
$root = (Get-Location).Path
New-Item -ItemType Directory -Force -Path (Join-Path $root "bench/results") | Out-Null

go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/gateway.exe ./cmd/gateway
go build -o bin/overload.exe ./cmd/overload

$Label = "noadm"
if ($MaxInflight -gt 0) { $Label = "adm$MaxInflight" }
if ($SendPriority -ne "") { $Label = "$Label-flat" }

foreach ($port in 8080, 9000, 9001, 9002) {
  if (Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue) {
    throw "port $port is already in use; stop leftover processes first (Get-Process mockbackend,gateway,overload | Stop-Process -Force)"
  }
}

function Wait-Ready($proc, [string]$url, [int]$timeoutSec) {
  $deadline = (Get-Date).AddSeconds($timeoutSec)
  while ((Get-Date) -lt $deadline) {
    if ($proc.HasExited) { return $false }
    try {
      $r = Invoke-WebRequest -UseBasicParsing -Uri $url -TimeoutSec 2
      if ($r.StatusCode -eq 200) { return $true }
    } catch { }
    Start-Sleep -Milliseconds 200
  }
  return $false
}

$procs = @()
try {
  foreach ($i in 0..2) {
    $port = 9000 + $i
    $p = Start-Process .\bin\mockbackend.exe -ArgumentList "-id=m$i","-addr=:$port","-cache-blocks=150" -PassThru -WindowStyle Hidden
    $procs += $p
    if (-not (Wait-Ready $p "http://127.0.0.1:$port/healthz" 15)) { throw "mock backend m$i did not become ready" }
  }
  $gwArgs = @("-policy=least-loaded","-backends=m0=127.0.0.1:9000,m1=127.0.0.1:9001,m2=127.0.0.1:9002","-addr=:8080")
  if ($MaxInflight -gt 0) { $gwArgs += @("-max-inflight=$MaxInflight","-max-queue=$MaxQueue","-queue-wait=$QueueWait") }
  $gwErr = Join-Path $root "bench/results/gateway-overload.err.log"
  $gwOut = Join-Path $root "bench/results/gateway-overload.out.log"
  $gw = Start-Process .\bin\gateway.exe -ArgumentList $gwArgs -PassThru -WindowStyle Hidden -RedirectStandardOutput $gwOut -RedirectStandardError $gwErr
  $procs += $gw
  if (-not (Wait-Ready $gw "http://127.0.0.1:8080/healthz" 15)) { throw "gateway did not become ready; see $gwErr" }

  .\bin\overload.exe -url http://127.0.0.1:8080 -rps $Rps -duration $Duration -warmup $Warmup -high-frac $HighFrac -seed $Seed -send-priority=$SendPriority -label $Label -out "bench/results/overload-$Label-rps$Rps-s$Seed.json"

  if ($gw.HasExited) { Write-Warning "gateway exited during the run; discard this result" }
  $m = (Invoke-WebRequest -UseBasicParsing -Uri http://127.0.0.1:8080/metrics).Content
  ($m -split "`n") | Where-Object { $_ -match '^servemesh_shed_total|^servemesh_retries_total|^servemesh_midstream' } | ForEach-Object { Write-Host $_ }
} finally {
  $procs | Stop-Process -Force -ErrorAction SilentlyContinue
}
