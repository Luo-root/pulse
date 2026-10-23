# Formal-release SQLite and recovery measurements (2026-10-23)

**COMPLETED_WITH_ERRORS**. This bounded collection uses public formal releases; it does not replace the [historical pre-release multidimensional report](./production-comparison.md). Only SQLite reliable confirmation, the Temporal native-persistence baseline and process recovery were rerun, not the full native, observe or resource matrices. It establishes neither a Go-upgrade speedup nor results for unpublished Core v0.5.1.

## Identity and method

| Component | Measured pin |
|---|---|
| Benchmark host | Go 1.26.0 / windows/amd64 |
| Core | v0.5.0 |
| Durable / official sqlitestore | v0.1.0 / v0.1.0 |
| Eino | v0.9.21 (unchanged pin) |
| Temporal SDK / CLI / Server | v1.50.0 / 1.9.1 / 1.32.0 (unchanged pins) |

Collected on 2026-10-23 without local replace. Three independent samples were planned per configuration, iterations=1: one first execution plus one reuse execution, not a single execution covering the entire case. Ordinary matrices use rounds=1 and N=1/10/100/1000/10000; business parallelism and each process's GOMAXPROCS are 4. The 60s case budget includes setup, first and reuse; the 65s parent watchdog bounds the process tree. Remaining repeats after a non-completion remain not_repeated, never zero-filled or replaced by reruns.

chain is a serial predecessor chain. wide consists of N independent tasks with no edges, not a common-source/common-sink fan-out/fan-in. diamonds alternates widths 1 and 2 with complete connections between adjacent layers; N counts actual tasks. Equal N does not imply equivalent parallel structure.

## Completion and errors

| Valid matrix | Planned identities | Status |
|---|---:|---|
| checkpoint / Pulse | 45 | 36 ok, 3 deadline, 6 not_repeated |
| checkpoint / Eino | 45 | 15 not_equivalent, 30 not_repeated |
| Temporal native | 45 | 30 ok, 1 deadline, 4 hard_timeout, 10 not_repeated |
| recovery | 36 | 36 ok |

There are 171 valid records. Another 45 Temporal checkpoint invocation errors remain as diagnostics: the adapter does not support that mode. The raw checkpoint file has 135 rows, or 90 after excluding these diagnostics. These are configuration errors, not Temporal performance failures, and do not enter performance statistics; the whole collection is not an all-success run. Eino ordinary success does not save per-node checkpoints: not_equivalent means neither zero cost nor inability to support persistent applications. Active-interrupt recovery is measured separately.

## First and reuse: separate guarantees, no overall ranking

Units: ms; each cell is median [min–max], n counts only complete ok samples. Values come from JSON first_ns and run_ns; iterations=1 makes the reuse batch one run. Medians are not online p50; ranges are not confidence intervals. — means no valid completed cost, not zero. Timeouts at 10000 do not establish that one execution necessarily exceeds 60s or an engine's maximum supported size.

### Pulse + Durable official SQLite: per-node reliable confirmation

| shape / N | n | First ms [min–max] | Reuse ms [min–max] | Status |
|---|---:|---|---|---|
| chain / 1 | 3 | 10.364200 [8.471900–11.266600] | 7.471000 [6.365900–10.390000] | ok=3 |
| chain / 10 | 3 | 54.447100 [53.846000–56.343500] | 53.536500 [51.450400–55.578800] | ok=3 |
| chain / 100 | 3 | 600.267700 [599.082100–610.809500] | 603.486300 [600.530300–615.435800] | ok=3 |
| chain / 1000 | 3 | 12525.316300 [12447.054900–13269.253900] | 14624.322000 [14287.211800–15754.115800] | ok=3 |
| chain / 10000 | 0 | — | — | deadline=1, not_repeated=2 |
| diamonds / 1 | 3 | 8.190700 [7.268500–9.051200] | 7.555100 [6.442200–8.790400] | ok=3 |
| diamonds / 10 | 3 | 58.358500 [54.990600–60.623600] | 57.345700 [51.630300–57.347000] | ok=3 |
| diamonds / 100 | 3 | 605.073000 [603.608900–649.888200] | 617.546100 [606.134100–673.631000] | ok=3 |
| diamonds / 1000 | 3 | 12985.821400 [12443.816000–13358.483400] | 13829.337400 [13772.147200–15070.546800] | ok=3 |
| diamonds / 10000 | 0 | — | — | deadline=1, not_repeated=2 |
| wide / 1 | 3 | 9.249400 [8.091800–9.359200] | 7.612800 [7.356700–8.520700] | ok=3 |
| wide / 10 | 3 | 55.805300 [53.114600–58.778600] | 54.483800 [47.481500–59.538900] | ok=3 |
| wide / 100 | 3 | 616.340100 [608.083400–778.549100] | 669.565200 [605.473100–671.984900] | ok=3 |
| wide / 1000 | 3 | 12879.575300 [12431.172700–13581.729100] | 14727.901900 [14027.857200–14899.504400] | ok=3 |
| wide / 10000 | 0 | — | — | deadline=1, not_repeated=2 |

### Temporal: native service-persistence baseline

These are native event-history and Activity confirmations in plain mode, not a rerun of the unsupported checkpoint mode or an equivalent of Pulse in-memory execution.

