param([int]$Requests = 9)
$ErrorActionPreference = "Stop"
Set-Location (Split-Path -Parent $PSScriptRoot)

go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/gateway.exe ./cmd/gateway

$mocks = @()
try {
  foreach ($i in 0..2) {
    $mocks += Start-Process .\bin\mockbackend.exe -ArgumentList "-id=m$i","-addr=:$(9000 + $i)" -PassThru -WindowStyle Hidden
  }
  Start-Process .\bin\gateway.exe -ArgumentList "-policy=round-robin","-health-interval=0s","-backends=m0=localhost:9000,m1=localhost:9001,m2=localhost:9002","-addr=:8080" -WindowStyle Hidden | Out-Null
  Start-Sleep -Seconds 2

  Stop-Process -Id $mocks[1].Id -Force
  Write-Host "m1 killed while idle (active health checks disabled)"
  Start-Sleep -Milliseconds 500

  $body = '{"model":"mock","stream":true,"max_tokens":3,"messages":[{"role":"user","content":"hello"}]}'
  foreach ($n in 1..$Requests) {
    try {
      $r = Invoke-WebRequest -UseBasicParsing -Method Post -Uri http://localhost:8080/v1/chat/completions -ContentType "application/json" -Body $body
      "{0}: status={1} backend={2} attempts={3}" -f $n, $r.StatusCode, $r.Headers["X-Backend-ID"], $r.Headers["X-Gateway-Attempts"]
    } catch {
      "{0}: FAILED {1}" -f $n, $_.Exception.Message
    }
  }
  Write-Host "---- gateway state ----"
  Invoke-RestMethod http://localhost:8080/backends | ConvertTo-Json -Depth 4
} finally {
  Get-Process mockbackend,gateway -ErrorAction SilentlyContinue | Stop-Process -Force
}
