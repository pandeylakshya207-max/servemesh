# servemesh

[![ci](https://github.com/pandeylakshya207-max/servemesh/actions/workflows/ci.yml/badge.svg)](https://github.com/pandeylakshya207-max/servemesh/actions/workflows/ci.yml)

A cache-aware, fault-tolerant gateway for LLM serving, written in Go. It sits in front of several model replicas, exposes an OpenAI-style streaming API, and decides which replica serves each request.

## Features

- Streaming reverse proxy for `/v1/chat/completions` (SSE).
- Pluggable routing policies: round-robin, least-loaded, and prefix-aware (routes to the replica most likely to hold the prompt's prefix in its KV cache, with a bounded-load cap and rendezvous-hash placement for cold prefixes).
- Active and passive health checking, retry on a different replica before the first byte, and explicit error events when a stream fails midway.
- A mock replica with a simulated prefix cache, and an open-loop (Poisson) benchmark harness with seeded workloads.

## Results (simulated)

On a 3-replica simulation (mean of 3 seeds), prefix-aware routing with a least-loaded fallback for cold prefixes raised the prefix-cache hit rate from about 28-31% (round-robin / least-loaded) to 68-73% on shared-system-prompt traffic, and from 10-13% to 34% on multi-turn traffic. It cut median time-to-first-token by about 91% to 95% on shared-system traffic. Versus least-loaded, p99 TTFT was about 22% lower at 10 rps and 58% lower at 25 rps on shared-system traffic, and 11% lower on multi-turn traffic. On traffic with nothing to cache it matched least-loaded. A backend killed under load failed only the 3 of 400 requests already streaming from it.

These are simulator results, not real GPU measurements. Full tables, caveats and limitations: [docs/RESULTS.md](docs/RESULTS.md).
## Quick start

```powershell
go test ./...
go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/gateway.exe ./cmd/gateway

.\bin\mockbackend.exe -id=m0 -addr=:9000
.\bin\mockbackend.exe -id=m1 -addr=:9001
.\bin\gateway.exe -policy=prefix-aware -cold-fallback=least-loaded -backends=m0=localhost:9000,m1=localhost:9001
```

## Layout

- `internal/router`: routing policies
- `internal/proxy`: streaming gateway, health checks
- `internal/backend`: mock replica with a simulated prefix cache
- `cmd/bench`: load generator; `bench/`: run, chaos and retry scripts

## Status

The gateway is validated against a simulator only. Planned: validation against real vLLM replicas, and a replicated (HA) gateway control plane.
