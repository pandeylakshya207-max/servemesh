param(
  [string]$Policy = "prefix-aware",
  [string]$Workload = "shared-system",
  [double]$Rps = 10,
  [int]$CacheBlocks = 150
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
$root = (Get-Location).Path

go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/gateway.exe ./cmd/gateway
go build -o bin/bench.exe ./cmd/bench

function Start-Mock([int]$i) {
  Start-Process .\bin\mockbackend.exe -ArgumentList "-id=m$i","-addr=:$(9000 + $i)","-cache-blocks=$CacheBlocks" -PassThru -WindowStyle Hidden
}

function Get-M1Healthy {
  try {
    $s = Invoke-RestMethod http://localhost:8080/backends -TimeoutSec 2
    return [bool](($s.backends | Where-Object { $_.id -eq "m1" }).healthy)
  } catch { return $null }
}

function Wait-M1([bool]$want, [int]$timeoutSec, [datetime]$since) {
  while (((Get-Date) - $since).TotalSeconds -lt $timeoutSec) {
    if ((Get-M1Healthy) -eq $want) { return [int](((Get-Date) - $since).TotalMilliseconds) }
    Start-Sleep -Milliseconds 50
  }
  return $null
}

function Show-Ms($v) { if ($null -eq $v) { "timed out" } else { "$v ms" } }

New-Item -ItemType Directory -Force -Path (Join-Path $root "bench/results") | Out-Null
$out = Join-Path $root "bench/results/chaos-$Policy.txt"
$mocks = @{}
try {
  foreach ($i in 0..2) { $mocks[$i] = Start-Mock $i }
  Start-Process .\bin\gateway.exe -ArgumentList "-policy=$Policy","-prefix-cache-blocks=$CacheBlocks","-backends=m0=localhost:9000,m1=localhost:9001,m2=localhost:9002","-addr=:8080" -WindowStyle Hidden | Out-Null
  Start-Sleep -Seconds 2

  $bench = Start-Process .\bin\bench.exe -ArgumentList "-url","http://localhost:8080","-workload",$Workload,"-rps","$Rps","-duration","45s","-warmup","5s","-label",$Policy -RedirectStandardOutput $out -PassThru -WindowStyle Hidden

  Start-Sleep -Seconds 15
  $tKill = Get-Date
  Stop-Process -Id $mocks[1].Id -Force
  Write-Host "m1 killed at t=15s"
  $ejectMs = Wait-M1 $false 10 $tKill
  Write-Host ("ejected from rotation after: " + (Show-Ms $ejectMs))

  Start-Sleep -Seconds 12
  $mocks[1] = Start-Mock 1
  $tUp = Get-Date
  Write-Host "m1 restarted (cold cache)"
  $recoverMs = Wait-M1 $true 15 $tUp
  Write-Host ("back in rotation after: " + (Show-Ms $recoverMs))

  $bench.WaitForExit(120000) | Out-Null
  Write-Host "---- bench output ----"
  Get-Content $out
  $st = Invoke-RestMethod http://localhost:8080/backends
  Write-Host "---- gateway counters ----"
  Write-Host ("retries={0} midstream_failures={1}" -f $st.retries, $st.midstream_failures)
} finally {
  Get-Process mockbackend,gateway,bench -ErrorAction SilentlyContinue | Stop-Process -Force
}
