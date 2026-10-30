# Pulse/Durable, Eino and Temporal: one comparison set

This page compares Pulse (with Durable for reliable execution), Eino and Temporal across execution, observation, resources, storage and recovery. Native execution, observation and resource data were collected on 2026-10-17; SQLite reliable execution and recovery data were collected on 2026-10-23.

The site's current API baseline is v0.5.2. The charts below remain historical measurements from the stated dates and pinned versions, not a v0.5.2 rerun. No new equivalent production matrix was run for the validation and graph-algorithm improvements, so old data cannot establish performance for the new release.

## Test configuration

The SQLite rerun uses Go 1.26, the Pulse Core v0.5.0 source baseline, Pulse Durable/sqlitestore v0.1.0, Eino v0.9.21 and Temporal v1.50.0. All tests use released dependencies without local replacements.

| Component | Pinned identity |
|---|---|
| Pulse Core source baseline | v0.5.0 (measured version) |
| Durable / official sqlitestore | v0.1.0 / v0.1.0 |
| Eino | v0.9.21 |
| Temporal SDK / CLI / Server | v1.50.0 / 1.9.1 / 1.32.0 |
| SQLite driver | modernc.org/sqlite v1.60.1 |
| Formal SQLite benchmark host | Go 1.26.0 / windows/amd64 |


The host is Windows 11 / i9-14900HX; each Go process uses GOMAXPROCS=4 and business concurrency is 4. Test graphs are chain, wide and diamonds with N=1/10/100/1000/10000; recovery uses rounds=256. The current formal SQLite rerun reports:

| Formal SQLite rerun matrix | Planned identities | Status |
|---|---:|---|
| checkpoint / Pulse | 45 | 36 ok, 3 deadline, 6 not_repeated |
| checkpoint / Eino | 45 | 15 not_equivalent, 30 not_repeated |
| Temporal native | 45 | 30 ok, 1 deadline, 4 hard_timeout, 10 not_repeated |
| recovery | 36 | 36 ok |

The rerun contains 171 valid records and 45 Temporal checkpoint configuration diagnostics. Incomplete cases retain their status; `—` indicates no completed sample.

| Dimension | Pulse / Durable | Eino | Temporal |
|---|---|---|---|
| Native execution | In-memory Pulse, fresh Execution per run | compose Graph, AllPredecessor, unique output keys | Real Server/worker, always native persistence |
| Observation | Observer lifecycle counters | Graph/Lambda start/end/error callbacks | Non-replay Workflow/Activity interceptors |
| Reliable confirmation | Durable Store confirmation before publication | No automatic per-node save on normal success; active CheckPointStore interrupt separately | Native history and Activity completion confirmation |
| Recovery | Kill host after confirmed prefix; new-process Resume | Exit after active checkpoint; new-process resume | Confirmed prefix without in-flight Activity, worker kill; Server restart separately |

## Native execution and preparation

Values are milliseconds and shown as medians of repeated samples.

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

Temporal chain/diamonds N=1000 and all N=10000 configurations did not complete; wide N=1000 completed.

| N=100 chain / stage | Pulse (ms) | Eino (ms) | Temporal (ms) |
|---|---|---|---|
| server_setup_ns | 0 | 0 | 364.083 |
| adapter_setup_ns | 0.0648 | 0.4919 | 66.1348 |
| first_ns | 0.4939 | 1.035 | 4186.82 |
| run_ns | 0.262107 | 0.613691 | 5049.45 |


Stage definitions: Pulse covers graph construction and first-start validation; Eino covers assembly/compilation; Temporal adapter setup covers connection, registration and worker.Start, with Server preparation separate. `first_ns` is the first execution and `run_ns` is reuse.

## Observation

| N=100 / shape | Pulse plain → observe (ms) | Eino plain → observe (ms) | Temporal plain → observe (ms) |
|---|---|---|---|
| chain | 0.262107 → 0.271002 | 0.613691 → 0.662909 | 5049.45 → 5052.55 |
| wide | 0.170895 → 0.173396 | 0.714113 → 0.741753 | 1374.74 → 1301.09 |
| diamonds | 0.282539 → 0.283872 | 0.834692 → 0.881922 | 3400.07 → 3398.72 |


![Plain and observe costs](/performance/2026-10-17-production/observability.png)

Observation uses public extension hooks and atomic counters. Event coverage differs by framework, so the values do not represent identical observation features.

## Equal-work CPU and memory

A separate resource experiment performs one first run plus one reuse for every framework, including process startup, preparation and GC. Temporal's independent Server belongs to the Windows Job. The table uses chain/rounds=1; CPU is seconds and RSS is MiB.

