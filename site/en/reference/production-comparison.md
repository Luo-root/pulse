# Pulse/Durable, Eino and Temporal: one comparison set

This round keeps **Pulse (with Durable for reliable execution), Eino and Temporal** throughout execution, observation, resources, storage and recovery. Go Taskflow does not enter the new comparison; the [older baseline and optimization data](./performance.md) remain historical.

Pulse has the lowest in-memory execution cost for this local integer DAG; Eino grows substantially at ten thousand nodes; Temporal retains persistence, service and Activity costs. These are measured workload costs, not an overall production ranking between an in-memory library and a durable service. This test does not score Eino's Agent/component ecosystem or Temporal's deployment and distributed execution features.

## Versions, samples and guarantees

Measured on 2026-10-17, using the cleaned, merged public versions. Pulse commit `aa02dd107a43845b1790e9c2d1e1a41fe5a53839`, Durable `71c693fc82ef24731c84c2c0aa9edf4c02d1e63e`; neither is a newly published tag. No local replace.

| Component | Pinned identity |
|---|---|
| Pulse | v0.4.2-0.20261017111210-aa02dd107a43 |
| Durable | v0.0.0-20261017105818-71c693fc82ef |
| Eino | v0.9.21 |
| Temporal SDK / CLI / Server | v1.50.0 / 1.9.1 / 1.32.0 |
| Benchmark Go / Server build Go | go1.26.0 / go1.26.8 |
| Host SQLite driver | v1.60.1 |


Windows 11 / i9-14900HX; GOMAXPROCS=4 per Go process, including Server, and business concurrency=4. This is not a four-core affinity limit shared across processes. chain/wide/diamonds, N=1/10/100/1000/10000, rounds=1/256. One node is one real task/regular Activity with actual direct-predecessor values. Per-node values, counts, concurrency and reuse are checked before freezing a non-race binary and rotating cases serially.

| Experiment | Planned / retained | ok | deadline | hard_timeout | not_repeated |
|---|---|---|---|---|---|
| native | 900 / 900 | 800 | 1 | 19 | 80 |
| resources | 81 / 81 | 75 | 1 | 1 | 4 |
| checkpoint | 45 / 45 | 36 | 3 | 0 | 6 |
| recovery | 36 / 36 | 36 | 0 | 0 | 0 |


All 1062 planned identities remain: 234 groups, 947 completed, 25 time limits and 90 not_repeated, with zero business-checksum errors. Native plain/observe uses five repeated batches; other experiments use three repeats. The 60s context includes setup/first run; the 65s parent watchdog kills the contained process tree. After the first limit, remaining repeats are not attempted. `—` means no completed cost, never zero; progress is not invented when hard-timeout output is incomplete.

| Dimension | Pulse / Durable | Eino | Temporal |
|---|---|---|---|
| Native execution | In-memory Pulse, fresh Execution per run | compose Graph, AllPredecessor, unique output keys | Real Server/worker, always native persistence |
| Observation | Observer lifecycle counters | Graph/Lambda start/end/error callbacks | Non-replay Workflow/Activity interceptors |
| Reliable confirmation | Durable Store confirmation before publication | No automatic per-node save on normal success; active CheckPointStore interrupt separately | Native history and Activity completion confirmation |
| Recovery | Kill host after confirmed prefix; new-process Resume | Exit after active checkpoint; new-process resume | Confirmed prefix without in-flight Activity, worker kill; Server restart separately |

Eino's host semaphore limits business bodies. Its host cancels and joins those bodies after errors, without promising all engine goroutines have exited. Temporal sets MaximumAttempts=1 and acquires the workflow semaphore after dependencies, before scheduling Activities.

## Native execution and preparation

The following rounds=1 numbers are ms, medians of five repeated reuse-batch means. Windows uses QPC to resolve short intervals. CSV retains min–max, both workloads, first runs and setup. These are not online p50s or confidence intervals.

