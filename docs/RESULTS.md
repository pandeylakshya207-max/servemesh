# servemesh results

Every number here comes from a simulator, not real GPUs. Read the Limitations section before quoting any of it.

## Setup

- 3 mock replicas (`cmd/mockbackend`) with a simulated prefix cache: 16-token blocks, LRU eviction, 150 blocks per replica. Prefill costs 2 ms per uncached prompt token. Decode costs 10 ms per token at zero load, plus 10% per additional in-flight request. 32 output tokens per request.
- The gateway (`cmd/gateway`) runs one routing policy at a time in front of the same replicas.
- Load: open-loop Poisson arrivals (`cmd/bench`), 30 s per run with the first 5 s excluded, seeds 1 to 3. Hit rate is cached prompt tokens divided by prompt tokens, as reported by the mock.
- Workloads:
  - `shared-system`: 16 distinct system prompts of about 400 words, each request adds a unique 30-word question.
  - `multiturn`: 50 conversations, each request resends the growing history.
  - `random`: unique 200-word prompts. This is the control; nothing can be cached.
- The load generator, gateway and replicas all ran on one Windows laptop and shared its CPU.

## Routing results (mean of 3 seeds)

The prefix-aware rows below use rendezvous (hash) placement for cold prefixes, the original default. A better variant is described in the cold-fallback experiment at the end of this file.

| Workload | RPS | Policy | Hit rate | TTFT p50 (ms) | TTFT p99 (ms) |
|---|---|---|---|---|---|
| shared-system | 10 | round-robin | 28.4% | 1139 | 1486 |
| shared-system | 10 | least-loaded | 27.0% | 1138 | 1487 |
| shared-system | 10 | prefix-aware | 60.9% | 99 | 1255 |
| shared-system | 25 | round-robin | 28.6% | 2920 | 4393 |
| shared-system | 25 | least-loaded | 27.9% | 2945 | 4136 |
| shared-system | 25 | prefix-aware | 69.9% | 148 | 2206 |
| multiturn | 10 | round-robin | 13.3% | 806 | 2280 |
| multiturn | 10 | least-loaded | 9.8% | 823 | 2226 |
| multiturn | 10 | prefix-aware | 28.0% | 578 | 2265 |
| random | 10 | round-robin | 0.0% | 546 | 851 |
| random | 10 | least-loaded | 0.0% | 536 | 648 |
| random | 10 | prefix-aware | 0.0% | 537 | 691 |

No run had request errors. Round-robin's p99 on `random` is inflated by one run (seed 3: 1272 ms).

### What the numbers show

- **Hit rate is 2.1x to 2.9x higher** with prefix-aware routing on both cacheable workloads.
- **Median TTFT drops a lot on shared-system:** about 91% at 10 rps and 95% at 25 rps. On multiturn it drops about 28% to 30%.
- **The tail benefit is smaller and depends on load.** On shared-system, p99 TTFT is about 16% lower at 10 rps and 47% to 50% lower at 25 rps. On multiturn there is no p99 improvement (+2% versus least-loaded, -1% versus round-robin; seed 2 was the worst case for prefix-aware).
- **On uncacheable traffic, prefix-aware has a small cost:** the median is identical, but p99 is about 6.6% higher than least-loaded, and higher on all three seeds (+6.2%, +7.3%, +6.7%). A plausible but untested cause: with no cached match, the policy falls back to a fixed rendezvous-hash placement that ignores current load.
- Round-robin and least-loaded are indistinguishable from each other.

### Caveats on these numbers

- Each run measures only about 250 requests at 10 rps (about 600 at 25 rps), so a p99 rests on 2 to 6 requests. Treat p99 differences under about 10% as noise.
- n = 3 seeds. Differences between seeds were small for hit rate and larger for p99.
- Multiturn hit rates are low because 50 growing conversations do not fit in three 150-block caches.
- TTFT magnitudes depend on the mock's cost model. Hit rate is the more robust metric.

## Failure behavior

### Backend killed under load

