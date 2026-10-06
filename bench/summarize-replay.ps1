Set-Location (Split-Path -Parent $PSScriptRoot)
Get-ChildItem bench/results/replay-*.json | ForEach-Object { Get-Content $_ -Raw | ConvertFrom-Json } |
  Group-Object label | ForEach-Object {
    $g = $_.Group
    [pscustomobject]@{
      Policy    = $g[0].label
      Runs      = $g.Count
      Measured  = ($g | Measure-Object requests_measured -Sum).Sum
      HitPct    = [math]::Round(100 * ($g | Measure-Object cache_hit_rate -Average).Average, 1)
      Prefilled = [math]::Round(($g | Measure-Object mean_prompt_tokens_processed -Average).Average)
      TTFTp50   = [math]::Round(($g | Measure-Object ttft_p50_ms -Average).Average)
      TTFTp95   = [math]::Round(($g | Measure-Object ttft_p95_ms -Average).Average)
      WarmN     = ($g | Measure-Object warm_requests -Sum).Sum
      ColdN     = ($g | Measure-Object cold_requests -Sum).Sum
    }
  } | Sort-Object Policy | Format-Table -AutoSize
