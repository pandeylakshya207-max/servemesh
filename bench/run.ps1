param(
  [string]$Policy = "round-robin",
  [string]$Workload = "shared-system",
  [double]$Rps = 10,
  [string]$Duration = "30s",
  [int]$CacheBlocks = 150,
  [int]$Seed = 1,
  [string]$ColdFallback = "rendezvous"
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/gateway.exe ./cmd/gateway
go build -o bin/bench.exe ./cmd/bench

$Label = $Policy
if ($Policy -eq "prefix-aware" -and $ColdFallback -eq "least-loaded") { $Label = "prefix-aware-ll" }

function Wait-Http([string]$url, [int]$timeoutSec = 10) {
  $deadline = (Get-Date).AddSeconds($timeoutSec)
  while ((Get-Date) -lt $deadline) {
    try { Invoke-WebRequest -UseBasicParsing -Uri $url -TimeoutSec 1 | Out-Null; return $true }
    catch { Start-Sleep -Milliseconds 100 }
  }
  return $false
}

foreach ($port in 8080, 9000, 9001, 9002) {
  if (Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue) {
    throw "port $port is already in use; stop leftover mockbackend/gateway processes first"
  }
}

$procs = @()
try {
  foreach ($i in 0..2) {
    $port = 9000 + $i
    $procs += Start-Process .\bin\mockbackend.exe -ArgumentList "-id=m$i","-addr=:$port","-cache-blocks=$CacheBlocks" -PassThru -WindowStyle Hidden
    if (-not (Wait-Http "http://localhost:$port/healthz")) { throw "mock backend m$i did not become ready" }
  }
  $gw = Start-Process .\bin\gateway.exe -ArgumentList "-policy=$Policy","-prefix-cache-blocks=$CacheBlocks","-cold-fallback=$ColdFallback","-backends=m0=localhost:9000,m1=localhost:9001,m2=localhost:9002","-addr=:8080" -PassThru -WindowStyle Hidden
  $procs += $gw
  if (-not (Wait-Http "http://localhost:8080/healthz")) { throw "gateway did not become ready" }

  .\bin\bench.exe -url http://localhost:8080 -workload $Workload -rps $Rps -duration $Duration -seed $Seed -label $Label -out "bench/results/$Workload-$Label-rps$Rps-seed$Seed.json"

  if ($gw.HasExited) { Write-Warning "gateway exited during the run (exit code $($gw.ExitCode)); discard this result" }
} finally {
  $procs | Stop-Process -Force -ErrorAction SilentlyContinue
}