`bench/chaos.ps1`: 10 rps for 45 s (40 s measured, seed 1), `m1` hard-killed at t=15 s and restarted 12 s later with an empty cache.

| | prefix-aware | least-loaded |
|---|---|---|
| Stopped routing to `m1` after | 189 ms | 74 ms |
| Back in rotation after restart | 1511 ms | 1876 ms |
| Requests OK / failed | 400 / 3 | 400 / 3 |
| Failure type | 3 stream error events | 3 stream error events |
| Hit rate | 57.4% | 30.8% |
| TTFT p50 / p99 (ms) | 107 / 1921 | 1140 / 1834 |

- Only requests already streaming from `m1` failed (0.7%). They received an explicit error event, not a silently truncated stream. Every request that arrived after the kill succeeded.
- `retries` was 0 here: in-flight streams failing ejected `m1` before any new request tried to connect to it. The retry path is exercised separately below.
- Prefix-aware degraded under failure: its p99 rose from about 1313 ms in the clean seed-1 run to 1921 ms, and its hit rate dipped from 61.6% to 57.4% (the measurement windows differ, 25 s versus 40 s, so this is approximate). Least-loaded's p99 rose about 11%. At p99 the two policies were roughly equal (1921 versus 1834 ms), but with 400 requests that is only about 4 samples.

### Retry before the first byte

`bench/retry-demo.ps1`: active health checks disabled, `m1` killed while idle, 9 sequential requests through round-robin. All 9 returned 200. One request was routed to the dead `m1`, retried on `m0` (`X-Gateway-Attempts: 2`), and `m1` was then ejected. Gateway counters: `retries=1`, `midstream_failures=0`.

### Design

- Active health checks every 1 s: eject after 2 consecutive failures, restore after 2 consecutive successes.
- Passive ejection: a connection failure or 5xx marks a backend unhealthy immediately.
- Up to 3 backends are tried per request, but only before the first response byte reaches the client. After that the failure is reported in-stream, because regenerated tokens would differ.
- A client disconnect is never counted against a backend.
- The prefix-aware policy forgets an ejected backend's cache contents, since a restarted replica comes back cold.

## Limitations

- **Simulator only.** No real model, GPU or vLLM was involved. The gateway's cache tracker uses the same block-hashing scheme as the mock, so it has unrealistically accurate knowledge of each replica's cache. Real replicas hash and evict differently, so absolute hit rates will not carry over.
- **Single gateway.** It is a single point of failure.
- **One load level per workload** (two for shared-system), one machine, 3 seeds.
- **The tracker is approximate.** It records a routing decision even if the request later fails.
- **No mid-stream resume.** A stream that fails after the first byte is not recovered.
- **Hit rate is below its ceiling.** On shared-system the shared prefix is about 90% of the prompt, yet hit rate is 61% to 70%. The cause (unique question tails evicting shared blocks, cold start, uneven group placement) has not been investigated.

## Reproduce

```powershell
foreach ($w in "shared-system","multiturn","random") {
  foreach ($seed in 1,2,3) {
    foreach ($p in "round-robin","least-loaded","prefix-aware") {
      powershell -ExecutionPolicy Bypass -File bench/run.ps1 -Policy $p -Workload $w -Rps 10 -Seed $seed
    }
  }
}
powershell -ExecutionPolicy Bypass -File bench/summarize.ps1
powershell -ExecutionPolicy Bypass -File bench/chaos.ps1 -Policy prefix-aware
powershell -ExecutionPolicy Bypass -File bench/retry-demo.ps1
```

## Experiment: cold-prefix fallback (rendezvous vs least-loaded)

Motivation: with rendezvous placement, prefix-aware was about 6% worse than least-loaded at p95 and p99 on uncacheable traffic. Hypothesis: when no replica holds any block of the prompt, a fixed hash placement ignores current load. The gateway gained a `-cold-fallback` flag: `rendezvous` (stable hash of the first block, the default) or `least-loaded`.