| shape / N | n | First ms [min–max] | Reuse ms [min–max] | Status |
|---|---:|---|---|---|
| chain / 1 | 3 | 95.225400 [88.151700–117.517800] | 145.276700 [95.541900–228.674900] | ok=3 |
| chain / 10 | 3 | 526.301300 [514.445800–613.305400] | 245.812200 [231.687600–259.565600] | ok=3 |
| chain / 100 | 3 | 4225.259400 [4091.973300–4282.209300] | 5050.856600 [5045.190400–5061.768300] | ok=3 |
| chain / 1000 | 0 | — | — | hard_timeout=1, not_repeated=2 |
| chain / 10000 | 0 | — | — | deadline=1, not_repeated=2 |
| diamonds / 1 | 3 | 84.936900 [82.057900–89.767100] | 160.572800 [143.179300–167.099700] | ok=3 |
| diamonds / 10 | 3 | 470.463900 [466.056500–503.188700] | 189.810800 [183.122700–210.378300] | ok=3 |
| diamonds / 100 | 3 | 2566.570500 [2542.619200–2576.617800] | 3398.344300 [3394.475700–3398.855400] | ok=3 |
| diamonds / 1000 | 0 | — | — | hard_timeout=1, not_repeated=2 |
| diamonds / 10000 | 0 | — | — | hard_timeout=1, not_repeated=2 |
| wide / 1 | 3 | 90.837800 [84.933700–105.078500] | 129.734100 [127.996900–158.591400] | ok=3 |
| wide / 10 | 3 | 439.554000 [426.323400–486.264900] | 162.987400 [142.887500–179.507100] | ok=3 |
| wide / 100 | 3 | 1504.366700 [1422.477300–1653.597400] | 1582.763500 [1376.340000–1681.513100] | ok=3 |
| wide / 1000 | 3 | 12702.614500 [12275.964200–14018.747100] | 12981.282300 [12338.029500–13129.035500] | ok=3 |
| wide / 10000 | 0 | — | — | hard_timeout=1, not_repeated=2 |

## Recovery: retain four distinct semantics

chain, rounds=256, N=10/100/1000, confirmed prefixes 5/10/10 respectively; n=3 per configuration, all 36 records ok. Pulse hard-kills the host after confirmation, then calls Resume in a new process. Eino saves a blob through active interrupt before exit and resume, not arbitrary crash recovery. Temporal runtime_kill kills only the worker; server_restart restarts both worker and Server. Probes select a confirmed boundary without in-flight Activities/external effects; they do not establish arbitrary exactly-once effects.

wall is controller recovery wall time. operation_ns is the child-command operation scope, including graph/Store reopening or client reconstruction, not pure suffix business time; Temporal also includes control commands/waiting. Do not interchange them or rank wall times across these different semantics.

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

Server-restart and worker-only recovery show counterintuitive ordering and broad ranges. Without controlled diagnosis, this is not evidence that restarting the service makes recovery faster. Recovery runs only the suffix; an ordinary case includes two complete executions, so recovery success does not establish ordinary-case completion within the same budget.

## SQLite, setup and evidence limits

Pulse runtime Load/Save, CAS and local run ownership all delegate to official sqlitestore v0.1.0, not the historical custom SQLite Store. A thin wrapper opens an additional inspection connection for independent reads/fault injection, never runtime persistence. Opening and PRAGMA validation of that connection are included in adapter_setup_ns; setup is not solely engine graph construction. Temporal server_setup_ns and adapter_setup_ns for Dial/registration/worker.Start also measure different work.

The official backend source configures and reads back WAL/FULL (synchronous=2); inspection/Eino connections do likewise. Temporal startup arguments request WAL/FULL. A PRAGMA readback on the inspection connection alone cannot establish synchronous on another connection; Temporal arguments are not independent per-connection readback evidence either. These are configuration/source-level guarantees. Process hard-kill is not power loss or disk-cache-loss testing; backup/restore, migration and distributed lease/fencing are not accepted here. DB/WAL/SHM lengths are not cumulative physical writes; this collection does not update the full resource ranking.

## Sources and checks

Compact tracked data: [summary.csv](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/summary.csv) · [identity.json](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/sqlite-formal-20261023/identity.json). The Temporal native group uses `kind=native_persistence`, distinct from Durable checkpointing.

The external evidence root is `go126-release-20261023/`. All numeric tables use the 57 groups in `summary/identity.json`, matched to `summary/summary.csv`. `measurement-plan.json` and `measurement-source-identity.json` pin public modules, toolchain and source hashes. `measurements/{checkpoint,temporal-native,recovery}/manifest.json` and `raw.jsonl` retain identities, budgets and all failed/not-repeated records. Raw databases, logs and binaries are not copied into the source repositories.

| raw.jsonl | SHA-256 |
|---|---|
| checkpoint | `d59f5116fe41cc87308d5be6724df55d18c679f4898f875a211d9325e436269d` |
| temporal-native | `610bb237276d167a968176d8fb33750538269a1cb8571786f511423a0d37e7b3` |
| recovery | `d0485cee9514d4d3f888b5a60eb9e0c5af5907202f0e88d774a264c53ba54d80` |