| shape / N | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| chain / 1 | 0.0043375 | 0.0065289 | 253.826 |
| chain / 100 | 0.262107 | 0.613691 | 5049.45 |
| chain / 1000 | 2.81535 | 38.6123 | — |
| chain / 10000 | 31.2935 | 4867 | — |
| wide / 1 | 0.0043377 | 0.0062323 | 229.887 |
| wide / 100 | 0.170895 | 0.714113 | 1374.74 |
| wide / 1000 | 1.87012 | 39.9403 | 11183.6 |
| wide / 10000 | 19.2415 | 4824.91 | — |
| diamonds / 1 | 0.00406 | 0.0064852 | 225.973 |
| diamonds / 100 | 0.282539 | 0.834692 | 3400.07 |
| diamonds / 1000 | 2.87873 | 40.5372 | — |
| diamonds / 10000 | 34.214 | 4796.76 | — |


![Native reuse cost](/performance/2026-10-17-production/native_reuse.png)

Temporal chain/diamonds N=1000 and every N=10000 configuration did not finish the whole case; no runtime is extrapolated and no wrong-result claim follows. Wide N=1000 completed. All Pulse/Eino native plain/observe plans completed. This tiny-task difference is not a proportional speedup for complete Agent applications. No profiles were collected this round, so curve changes are not attributed to proven internal hotspots.

| N=100 chain / stage | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| server_setup_ns | 0 | 0 | 364.083 |
| adapter_setup_ns | 0.0648 | 0.4919 | 66.1348 |
| first_ns | 0.4939 | 1.035 | 4186.82 |
| run_ns | 0.262107 | 0.613691 | 5049.45 |


Stage responsibilities differ: Pulse builds a graph and validates on first start; Eino assembles/compiles; Temporal adapter setup connects/registers/starts a worker, with Server preparation separate. First execution covers submission through correct-result confirmation; reuse is subsequent. Do not subtract stage medians for attribution. Temporal persistence is retained rather than presented as an equivalent in-memory guarantee.

## Observation

| N=100 / shape | Pulse plain → observe (ms) | Eino plain → observe (ms) | Temporal plain → observe (ms) |
|---|---|---|---|
| chain | 0.262107 → 0.271002 | 0.613691 → 0.662909 | 5049.45 → 5052.55 |
| wide | 0.170895 → 0.173396 | 0.714113 → 0.741753 | 1374.74 → 1301.09 |
| diamonds | 0.282539 → 0.283872 | 0.834692 → 0.881922 | 3400.07 → 3398.72 |


![Plain and observe costs](/performance/2026-10-17-production/observability.png)

All use public extension hooks with atomic counters and no external logging. Coverage and event counts differ; differences are not prices for identical features. Some observe medians are lower: these are independently sampled variations, not evidence that observation accelerates execution. CSV retains event types/counts and ranges.

## Equal-work CPU and memory

A separate resource experiment performs one first run plus one reuse for every framework, including process startup, preparation and GC. Temporal's independent Server belongs to the Windows Job. The table uses chain/rounds=1; CPU is seconds and RSS is MiB.

| chain / N | Pulse CPU s / RSS MiB | Eino CPU s / RSS MiB | Temporal CPU s / RSS MiB |
|---|---|---|---|
| 1 | 0.015625 / 18.3594 | 0.015625 / 17.793 | 0.609375 / 170.289 |
| 100 | 0.015625 / 18.3906 | 0.03125 / 17.7969 | 2.53125 / 213.496 |
| 1000 | 0.015625 / 18.4102 | 0.140625 / 26.9883 | — / — |


![Equal-work CPU and sampled RSS](/performance/2026-10-17-production/resources.png)

Job CPU includes exited descendants. Local process CPU accounting is quantized at roughly 15.625ms; equal small values do not prove identical CPU work. RSS is the largest simultaneous tree sum observed every 25ms and may miss a short peak. The common binary links all adapters: this includes a common harness baseline, not minimal standalone deployment footprints. Incomplete-case resources remain in raw data rather than completed-cost statistics.

CSV also retains cumulative process-local Go reuse allocations. Temporal Server is excluded from those counters, so they cannot rank whole-engine memory. Cumulative allocations, live heap and RSS differ. Native throughput batches and equal-work resource cases have different iteration counts; their total CPU is not mixed.

## Reliable execution and storage growth