Setup is as above, 3 seeds. The baselines were re-run in the same session, so least-loaded numbers differ slightly from the tables above (its hit rate varies by a few points between sessions). One least-loaded run (shared-system, seed 1) was discarded because the gateway refused connections for part of the measured window (25 errors, p50 3312 ms); it was re-run cleanly after `bench/run.ps1` gained readiness checks. The cause of the original failure is unknown.

| Workload | Policy | Hit rate | TTFT p50 (ms) | p95 | p99 |
|---|---|---|---|---|---|
| random | round-robin | 0% | 546 | 696 | 851 |
| random | least-loaded | 0% | 536 | 620 | 649 |
| random | prefix-aware (rendezvous) | 0% | 537 | 659 | 691 |
| random | prefix-aware (least-loaded fallback) | 0% | 525 | 620 | 648 |
| shared-system, 10 rps | round-robin | 28.4% | 1139 | 1427 | 1486 |
| shared-system, 10 rps | least-loaded | 31.5% | 1082 | 1312 | 1455 |
| shared-system, 10 rps | prefix-aware (rendezvous) | 61.3% | 99 | 1196 | 1255 |
| shared-system, 10 rps | prefix-aware (least-loaded fallback) | 67.6% | 95 | 1052 | 1137 |
| shared-system, 25 rps | round-robin | 28.6% | 2920 | 4135 | 4393 |
| shared-system, 25 rps | least-loaded | 27.9% | 2945 | 3986 | 4136 |
| shared-system, 25 rps | prefix-aware (rendezvous) | 69.9% | 148 | 1836 | 2206 |
| shared-system, 25 rps | prefix-aware (least-loaded fallback) | 72.6% | 140 | 1572 | 1720 |
| multiturn, 10 rps | round-robin | 13.3% | 806 | 1846 | 2280 |
| multiturn, 10 rps | least-loaded | 9.8% | 823 | 1830 | 2226 |
| multiturn, 10 rps | prefix-aware (rendezvous) | 28.0% | 578 | 1675 | 2265 |
| multiturn, 10 rps | prefix-aware (least-loaded fallback) | 33.9% | 452 | 1499 | 1979 |

Findings:

- On `random` the least-loaded fallback closes the gap: p95 620 and p99 648, versus 620 and 649 for plain least-loaded (rendezvous: 659 and 691). This supports the hypothesis.
- On `shared-system` it beat rendezvous in every cell and on every seed, which is the opposite of what I predicted. Hit rate rose 2.7 to 6.3 points, p95 fell 10% to 14%, and p99 fell 9% to 22%. Versus plain least-loaded, p99 is 22% lower at 10 rps and 58% lower at 25 rps.
- On `multiturn` it improved p99 (1979 ms versus 2265 for rendezvous and 2226 for least-loaded) on all 3 seeds. The earlier "no p99 gain on multiturn" result applies only to the rendezvous variant.

Possible explanation (not verified): the 16 system prompts take about 25 cache blocks each, so a 150-block cache holds at most 6. With hash placement of 16 prompts over 3 replicas, the chance that no replica gets more than 6 is about 26%, so most hash assignments overload at least one replica. Load-aware placement may spread the prompts more evenly. Actual placement was not measured.

Remaining limitations of the least-loaded fallback:

- At zero load all replicas tie and the first one in the list wins, so cold prefixes would concentrate on one replica when traffic is light. The benchmarks keep about 15 requests in flight at 10 rps and do not exercise this. A possible fix is to break ties with the rendezvous hash.
- `Pick` and `Acquire` are not atomic, so simultaneous cold arrivals can all pick the same replica.
- Rendezvous placement is deterministic, so several gateway replicas would agree without sharing state; least-loaded depends on local load. This matters for a replicated gateway.
- Sample sizes are the same as above (3 seeds, a few hundred requests per run), so p99 differences under about 10% are within noise. The hit-rate and p95 differences are consistent across seeds.

For these reasons the default remains `rendezvous`; use `-cold-fallback=least-loaded` for loaded deployments.
