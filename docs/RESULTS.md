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

Motivation: with rendezvous placement, prefix-aware was about 6% worse than least-loaded at p95 and p99 on uncacheable traffic. Hypothesis: when no replica holds any block of the prompt, a fixed hash placement ignores current load. The gateway gained a `-cold-fallback` flag: `rendezvous` (stable hash of the first block) or `least-loaded`.

Method notes:

- Setup is as above, 3 seeds. Baselines were re-run in the same session, so least-loaded numbers differ slightly from the earlier tables (its hit rate varies by a few points between sessions).
- Two runs were discarded and re-run: least-loaded on shared-system (seed 1, 25 errors) and prefix-aware with the least-loaded fallback on multiturn (seed 2, 76 errors). In both, the gateway refused connections for part of the measured window and latencies were 3 to 4 times normal. The gateway process was still alive afterwards. The cause is unknown; contention on the single test machine is a guess, not a finding. All reported numbers come from clean runs.
- After the first measurement I found a flaw in the least-loaded fallback: exact ties always went to the first replica in the list. Ties are now broken with the rendezvous hash. All least-loaded-fallback rows below are measured after that fix. Before it, the same cells read: shared-system 10 rps 67.6% hit / p95 1052 / p99 1137, 25 rps 72.6% / 1572 / 1720, multiturn 33.9% / 1499 / 1979. These differ from the table below by a few points in either direction, which with 3 seeds cannot be separated from run-to-run noise.
- The failure experiments earlier in this file used the rendezvous fallback.

| Workload | Policy | Hit rate | TTFT p50 (ms) | p95 | p99 |
|---|---|---|---|---|---|
| random | round-robin | 0% | 546 | 696 | 851 |
| random | least-loaded | 0% | 536 | 620 | 649 |
| random | prefix-aware (rendezvous) | 0% | 537 | 659 | 691 |
| random | prefix-aware (least-loaded fallback) | 0% | 536 | 620 | 649 |
| shared-system, 10 rps | round-robin | 28.4% | 1139 | 1427 | 1486 |
| shared-system, 10 rps | least-loaded | 31.5% | 1082 | 1312 | 1455 |
| shared-system, 10 rps | prefix-aware (rendezvous) | 61.3% | 99 | 1196 | 1255 |
| shared-system, 10 rps | prefix-aware (least-loaded fallback) | 64.6% | 96 | 1080 | 1166 |
| shared-system, 25 rps | round-robin | 28.6% | 2920 | 4135 | 4393 |
| shared-system, 25 rps | least-loaded | 27.9% | 2945 | 3986 | 4136 |
| shared-system, 25 rps | prefix-aware (rendezvous) | 69.9% | 148 | 1836 | 2206 |
| shared-system, 25 rps | prefix-aware (least-loaded fallback) | 71.0% | 140 | 1549 | 1746 |
| multiturn, 10 rps | round-robin | 13.3% | 806 | 1846 | 2280 |
| multiturn, 10 rps | least-loaded | 9.8% | 823 | 1830 | 2226 |
| multiturn, 10 rps | prefix-aware (rendezvous) | 28.0% | 578 | 1675 | 2265 |
| multiturn, 10 rps | prefix-aware (least-loaded fallback) | 32.5% | 458 | 1506 | 1922 |

Findings:

- On `random` the least-loaded fallback removes the cost: p95 620 and p99 649, identical to plain least-loaded (rendezvous: 659 and 691). This supports the hypothesis.
- Versus rendezvous, the least-loaded fallback is better or equal in every cell: hit rate +1.1 to +4.5 points on cacheable workloads, p95 6% to 16% lower, p99 6% to 21% lower. My prediction before running this was that it would hurt the shared-system workload; it did not.
- Versus plain least-loaded, the least-loaded fallback has 2.1x (shared-system, 10 rps), 2.5x (25 rps) and 3.3x (multiturn) the hit rate, 91%, 95% and 44% lower median TTFT, and 20%, 58% and 14% lower p99.

Possible explanation for the shared-system gain (not verified): the 16 system prompts take about 25 cache blocks each, so a 150-block cache holds at most 6. With hash placement of 16 prompts over 3 replicas, the chance that no replica gets more than 6 is about 26%, so most assignments overload at least one replica. Load-aware placement may spread the prompts more evenly. Actual placement was not measured.

Remaining limitations of the least-loaded fallback:

