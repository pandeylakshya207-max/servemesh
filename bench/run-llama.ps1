param(
  [string]$Policy = "prefix-aware",
  [string]$Workload = "shared-system",
  [double]$Rps = 0.5,
  [string]$Duration = "90s",
  [string]$Warmup = "20s",
  [int]$Seed = 1,
  [string]$ColdFallback = "least-loaded",
  [int]$Groups = 8,
  [int]$SystemWords = 60,
  [int]$UserWords = 15,
  [int]$MaxTokens = 16,
  [int]$Slots = 4,
  [int]$Threads = 3,
  [string]$HeaderTimeout = "60s",
  [string]$Server = (Join-Path $HOME "llama.cpp\llama-server.exe")
)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)
$root = (Get-Location).Path
New-Item -ItemType Directory -Force -Path (Join-Path $root "bench/results") | Out-Null

go build -o bin/gateway.exe ./cmd/gateway
go build -o bin/bench.exe ./cmd/bench

$Label = $Policy
if ($Policy -eq "prefix-aware" -and $ColdFallback -eq "least-loaded") { $Label = "prefix-aware-ll" }
$FullLabel = "llama-$Label"
$PrefixBlocks = $Slots * 8

foreach ($port in 8080, 9100, 9101, 9102) {
  if (Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue) {
    throw "port $port is already in use; stop leftover processes first (Get-Process llama-server,gateway,bench | Stop-Process -Force)"
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
    Start-Sleep -Milliseconds 500
  }
  return $false
}

$procs = @()
try {
  foreach ($i in 0..2) {
    $port = 9100 + $i
    $out = Join-Path $root "bench/results/llama-server-$i.out.log"
    $err = Join-Path $root "bench/results/llama-server-$i.err.log"
    $p = Start-Process $Server -ArgumentList "-hf","Qwen/Qwen2.5-0.5B-Instruct-GGUF:Q4_K_M","--port","$port","-np","$Slots","-c","$($Slots * 2048)","-t","$Threads" -PassThru -WindowStyle Hidden -RedirectStandardOutput $out -RedirectStandardError $err
    $procs += $p
    if (-not (Wait-Ready $p "http://127.0.0.1:$port/health" 240)) { throw "llama-server $i did not become ready; see $err" }
  }
  $gwOut = Join-Path $root "bench/results/gateway.out.log"
  $gwErr = Join-Path $root "bench/results/gateway.err.log"
  $gw = Start-Process .\bin\gateway.exe -ArgumentList "-policy=$Policy","-prefix-cache-blocks=$PrefixBlocks","-cold-fallback=$ColdFallback","-health-path=/health","-health-interval=2s","-health-timeout=2s","-header-timeout=$HeaderTimeout","-backends=m0=127.0.0.1:9100,m1=127.0.0.1:9101,m2=127.0.0.1:9102","-addr=:8080" -PassThru -WindowStyle Hidden -RedirectStandardOutput $gwOut -RedirectStandardError $gwErr
  $procs += $gw
  if (-not (Wait-Ready $gw "http://127.0.0.1:8080/healthz" 15)) { throw "gateway did not become ready; see $gwErr" }

  .\bin\bench.exe -url http://127.0.0.1:8080 -workload $Workload -rps $Rps -duration $Duration -warmup $Warmup -seed $Seed -groups $Groups -system-words $SystemWords -user-words $UserWords -max-tokens $MaxTokens -label $FullLabel -out "bench/results/$Workload-$FullLabel-rps$Rps-seed$Seed.json"

  if ($gw.HasExited) { Write-Warning "gateway exited during the run (exit code $($gw.ExitCode)); discard this result" }
  $st = Invoke-RestMethod http://127.0.0.1:8080/backends
  Write-Host ("gateway: retries={0} midstream_failures={1} upstream_timeouts={2} healthy={3}" -f $st.retries, $st.midstream_failures, $st.upstream_timeouts, (($st.backends | Where-Object { $_.healthy }).Count))
  Write-Host "---- gateway log (last 8 lines) ----"
  Get-Content $gwErr -Tail 8
} finally {
  $procs | Stop-Process -Force -ErrorAction SilentlyContinue
}
