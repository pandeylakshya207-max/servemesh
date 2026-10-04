param(
  [string]$Policy = "round-robin",
  [string]$Workload = "shared-system",
  [double]$Rps = 10,
  [string]$Duration = "30s",
  [int]$CacheBlocks = 150
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/gateway.exe ./cmd/gateway
go build -o bin/bench.exe ./cmd/bench

$procs = @()
try {
  foreach ($i in 0..2) {
    $port = 9000 + $i
    $procs += Start-Process .\bin\mockbackend.exe -ArgumentList "-id=m$i","-addr=:$port","-cache-blocks=$CacheBlocks" -PassThru -WindowStyle Hidden
  }
  $procs += Start-Process .\bin\gateway.exe -ArgumentList "-policy=$Policy","-prefix-cache-blocks=$CacheBlocks","-backends=m0=localhost:9000,m1=localhost:9001,m2=localhost:9002","-addr=:8080" -PassThru -WindowStyle Hidden
  Start-Sleep -Seconds 1
  .\bin\bench.exe -url http://localhost:8080 -workload $Workload -rps $Rps -duration $Duration -label $Policy -out "bench/results/$Workload-$Policy.json"
} finally {
  $procs | Stop-Process -Force -ErrorAction SilentlyContinue
}