- `Pick` and `Acquire` are not atomic, so simultaneous cold arrivals can pick the same replica.
- Least-loaded placement depends on local load, so several gateway replicas, each seeing only its own in-flight counts, would not agree on where a prefix lives. Rendezvous placement would. This matters for a replicated gateway.
- 3 seeds and a few hundred requests per run: p99 differences under about 10% are within noise. Hit-rate and p95 differences are the more reliable signal.
- Everything here is simulated.

The library and command-line default remain `rendezvous`. `-cold-fallback=least-loaded` is the measured-better setting for a single gateway.

## Real engine: llama.cpp on CPU

The routing study above used a simulator. This section runs the same gateway in front of three real llama-server replicas.

Setup:

- llama.cpp release b11429, model Qwen2.5-0.5B-Instruct (Q4_K_M), 3 replicas on one Windows laptop, each with 4 slots and 3 CPU threads. The gateway probes `/health` and its response-header timeout is raised to 120 s.
- Sequential single-client replay (`cmd/replay`): 100 requests, the first 12 excluded, 6 distinct system prompts of 60 random words each, a unique 15-word user message, 8 output tokens. Each prompt is about 530 model tokens.
- For a given seed the request sequence is identical for every policy, so comparisons are paired. 5 seeds, 88 measured requests per run, 440 per policy.
- Cache hits come from llama.cpp's own per-request counters (`cache_n` reused tokens, `prompt_n` processed tokens). Warm means the cache covered at least half of the prompt.
- All 10 runs had 0 request errors, 0 retries, 0 upstream timeouts and 3 healthy replicas.

Results (mean over 5 seeds):

| | round-robin | prefix-aware (least-loaded fallback) |
|---|---|---|
| Cache hit rate (range) | 71.5% (69.9 to 72.6) | 78.8% (78.0 to 79.7) |
| Mean prompt tokens prefilled per request | 151 | 112 |
| Cold requests | 46 of 440 (10.5%) | 5 of 440 (1.1%) |
| TTFT p50 | 998 ms | 997 ms |
| TTFT p95 | 3933 ms | 1125 ms |

Findings:

- Prefix-aware routing had the higher hit rate on all 5 paired seeds, with non-overlapping ranges, and prefilled 26% fewer prompt tokens. Its hit rate was reproducible: seed 1 gave 78.0% and 117 tokens in two separate sessions.
- Median TTFT was unchanged (998 vs 997 ms; the per-seed differences were within 3% and of mixed sign). Most requests are warm under both policies, and a warm request still pays about 1 s to prefill its own 110 to 150 unique tokens.
- p95 TTFT fell 71%. The cause is fewer cold requests (10.5% to 1.1%); a cold request costs about 3.9 s, about 4 times a warm one. This is partly a threshold effect: round-robin has more than 5% cold requests, so its p95 falls among them, while prefix-aware has about 1%, so its p95 falls among warm ones. A rough mean TTFT from the warm/cold split is about 1.3 s vs 1.03 s (about 20% lower); this is an estimate, since the tool does not report a mean.
- Absolute latency drifted upward by about 10% over the session in both policies (p50 about 930 to 1040 ms), so only within-seed comparisons are meaningful.

Compared with the simulator, the real engine behaved very differently. Round-robin reached a 71.5% hit rate here versus about 28% in the simulator, and the gain from prefix-aware routing is about 7 points rather than a doubling. My prediction before running was that round-robin would hit about 52%, because 6 prompts should not fit in 4 slots; the observed warm fraction (89.5%) shows llama.cpp retains more than its slot count. The cause is not verified (one possibility is a host-memory prompt cache in recent llama-server builds). The simulator's cache model was harsher than this engine's on this workload, so its large gains should not be read as predictions for real engines.

Limitations:

- The client is sequential, so there is no queueing. This measures cache effects only, not load balancing or capacity. Open-loop runs at 0.1 to 0.5 requests/s on this machine were dominated by CPU contention and occasional stalls of unknown cause (three servers, the gateway and the load generator share one laptop) and are not reported. An earlier open-loop run (1 request/s) showed 27 retries and 5 gateway 502s. My reading, not confirmed by tracing, is that the hard-coded 30 s header timeout made the gateway treat queued but alive replicas as dead, ejecting and retrying them. Slow responses now return 504 without ejection or retry, and the timeout is configurable; later runs also used much lower load, so how much of the improvement is due to the fix was not separated.
- One small model, CPU only, one machine. Tested with 6 and 12 prompt groups (see the follow-up below); neither pressured the cache enough to hurt round-robin.
- Least-loaded was not compared: with one request in flight at a time every replica reports zero load, so it always picks the first.
- The gateway's prefix tracker hashes blocks of whitespace-separated words, not model tokens; it only needs identical prefixes to hash identically.
- Not tested on a GPU or on vLLM.

