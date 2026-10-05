Set-Location (Split-Path -Parent $PSScriptRoot)
$rows = Get-ChildItem bench/results/*-seed*.json | ForEach-Object { Get-Content $_ -Raw | ConvertFrom-Json }
$rows | Group-Object workload, offered_rps, label | ForEach-Object {
  $g = $_.Group
  [pscustomobject]@{
    Workload = $g[0].workload
    RPS      = $g[0].offered_rps
    Policy   = $g[0].label
    Runs     = $g.Count
    Errors   = ($g | Measure-Object errors -Sum).Sum
    HitPct   = [math]::Round(100 * ($g | Measure-Object cache_hit_rate -Average).Average, 1)
    TTFTp50  = [math]::Round(($g | ForEach-Object { $_.ttft.p50_ms } | Measure-Object -Average).Average)
    TTFTp95  = [math]::Round(($g | ForEach-Object { $_.ttft.p95_ms } | Measure-Object -Average).Average)
    TTFTp99  = [math]::Round(($g | ForEach-Object { $_.ttft.p99_ms } | Measure-Object -Average).Average)
  }
} | Sort-Object Workload, RPS, Policy | Format-Table -AutoSize
