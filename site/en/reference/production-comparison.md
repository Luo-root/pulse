# v0.5.0 pre-release remeasurement: Pulse/Durable, Eino and Temporal

> New collection: [2026-10-23 formal-release SQLite and recovery report](./sqlite-formal-20261023.md), pinning Core v0.5.0 and Durable/sqlitestore v0.1.0. Only reliable confirmation, the Temporal native-persistence baseline and recovery were rerun; status: COMPLETED_WITH_ERRORS, not a full multidimensional rerun. This page retains its historical identities and data.

This round keeps **Pulse (with Durable for reliable execution), Eino and Temporal** throughout execution, observation, resources, storage and recovery. Go Taskflow does not enter the new comparison; the [older baseline and optimization data](./performance.md) remain historical.

This page is generated from a new v0.5.0 pre-release collection: execution, resources, reliable confirmation and recovery of the same integer DAG. Native guarantees remain intact, rather than collapsing in-memory libraries and durable services into an overall production ranking. Eino's Agent/component ecosystem and Temporal's distributed deployment features are outside this short-task test.

## Versions, samples and guarantees

Collected at 2026-10-21 15:16–2026-10-21 17:13 UTC. Pulse candidate `be778722b781ad97f2846848dcd9dacdc0e501e7`, companion Durable `5db6eae0e9fb1cb743b56027b1cdadeb03736d1c`; exact public pseudo-versions, no local replace. **The v0.5.0 tag did not exist during measurement; this is not a publication claim.** These results apply to the release only if its final engine source matches this candidate; otherwise remeasure.

| Component | Pinned identity |
|---|---|
| Pulse | v0.4.2-0.20261021081643-be778722b781 |
| Durable | v0.0.0-20261021085955-5db6eae0e9fb |
| Eino | v0.9.21 |
| Temporal SDK / CLI / Server | v1.50.0 / 1.9.1 / 1.32.0 |
| Benchmark Go / Server build Go | go1.26.0 / go1.26.8 |
| Host SQLite driver | v1.60.1 |


Windows 11 / i9-14900HX; GOMAXPROCS=4 per Go process, including Server, and business concurrency=4. This is not a four-core affinity limit shared across processes. chain/wide/diamonds, N=1/10/100/1000/10000, rounds=1/256. One node is one real task/regular Activity with actual direct-predecessor values. Per-node values, counts, concurrency and reuse are checked before freezing a non-race binary and rotating cases serially.

| Experiment | Planned / retained | ok | deadline | hard_timeout | not_repeated | error |
|---|---|---|---|---|---|---|
| native | 900 / 900 | 784 | 6 | 19 | 90 | 1 |
| resources | 81 / 81 | 75 | 0 | 2 | 4 | 0 |
| checkpoint | 45 / 45 | 36 | 3 | 0 | 6 | 0 |
| recovery | 36 / 36 | 36 | 0 | 0 | 0 | 0 |


All 1062 planned identities were checked individually, with no missing or duplicate identities: 234 groups, 931 completed, 30 time limits and 100 not_repeated, with 1 retained measurement error(s) and zero checksum errors in successful results. Native plain/observe plans five repeated batches; other experiments plan three repeats. The 60s context includes setup/first run; the 65s parent watchdog kills the contained process tree. After the first limit, remaining repeats are not attempted. `—` means no completed cost, never zero; progress is not invented when hard-timeout output is incomplete.

| Dimension | Pulse / Durable | Eino | Temporal |
|---|---|---|---|
| Native execution | In-memory Pulse, fresh Execution per run | compose Graph, AllPredecessor, unique output keys | Real Server/worker, always native persistence |
| Observation | Observer lifecycle counters | Graph/Lambda start/end/error callbacks | Non-replay Workflow/Activity interceptors |
| Reliable confirmation | Durable Store confirmation before publication | No automatic per-node save on normal success; active CheckPointStore interrupt separately | Native history and Activity completion confirmation |
| Recovery | Kill host after confirmed prefix; new-process Resume | Exit after active checkpoint; new-process resume | Confirmed prefix without in-flight Activity, worker kill; Server restart separately |

Eino's host semaphore limits business bodies. Its host cancels and joins those bodies after errors, without promising all engine goroutines have exited. Temporal sets MaximumAttempts=1 and acquires the workflow semaphore after dependencies, before scheduling Activities.

One Temporal setup connection failure remains: plain/chain/N=1/rounds=1 repeat=3 was refused at 127.0.0.1:59472 before business execution, with zero iterations and no first/reuse time or checksum. The cause is undetermined and is not attributed to an engine. The other four repeats completed (n=4). The failure was neither zero-filled nor replaced by a rerun; sampler and summary exit=1 are retained. Structural integrity passing does not mean all experiments succeeded.

## Native execution and preparation

