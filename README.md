# servemesh

[![ci](https://github.com/pandeylakshya207-max/servemesh/actions/workflows/ci.yml/badge.svg)](https://github.com/pandeylakshya207-max/servemesh/actions/workflows/ci.yml)

A cache-aware, fault-tolerant gateway for LLM serving, written in Go. It sits in front of several model replicas, exposes an OpenAI-style streaming API, and decides which replica serves each request.

## Features

- Streaming reverse proxy for `/v1/chat/completions` (SSE).
- Pluggable routing policies: round-robin, least-loaded, and prefix-aware (routes to the replica most likely to hold the prompt's prefix in its KV cache, with a bounded-load cap and rendezvous-hash placement for cold prefixes).
- Active and passive health checking, retry on a different replica before the first byte, and explicit error events when a stream fails midway.
- A mock replica with a simulated prefix cache, and an open-loop (Poisson) benchmark harness with seeded workloads.

## Results (simulated)

On a 3-replica simulation, prefix-aware routing raised prefix-cache hit rate from about 28% to 61% to 70% on shared-system-prompt traffic and cut median time-to-first-token by more than 90%. The p99 improvement was smaller (about 16% at 10 rps, about 50% at 25 rps), there was no p99 gain on multi-turn traffic, and there was a small p99 cost (about 6%) on traffic with nothing to cache. A backend killed under load failed only the 3 of 400 requests already streaming from it.

These are simulator results, not real GPU measurements. Full tables, caveats and limitations: [docs/RESULTS.md](docs/RESULTS.md).

## Quick start

```powershell
go test ./...
go build -o bin/mockbackend.exe ./cmd/mockbackend
go build -o bin/gateway.exe ./cmd/gateway

.\bin\mockbackend.exe -id=m0 -addr=:9000
.\bin\mockbackend.exe -id=m1 -addr=:9001
.\bin\gateway.exe -policy=prefix-aware -backends=m0=localhost:9000,m1=localhost:9001
```

## Layout

- `internal/router`: routing policies
- `internal/proxy`: streaming gateway, health checks
- `internal/backend`: mock replica with a simulated prefix cache
- `cmd/bench`: load generator; `bench/`: run, chaos and retry scripts

## Status

The gateway is validated against a simulator only. Planned: validation against real vLLM replicas, and a replicated (HA) gateway control plane.