### Follow-up: 12 prompt groups

Same setup, but 12 distinct system prompts and 140 requests per run (first 40 excluded), 3 seeds, 300 measured requests per policy. All 6 runs had 0 errors, 0 retries, 0 upstream timeouts and 3 healthy replicas.

| | round-robin | prefix-aware (least-loaded fallback) |
|---|---|---|
| Cache hit rate (per seed) | 72.7% (73.5, 71.9, 72.7) | 79.3% (78.2, 79.8, 79.8) |
| Mean prompt tokens prefilled per request | 145 | 110 |
| Cold requests | 27 of 300 (9.0%) | 2 of 300 (0.7%) |
| TTFT p50 | 975 ms | 1002 ms |
| TTFT p95 | 3869 ms | 1178 ms |

Doubling the number of groups changed almost nothing: round-robin's hit rate went from 71.5% to 72.7% and its cold share from 10.5% to 9.0%; the hit-rate gap went from 7.3 to 6.6 points and the p95 reduction stayed at about 70%. My earlier expectation that round-robin would degrade with more groups than slots was not supported at 12 groups.

Background, checked against the llama.cpp sources: since PR #16391 (October 2025), llama-server has a host-memory prompt cache that acts as extra slots, controlled by `--cache-ram`; this build (b11429) postdates it. It would explain why replicas retain far more than their 4 slots, but whether it was active in these runs and its default size were not verified. The next experiment disables it.

### Follow-up: host-memory prompt cache disabled (`--cache-ram 0`)

Same 12-group setup, 140 requests per run (first 40 excluded), but every llama-server replica was started with `--cache-ram 0`, which I expected to disable the host-memory prompt cache (the server logs were not inspected, so this is inferred from the effect). 3 seeds per policy.

| | round-robin | prefix-aware (least-loaded fallback) |
|---|---|---|
| Cache hit rate, seed 1 / 2 / 3 | 31.1 / 29.3 / 20.6% | 39.5 / 56.9 / 67.9% |
| Cache hit rate, mean | 27.0% | 54.8% |
| Mean prompt tokens prefilled per request | 387 | 240 |
| Cold requests | 200 of 299 (66.9%) | 95 of 300 (31.7%) |
| TTFT p50 | 3494 ms | 1749 ms |
| TTFT p95 | 4084 ms | 4063 ms |

Findings:

- With the host cache disabled, prefix-aware routing had the higher hit rate on all 3 seeds (mean 54.8% vs 27.0%), prefilled 38% fewer prompt tokens, cut the cold-request share from 67% to 32%, and halved median TTFT. With the default configuration the gap was 6.6 points (72.7% vs 79.3%). Cache-aware routing therefore matters much more when per-replica cache is scarce.
- Disabling the flag cut round-robin's hit rate from 72.7% to 27.0%, so the default configuration was retaining far more than the 4 slots. This matches the host-memory prompt cache added in llama.cpp PR #16391; the server logs were not checked.
- My prediction before running was that prefix-aware would stay near 79%. It averaged 54.8% and varied from 39.5% to 67.9% across seeds. A possible cause (not measured) is that seeds change how the 12 prompts spread over the replicas, and an uneven split overflows the 4 slots on some replica. With 3 seeds the spread is not explained.
- p95 TTFT is about equal (4063 vs 4084 ms) because both policies have more than 5% cold requests, so the p95 lands among cold requests in both.

Data-quality notes:

- The first prefix-aware seed-2 run took about 3 hours of wall-clock time (result files written at 12:23 and 15:25) and had 2 requests hit the replay client's 180 s timeout. Windows Kernel-Power events at 12:26 and 15:23 (which I believe mark entering and leaving Modern Standby) match the gap, so I attribute the stall to laptop sleep. The run was repeated with sleep disabled; it ran normally and reproduced the earlier numbers (hit rate 56.9% vs 56.5%, 228 vs 231 prompt tokens prefilled), and the repeated run is the one reported above.
- One round-robin request (seed 1, 12:10, before the standby period) also hit the 180 s client timeout; its cause is unknown, and that run's 99 measured requests are reported as is.
- In one run the gateway logged one connection EOF from a replica, retried the request successfully, and the replica was healthy again 2 seconds later. A possible cause is a stale keep-alive connection: llama-server's keep-alive timeout appears to be 5 s while the gateway keeps idle connections for 90 s. Not verified.