The following rounds=1 numbers are ms, medians of completed reuse-batch means out of five planned repeats; n is shown when fewer than five completed. Windows uses QPC to resolve short intervals. CSV retains min–max, both workloads, first runs and setup. These are not online p50s or confidence intervals.

| shape / N | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| chain / 1 | 0.004523 | 0.0061328 | 257.098 (n=4) |
| chain / 100 | 0.269892 | 0.603431 | 5046.78 |
| chain / 1000 | 2.73028 | 38.0955 | — |
| chain / 10000 | 31.661 | 5020.85 | — |
| wide / 1 | 0.0043671 | 0.0061426 | 141.126 |
| wide / 100 | 0.166703 | 0.700557 | 1577.25 |
| wide / 1000 | 1.686 | 38.6783 | 13092.5 |
| wide / 10000 | 19.3844 | 4876.8 (n=2) | — |
| diamonds / 1 | 0.0044405 | 0.006177 | 190.714 |
| diamonds / 100 | 0.269941 | 0.850725 | 3399.2 |
| diamonds / 1000 | 2.86546 | 39.9722 | — |
| diamonds / 10000 | 34.2804 | 4919.04 | — |


![Native reuse cost](/performance/v0.5.0-candidate-20261021/native_reuse.png)

Completion follows the status of each new CSV configuration. Incomplete cases receive no extrapolated runtime or wrong-result claim. Tiny-task differences are not proportional speedups for complete Agent applications. No profiles were collected, so curve changes are not attributed to proven internal hotspots.

| N=100 chain / stage | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| server_setup_ns | 0 | 0 | 416.315 |
| adapter_setup_ns | 0.0683 | 0.4674 | 72.8935 |
| first_ns | 0.6663 | 1.0291 | 4187 |
| run_ns | 0.269892 | 0.603431 | 5046.78 |


Stage responsibilities differ: Pulse builds a graph and validates on first start; Eino assembles/compiles; Temporal adapter setup connects/registers/starts a worker, with Server preparation separate. First execution covers submission through correct-result confirmation; reuse is subsequent. Do not subtract stage medians for attribution. Temporal persistence is retained rather than presented as an equivalent in-memory guarantee.

## Observation

| N=100 / shape | Pulse plain → observe (ms) | Eino plain → observe (ms) | Temporal plain → observe (ms) |
|---|---|---|---|
| chain | 0.269892 → 0.271658 | 0.603431 → 0.64464 | 5046.78 → 5044.3 |
| wide | 0.166703 → 0.163291 | 0.700557 → 0.709335 | 1577.25 → 1529.96 |
| diamonds | 0.269941 → 0.276178 | 0.850725 → 0.90326 | 3399.2 → 3397.31 |


![Plain and observe costs](/performance/v0.5.0-candidate-20261021/observability.png)

All use public extension hooks with atomic counters and no external logging. Coverage and event counts differ; differences are not prices for identical features. Some observe medians are lower: these are independently sampled variations, not evidence that observation accelerates execution. CSV retains event types/counts and ranges.

## Equal-work CPU and memory

A separate resource experiment performs one first run plus one reuse for every framework, including process startup, preparation and GC. Temporal's independent Server belongs to the Windows Job. The table uses chain/rounds=1; CPU is seconds and RSS is MiB.

| chain / N | Pulse CPU s / RSS MiB | Eino CPU s / RSS MiB | Temporal CPU s / RSS MiB |
|---|---|---|---|
| 1 | 0.015625 / 18.7422 | 0.015625 / 18.6445 | 0.71875 / 163.582 |
| 100 | 0.015625 / 18.3477 | 0.03125 / 18.4883 | 4.32812 / 215.613 |
| 1000 | 0.03125 / 18.4102 | 0.15625 / 27.1953 | — / — |


![Equal-work CPU and sampled RSS](/performance/v0.5.0-candidate-20261021/resources.png)

Job CPU includes exited descendants. Local process CPU accounting is quantized at roughly 15.625ms; equal small values do not prove identical CPU work. RSS is the largest simultaneous tree sum observed every 25ms and may miss a short peak. The common binary links all adapters: this includes a common harness baseline, not minimal standalone deployment footprints. Incomplete-case resources remain in raw data rather than completed-cost statistics.

CSV also retains cumulative process-local Go reuse allocations. Temporal Server is excluded from those counters, so they cannot rank whole-engine memory. Cumulative allocations, live heap and RSS differ. Native throughput batches and equal-work resource cases have different iteration counts; their total CPU is not mixed.

## Reliable execution and storage growth