| chain / N | Pulse CPU s / RSS MiB | Eino CPU s / RSS MiB | Temporal CPU s / RSS MiB |
|---|---|---|---|
| 1 | 0.015625 / 18.3594 | 0.015625 / 17.793 | 0.609375 / 170.289 |
| 100 | 0.015625 / 18.3906 | 0.03125 / 17.7969 | 2.53125 / 213.496 |
| 1000 | 0.015625 / 18.4102 | 0.140625 / 26.9883 | — / — |


![Equal-work CPU and sampled RSS](/performance/2026-10-17-production/resources.png)

CPU includes child processes and RSS is the sampled process-tree peak. Temporal CPU/RSS includes its independent Server; Go allocation counters exclude that Server.

## Reliable execution and storage growth

Pulse + Durable reliable-confirmation results are shown below in milliseconds as first/reuse medians.

| shape / N | Pulse + Durable first / reuse (ms) | status |
|---|---|---|
| chain / 1 | 10.364200 / 7.471000 | ok=3 |
| chain / 10 | 54.447100 / 53.536500 | ok=3 |
| chain / 100 | 600.267700 / 603.486300 | ok=3 |
| chain / 1000 | 12525.316300 / 14624.322000 | ok=3 |
| chain / 10000 | — / — | deadline=1, not_repeated=2 |
| diamonds / 1000 | 12985.821400 / 13829.337400 | ok=3 |
| wide / 1000 | 12879.575300 / 14727.901900 | ok=3 |
| wide / 10000 | — / — | deadline=1, not_repeated=2 |

Official sqlitestore v0.1.0 uses WAL/FULL; Pulse Load/Save, CAS and local run ownership use that Store. The measurement does not cover backup/restore, migration or distributed lease/fencing.

## Cross-process recovery

The formal recovery run uses chain/rounds=256, N=10/100/1000, confirmed prefixes 5/10/10, n=3 per configuration, with all 36 records completed. Pulse kills the host after confirmation and resumes in a new process; Eino saves a blob through active interrupt and resumes; Temporal separates runtime_kill from server_restart. These are confirmed boundaries without in-flight Activities/external effects and do not establish arbitrary exactly-once effects.

| Engine / scenario / N | n | wall s [min–max] | operation ms [min–max] |
|---|---:|---|---|
| eino / active_interrupt / 10 | 3 | 0.049907 [0.046073–0.050180] | 24.372800 [21.061400–24.645800] |
| eino / active_interrupt / 100 | 3 | 0.046845 [0.044777–0.054857] | 21.419600 [21.387700–25.076200] |
| eino / active_interrupt / 1000 | 3 | 0.134673 [0.130496–0.136944] | 103.711600 [103.558600–108.297300] |
| pulse / runtime_kill / 10 | 3 | 0.072193 [0.065890–0.074435] | 19.992000 [19.426300–23.130100] |
| pulse / runtime_kill / 100 | 3 | 0.335978 [0.303561–0.389495] | 286.402000 [276.070700–328.842300] |
| pulse / runtime_kill / 1000 | 3 | 9.886758 [9.355772–10.618498] | 9804.680500 [9303.068600–10548.256300] |
| temporal / runtime_kill / 10 | 3 | 5.199157 [5.188746–5.213046] | 5134.301200 [5111.029900–5134.654700] |
| temporal / server_restart / 10 | 3 | 10.167985 [0.713383–10.179320] | 9884.872200 [441.128800–9916.820000] |
| temporal / runtime_kill / 100 | 3 | 8.647032 [8.632625–13.595738] | 8578.779300 [8567.382300–13526.297700] |
| temporal / server_restart / 100 | 3 | 4.080621 [4.029154–4.241825] | 3795.921100 [3757.966300–3846.748700] |
| temporal / runtime_kill / 1000 | 3 | 53.706356 [53.680285–53.717853] | 53615.510200 [53612.884800–53635.186500] |
| temporal / server_restart / 1000 | 3 | 49.185665 [49.084596–49.220536] | 48907.995200 [48819.051800–48933.869500] |

wall is controller recovery time; operation is the child-command time, including Graph/Store or client reconstruction. The two metrics are not interchangeable.

## Data, reproduction and checks

The historical full-dimensional data remains in `benchmarks/reports/2026-10-17-production/` with its compact CSV and identity. Current formal SQLite data is in [sqlite-formal-20261023 summary.csv](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/summary.csv) and [identity.json](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/identity.json). The external evidence root is `go126-release-20261023/`; raw records retain all failed and not-repeated states, while databases, logs and binaries stay outside the source repository.

The formal rerun status is **COMPLETED_WITH_ERRORS**; recovery completed 36/36.

Pinned sources: [Eino v0.9.21](https://github.com/cloudwego/eino/tree/v0.9.21), [Temporal SDK v1.50.0](https://github.com/temporalio/sdk-go/tree/v1.50.0), [CLI v1.9.1](https://github.com/temporalio/cli/releases/tag/v1.9.1).
