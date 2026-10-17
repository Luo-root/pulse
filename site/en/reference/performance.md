# Scale and performance

This baseline gives different Go engines the same deterministic integer task and measures cost from 1 to 10000 nodes. It helps developers selecting an execution library understand small-graph fixed overhead, large-graph growth, reuse and cumulative allocation costs, without assuming Pulse wins.

Measured on **2026-10-17**. Pulse is pinned to `v0.4.2-0.20261016143631-7d70e1e14771`, main integration commit `7d70e1e1477192669d5008d47729e3c8c19a336f`. This is a public development revision during v0.5.0 preparation, not an uncreated release tag. Comparisons pin Azure/go-workflow [v0.1.13](https://github.com/Azure/go-workflow/releases/tag/v0.1.13) and go-taskflow [v1.2.0](https://github.com/noneback/go-taskflow/releases/tag/v1.2.0), using their tagged public APIs.

## Local results: tiny tasks expose engine overhead

Taskflow costs less than Pulse for these lightweight calculations; Pulse does not beat Azure at every size either. A 10000-node chain completes, but definition reuse makes a material difference. Splitting business work into finer nodes is not a free abstraction.

The table reports whole-task median time for **chain, rounds=1, e2e (construct and execute each iteration)**, in **ms**. Numeric points have five valid samples. The direct-Go reference remains in plots/raw data and is not ranked as a fourth engine.

| Business nodes | Pulse | Taskflow | Azure/go-workflow |
|---:|---:|---:|---:|
| 1 | 0.00562 | 0.00236 | 0.00349 |
| 10 | 0.0464 | 0.0175 | 0.159 |
| 100 | 0.407 | 0.155 | 37.1 |
| 1000 | 6.25 | 1.56 | Case budget reached; no valid sample |
| 10000 | 119 | 14.5 | Case budget reached; no valid sample |

For Pulse's 10000-node chain, reuse measures **44.8ms**, versus **119ms** e2e, with cumulative Go allocations approximately **23.4MiB / 31.5MiB**. Taskflow's corresponding times are **13.6ms / 14.5ms**. Reusing definitions has practical value; subtracting these independent medians does not produce a profiler-derived assembly breakdown.

![Construction and execution across three shapes](/performance/2026-10-17/dag-e2e-r1-time.png)

![Execution with reused definitions](/performance/2026-10-17/dag-reuse-r1-time.png)

![Amortized time per business node](/performance/2026-10-17/dag-e2e-r1-per-node.png)

![Cumulative Go allocations per graph](/performance/2026-10-17/dag-e2e-r1-allocations.png)

![Costs with additional fixed business computation](/performance/2026-10-17/dag-e2e-r256-time.png)

The plots retain wide, diamonds and rounds=256 results; the chain table does not replace the full matrix. Multiplicative size sampling does not presume exponential runtime growth. Points without measurements retain limit annotations rather than fabricated zeros.

All three 10000-node shapes retain five valid samples rather than selecting only the most favorable graph:

| 10000 business nodes, rounds=1 | Pulse e2e / ms | Pulse reuse / ms | Taskflow e2e / ms | Taskflow reuse / ms |
|---|---:|---:|---:|---:|
| chain | 119 | 44.8 | 14.5 | 13.6 |
| wide | 121 | 30.9 | 8.41 | 6.92 |
| diamonds | 141 | 50.4 | 13.4 | 9.56 |

Between 1000 and 10000 nodes, a tenfold increase in business work raises Pulse reuse time by approximately 10–11 times across these shapes and e2e time by approximately 19–23 times. These are observed interval ratios, not complexity proofs. Reuse Graph definitions for fixed workflows while creating a new Execution per run, and budget scheduling/allocation costs before splitting tiny work into tens of thousands of nodes. This experiment does not attribute a measured share of Taskflow's lower cost to particular contract differences.

The DAG suite retains **1800 planned samples**: 1700 successful, 20 case timeouts and 80 later repetitions not attempted. All 20 timeouts are Azure execution-mode points, each retaining its first timeout and four `not_repeated` entries. No checksum mismatch or sampler error counts as success.

Concise evidence: [CSV summary](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-dag.csv) · [versions and raw-data digest](https://github.com/Luo-root/pulse/blob/main/benchmarks/reports/2026-10-17-dag.json). Full samples, diagnostics and generated plot sets are retained in a separate evidence archive rather than the source repository. Reproduction writes them to ignored `benchmarks/results/`. Raw sample SHA256: `fc31f4a71d8bd3cff4d85b6697a71935cb52982cf570593e115371c8ca689ffd`.

## The identical task

Every node computes a uint64 value. Roots receive 1; other inputs are `1 + sum(predecessor outputs)`, with uint64 wrapping. The same xorshift/multiply function runs for 1 or 256 rounds. The final checksum sums terminal-node outputs, checked by Go and an independent Python implementation.

| Shape | Actual dependencies | N=1 |
|---|---|---|
| chain | Each node depends on its predecessor | One independent node |
| wide | All nodes independent, under the same business-concurrency cap | One independent node |
| diamonds | Alternating widths 1 and 2, all-to-all between adjacent layers, truncating the last to exactly N nodes | One node in the first layer |

There are no extra empty source or aggregation nodes. Every sample retains actual nodes, edges and critical path in nodes. At N=10000, chain/wide/diamonds have 9999/0/13332 edges and critical paths 10000/1/6667. Direct Go computation is a serial business-cost reference, not a fourth workflow engine.

These tasks contain many tiny calculations, making assembly, dependency checks, node wakeups and value delivery visible. They include no model, remote API, database call or artificial sleep, and do not replace application-specific load tests.

## Read three timing modes separately

| Mode | Included | Excluded |
|---|---|---|
| e2e | Definition, adapter and executor initialization each iteration; native execution/validation; final output reading and checksum comparison | Common task specification, oracle, one warmup, pre-sample GC, process launch |
| build | Definition/adapter/executor assembly; last object retained through timing | Business execution and implicit validation occurring only in Start/Do/Run |
| reuse | Native run/reset, per-run state, buffer clearing and output reading on a warmed definition | Initial definition/adapter/executor assembly |

Pulse still creates a fresh Execution each time. Azure Do reset/preflight and Taskflow native Run preparation remain timed on every run. Pulse alone does not get validation moved out of timing. `build` does not represent every preparation required to run, and medians across the three modes are not an exact additive/subtractive cost breakdown.

Pulse uses typed Get/Set and declared-outlet ReadOutput; the other adapters use dependency-ordered business values for the same calculation. Error, skip, slot, retry and observation contracts differ; only the common successful task is compared. Taskflow exposes no pool shutdown/join or context-cancellation API. Wait completes business work, but a short worker epilogue can cross that boundary. A process watchdog bounds every CLI. JSON notes preserve these differences.

## Checking the steep result

The Azure adapter is an ordinary leaf Step; its body performs only the common arithmetic and array access, without logging, whole-graph copies or artificial sleep. In the pinned [Workflow source](https://github.com/Azure/go-workflow/blob/v0.1.13/workflow.go), preflight repeatedly scans; tick queries StateOf/UpstreamOf per Step, while StateOf/RootOf traverse roots. [Traverse](https://github.com/Azure/go-workflow/blob/v0.1.13/wrap.go) builds traversal paths. These native lookups/traversals confirm extra work independent of business computation. Static inspection is not an exact profile or timing-share measurement, and the old tag's curve does not establish main/other-version performance.

## Machine, statistics and limits

Intel Core i9-14900HX, 24 cores/32 logical processors, approximately 32GiB RAM, Windows 11 `10.0.26200`, amd64, Go `1.25.0`. `GOMAXPROCS=4` and business concurrency cap 4. Engines execute serially, without concurrent local compilation, race tests, site builds or profiling. Filesystem caches are not flushed; workstation power, temperature and unrelated programs are not tightly controlled.

Each primary point plans five samples. After a warmup and GC, each sample accumulates at least 100ms of measured work; slow cases can contain one operation. Values are medians of repeated batch means; error bars are observed min–max, not production p95/p99 or confidence intervals. Nanosecond averages of short tasks do not imply equivalent Windows clock resolution. All three shapes are identical at N=1; differences between their separately timed samples are not topology effects and illustrate workstation variation.

The 60-second child case budget includes assembly, warmup and measurement, with a separate 65-second parent limit. A timeout with no valid sample is therefore not a claim that one operation necessarily takes over 60 seconds. After a limit, remaining repetitions are recorded as `not_repeated`. Failure, unsupported and timeout points are neither zero-filled nor extrapolated to fabricate a 10000-node result.

B/op and allocs/op are cumulative process-wide Go allocations divided by completed iterations, not peak heap or RSS. Per-node time and business-node throughput derive from whole-task time, not independently observed node latency. Read the [Durable report](https://github.com/Luo-root/pulse-durable/blob/main/site/en/guide/performance.md) for MemoryStore, file confirmation and recovery; these in-memory timings do not establish persistence performance.

## Reproduce and check

See the [benchmark method](https://github.com/Luo-root/pulse/blob/main/benchmarks/README_en.md) for exact commands, the compute function, pinned versions and adapter caveats. `benchmarks/dag` is a separate module with no replacements. Comparison dependencies do not enter the production module; the Pulse root runtime dependency closure remains standard-library-only. Full timing is manual; normal CI checks small-case correctness.

Before sampling, 192 small graph/workload/concurrency/engine combinations passed per-node checks and race tests. Direct/Pulse/Taskflow also passed non-race per-node verification for all three 10000-node shapes. Independent review checked equal work, reuse, timeout behavior and sampler error handling. Azure points exceeding a budget do not count as large-scale correctness passes.

The external evidence archive retains raw JSONL, configuration, module versions/checksums, binary SHA256, machine details, CSVs and plots. The source repository keeps only concise statistics, version summaries and charts used by the report. Go's retained JSON does not contain Origin fields; separate `module-origins.json` queries the same-version proxy's actual metadata, explicitly marking missing or failed entries. Do not rerun with `@latest`; save a new matrix after changing concurrency, toolchain, computation or backend. This baseline does not alter scheduling or weaken required semantics to publish favorable numbers.