SQLite uses WAL/FULL with native checkpoint/transaction granularity retained. Durable and Eino access benchmark SQLite hosts via existing Store contracts; these are not new official production backends. Durable encodes the complete RunRecord as JSON in SQLite's record BLOB column, with Revision-based CAS. It borrows only filestore OS locks, never its Load/Save methods, so it does not write a second JSON run file or provide distributed leases or fencing. Temporal is a real single-machine CLI Service; [production deployment](https://docs.temporal.io/production-deployment) still requires a production Service. This does not test clusters, HA or production databases. Native cases explicitly raise history-count error to 200000 and warn to 100000; these are not default-configuration scores.

The same three columns below show chain/rounds=1 reliable reuse. Durable plans three independent repeats; Temporal references the same native durable points above (five planned batches, four valid at N=1), not a fabricated second equivalent experiment.

| chain / N | Pulse + Durable (ms) | Eino | Temporal (ms) |
|---|---|---|---|
| 1 | 7.6359 | Active interrupt separately | 257.098 (n=4) |
| 10 | 53.8247 | Active interrupt separately | 230.261 |
| 100 | 551.431 | Active interrupt separately | 5046.78 |
| 1000 | 12588.3 | Active interrupt separately | — |
| 10000 | — | Active interrupt separately | — |


Reliable scale follows valid samples and statuses in this collection's checkpoint rows. A blank is not a completed-case score or accepted reliable scale. Eino's column means no equivalent ordinary per-node confirmation was measured, not that its extension mechanisms cannot support persistence.

For N=100 chain, median DB/WAL/SHM lengths (setup → first → reuse) are Durable 45,136 B → 4,230,648 B → 4,329,168 B; Temporal 2,356,456 B → 5,053,944 B → 5,250,648 B. These contain two RunIDs/Workflow histories, initial metadata and reused WAL; differences are not cumulative physical writes or per-task record sizes. Eino ordinary cases have no Store: actual 0B is not zero cost for equivalent reliable work; active checkpoint blobs are validated in recovery.

## Cross-process recovery

chain/rounds=256; confirm five nodes for N=10 and ten for N=100/1000. The parent independently reads Pulse SQLite plus execution audit, reads/hashes Eino's saved blob, or uses an independent Temporal client to verify completed-prefix history with no in-flight Activity before exit/kill. Three repeats per scenario, 36/36 completed, checking final checksum and no prefix rerun.

| N / prefix | Pulse host kill (s) | Eino active interrupt (s) | Temporal worker kill (s) | Temporal + Server restart (s) |
|---|---|---|---|---|
| 10 / 5 | 0.104242 | 0.0629073 | 5.20092 | 0.827797 |
| 100 / 10 | 0.338117 | 0.062967 | 8.6593 | 4.30853 |
| 1000 / 10 | 6.31158 | 0.143311 | 53.7078 | 49.3688 |


![Recovery separated by guarantee](/performance/v0.5.0-candidate-20261021/recovery.png)

Seconds from beginning exit/relaunch after confirmed prefix through correct-result confirmation, including remaining business. CSV operation_ns separately measures the final CLI operation, excluding earlier controller relaunch work. Eino is active-interrupt recovery, not arbitrary-crash recovery; none of these tests proves exactly-once in-flight external effects.

Server-alive and Server-restarted recovery are separate scenarios; these few cases do not recommend restarting production Servers. Routing or other causes require controlled diagnosis, which was not performed here. Native cases contain a first run plus full reuse, whereas recovery completes only the suffix after a confirmed prefix; their success/timeout boundaries are not interchangeable.

## Data, reproduction and checks

[Compact CSV](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/v0.5.0-candidate-20261021/summary.csv) contains all 234 groups with statuses/nulls/allocations/storage/events. [Identity](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/v0.5.0-candidate-20261021/identity.json) pins commits, raw hashes and scopes. [Bilingual reproduction](https://github.com/Luo-root/pulse/tree/main/benchmarks/production) supplies commands/tests; summarize_production.py regenerates figures from retained raw data. Raw JSONL, stdout/stderr, full machine/build inventories, diagnostics and databases stay outside Git; evidence is delivered separately, without invented online raw links.

This run passed adapter vet/full race, real Temporal Service tests and all three Python tool suites. Binary and all script hashes matched before/after serial collection; no builds/tests/plots ran concurrently. An isolated module cache passed verification; short paths were used for Python fixtures, and the external worktree build used -buildvcs=false with explicit commit/diff/source hashes retained. Historical scores are not rewritten or mixed into upgrade gains. No tag/release was created.

Pinned sources: [Eino v0.9.21](https://github.com/cloudwego/eino/tree/v0.9.21), [Temporal SDK v1.50.0](https://github.com/temporalio/sdk-go/tree/v1.50.0), [CLI v1.9.1](https://github.com/temporalio/cli/releases/tag/v1.9.1).

The October 17 comparison remains at its [fixed historical revision](https://github.com/Luo-root/pulse/blob/be778722b781ad97f2846848dcd9dacdc0e501e7/site/en/reference/production-comparison.md) and original identity summary; every table and figure here uses this new collection.