SQLite uses WAL/FULL with native checkpoint/transaction granularity retained. Durable and Eino access benchmark SQLite hosts via existing Store contracts; these are not new official production backends. Durable CAS-saves complete RunRecord JSON and reuses filestore OS locks, without distributed leases or fencing. Temporal is a real single-machine CLI Service; [production deployment](https://docs.temporal.io/production-deployment) still requires a production Service. This does not test clusters, HA or production databases. Native cases explicitly raise history-count error to 200000 and warn to 100000; these are not default-configuration scores.

The same three columns below show chain/rounds=1 reliable reuse. Durable uses three repeats; Temporal references the same native durable points above (five batches), not a fabricated second equivalent experiment.

| chain / N | Pulse + Durable (ms) | Eino | Temporal (ms) |
|---|---|---|---|
| 1 | 7.1326 | Active interrupt only | 253.826 |
| 10 | 45.5552 | Active interrupt only | 197.664 |
| 100 | 503.488 | Active interrupt only | 5049.45 |
| 1000 | 8553.19 | Active interrupt only | — |
| 10000 | — | Active interrupt only | — |


Durable N=10000 hit the 60s whole-case budget in all three shapes; remaining repeats were not run. Ten-thousand-step reliable scale is therefore not accepted by this experiment. Eino's column means no equivalent ordinary per-node confirmation was measured, not that its extension mechanisms cannot support persistence.

For N=100 chain, median DB/WAL/SHM lengths are Durable 45136B after preparation → 4230648B after first run → 4329168B after reuse; Temporal 2356456B → 5053944B → 5250648B. They retain two separate RunIDs/Workflow histories, initial metadata and reused WAL space. Differences are not cumulative physical writes or bytes per task. Eino ordinary cases do not enable Store: actual 0B is not zero cost for equivalent reliable work; active checkpoint blobs are validated in recovery.

## Cross-process recovery

chain/rounds=256; confirm five nodes for N=10 and ten for N=100/1000. The parent independently reads Pulse SQLite plus execution audit, reads/hashes Eino's saved blob, or uses an independent Temporal client to verify completed-prefix history with no in-flight Activity before exit/kill. Three repeats per scenario, 36/36 completed, checking final checksum and no prefix rerun.

| N / prefix | Pulse host kill (s) | Eino active interrupt (s) | Temporal worker kill (s) | Temporal + Server restart (s) |
|---|---|---|---|---|
| 10 / 5 | 0.150567 | 0.0454763 | 5.17285 | 0.663576 |
| 100 / 10 | 0.293597 | 0.0549201 | 8.62182 | 4.02771 |
| 1000 / 10 | 4.69759 | 0.126953 | 53.6735 | 49.0806 |


![Recovery separated by guarantee](/performance/2026-10-17-production/recovery.png)

Seconds from beginning exit/relaunch after confirmed prefix through correct-result confirmation, including remaining business. CSV operation_ns separately measures the final CLI operation, excluding earlier controller relaunch work. Eino is active-interrupt recovery, not arbitrary-crash recovery; none of these tests proves exactly-once in-flight external effects.

Temporal's Server-alive scenario is slower than Server-restarted here; unfavorable/surprising results remain. Post-exit routing or sticky-cache behavior would require separate controlled diagnosis; no causal claim or recommendation to restart production Servers follows. Native cases need a first run plus full reuse, while recovery only completes the remaining suffix. Successful N=1000 recovery therefore does not contradict a native whole-case timeout.

## Data, reproduction and checks

[Compact CSV](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-production/summary.csv) contains all 234 groups with statuses/nulls/allocations/storage/events. [Identity](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-production/identity.json) pins commits, raw hashes and scopes. [Bilingual reproduction](https://github.com/Luo-root/pulse/tree/main/benchmarks/production) supplies commands/tests; summarize_production.py regenerates figures from retained raw data. Raw JSONL, stdout/stderr, full machine/build inventories, diagnostics and databases stay outside Git; evidence is delivered separately, without invented online raw links.

Merged Core tree equality, latest public Core with Durable build/vet/full race and real process recovery passed. New adapters passed full race/vet, live Temporal failure/cancellation reuse/history concurrency checks, sampler counterexamples and independent review. Collection used unchanged binary/script hashes and serial stages. Older Go1.25 results are not overwritten or mixed into upgrade improvements. No tag/release was created.

Pinned sources: [Eino v0.9.21](https://github.com/cloudwego/eino/tree/v0.9.21), [Temporal SDK v1.50.0](https://github.com/temporalio/sdk-go/tree/v1.50.0), [CLI v1.9.1](https://github.com/temporalio/cli/releases/tag/v1.9.1).
