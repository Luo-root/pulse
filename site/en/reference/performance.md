# Scale and performance

This baseline gives different Go engines the same deterministic integer task and measures cost from 1 to 10000 nodes. It helps developers selecting an execution library understand small-graph fixed overhead, large-graph growth, reuse and cumulative allocation costs, without assuming Pulse wins.

Measured on **2026-10-17**. Pulse is pinned to `v0.4.2-0.20261016143631-7d70e1e14771`, main integration commit `7d70e1e1477192669d5008d47729e3c8c19a336f`. This is a public development revision during v0.5.0 preparation, not an uncreated release tag. Comparisons pin Azure/go-workflow [v0.1.13](https://github.com/Azure/go-workflow/releases/tag/v0.1.13) and go-taskflow [v1.2.0](https://github.com/noneback/go-taskflow/releases/tag/v1.2.0), using their tagged public APIs.

## #422: three optimization steps with semantics preserved

This is a separate, retained candidate measurement from the same day. The original baseline sections and dataset remain unchanged. It compares baseline `7d70e1e1477192669d5008d47729e3c8c19a336f`, final candidate `f6049c4aac4d2d5316e0ced5959dc7bc2a257b46`, and Taskflow `v1.2.0`. The candidate is not a release tag. All improvements use the baseline remeasured in this run, without mixing in historical baseline numbers below.

The changes are split for review:

1. [#423 node ID index](https://github.com/Luo-root/pulse/pull/423): adding 10000 distinct nodes individually used to scan existing nodes repeatedly, totaling 49,995,000 ID comparisons. A private index now records only successfully committed nodes. A single node does not allocate this index; a successful commit taking the total above one creates it and backfills old nodes. Registration follows whole-batch validation, preserving failed-batch name reuse, error priority and declaration order.
2. [#424 static-check reuse and input-map allocation](https://github.com/Luo-root/pulse/pull/424): a successfully sealed ordinary graph without Sub/stream declarations reuses invariant static checks. Per-run inputs, host inputs, checkpoint, dynamic-value and recovery checks still execute. No declared inlets means no required-input map; otherwise capacity follows inlet count. The fast path still scans declarations and is not O(1) Start. A fresh graph's first start cannot use sealed validation reuse.
3. [#425 nil-error classification](https://github.com/Luo-root/pulse/pull/425): three error classifiers return false immediately only for a truly nil interface. Every non-nil path retains the original `errors.As`, including typed nil, custom As, wrappers/Join and Unwrap panic, preserving return and panic behavior.

Scheduling, business work, status maintenance, normal skip, cancellation priority, retry budgets and checkpoint-confirmation-before-publication rules are unchanged. Graph definitions remain reusable with a fresh Execution per run, and the root runtime dependency closure remains standard-library-only.

### Ten-thousand-node results and the remaining gap

These are whole-graph median times for **N=10000, rounds=1**, in **ms**, with five batch means per point. Build constructs definitions, reuse executes warmed definitions, and e2e constructs and executes each iteration. Subtracting modes is not a profile breakdown.

| Shape | Mode | Remeasured Pulse baseline | Final candidate | Time reduction | Taskflow |
|---|---|---:|---:|---:|---:|
| chain | build | 75.994 | 6.193 | 91.9% | 1.520 |
| chain | reuse | 41.413 | 31.061 | 25.0% | 12.790 |
| chain | e2e | 119.783 | 45.484 | 62.0% | 15.020 |
| wide | build | 86.701 | 7.480 | 91.4% | 1.158 |
| wide | reuse | 23.002 | 21.255 | 7.6% | 5.761 |
| wide | e2e | 102.520 | 31.543 | 69.2% | 7.261 |
| diamonds | build | 91.330 | 6.585 | 92.8% | 1.674 |
| diamonds | reuse | 45.135 | 38.139 | 15.5% | 9.535 |
| diamonds | e2e | 130.238 | 51.355 | 60.6% | 11.828 |

The largest gain removes repeated duplicate checks during large-graph construction. Reused execution also improves, with shape-dependent gains. Taskflow remains faster on all three lightweight ten-thousand-node tasks: candidate e2e times are still about 3.0–4.3 times Taskflow's. This is not parity. The libraries' data-delivery, error and lifecycle contracts differ; this run does not attribute measured shares of the remaining gap to those contracts.

![Chain: final candidate, remeasured baseline and Taskflow](/performance/2026-10-17-optimization/chain-R1-time.svg)

![Independent-node wide graph](/performance/2026-10-17-optimization/wide-R1-time.svg)

![Repeated fork/join diamonds](/performance/2026-10-17-optimization/diamonds-R1-time.svg)

The full dataset retains three shapes, 1/10/100/1000/10000 nodes, rounds=1/256 and build/reuse/e2e. Twelve SVG/PNG pairs show both time and cumulative allocation. The lightweight ten-thousand-node table does not replace the full matrix.

### Space costs and slower points

The index trades additional O(N) space for duplicate-check speed. Cumulative Go build allocation at N=10000 rises from **7.164/10.186/7.418MiB** to **7.997/11.019/8.251MiB** for chain/wide/diamonds, approximately **0.833MiB** extra. This is cumulative allocation, not the live index size, peak heap or RSS.

Reused execution allocation falls from **23.581/20.652/24.085MiB** to **17.449/16.905/18.104MiB**, reductions of **26.0%/18.1%/24.8%**. E2e allocation falls by **7.7%/6.4%/5.9%** respectively.

![Chain cumulative Go allocation costs and gains](/performance/2026-10-17-optimization/chain-R1-allocation.svg)

Across 90 baseline/candidate configuration pairs, no candidate time median is over 10% slower, but not every point accelerates. Two candidate min–max ranges lie entirely above baseline: N=10 with the rounds=256 label, chain build **4.829→5.115μs (5.9% slower)** and diamonds build **5.412→5.723μs (5.7% slower)**. Build does not execute the compute body; rounds is only a configuration label here. These and all other points remain in `regression-observations.json` / `comparison.csv`. Five-sample ranges are not confidence intervals.

The first formal candidate run exposed extra index allocation at N=1. Index creation was then delayed until a successful commit of the second node, followed by a complete rerun of the final commits. That first matrix remains separate rather than being mixed into the final run or hiding the issue it exposed.

### Stages, correctness and reproduction

The two intermediate cumulative stages were also measured for chain/N10000/rounds1. These are observed stage medians, not profiler shares. A code change that does not affect a mode can still coincide with sampling variation.

| Stage | Exact commit prefix | build / ms | reuse / ms | e2e / ms |
|---|---|---:|---:|---:|
| Baseline | `7d70e1e14771` | 75.994 | 41.413 | 119.783 |
| ID index | `3a4c89033478` | 5.953 | 40.960 | 49.116 |
| Plus static checks/input map | `e33b5cbb27d0` | 6.117 | 33.841 | 48.979 |
| Plus nil-error classification | `f6049c4aac4d` | 6.193 | 31.061 | 45.484 |

Before timing, **30 ten-thousand-node checks**, covering five variants × three shapes × two compute loads, verified per-node invocation counts, values, maximum business concurrency and two runs of reused definitions. Timing disables per-node probes but still checks every final checksum. **All 1380/1380 samples succeeded, with five valid samples in each of 276 groups.** The final stack passed build, vet, full race, root dependency-closure and whitespace checks. Durable's full race suite, including real process exit/recovery, also passed with the final Core candidate. Focused tests and mutation controls cover whole-batch commit, static/per-run validation boundaries, normal skip, recovery rejection, and original nil/typed-nil behavior. Each layer also received independent review.

The environment, task and timing boundaries follow the method below. Serial rotated timing began after local compilation/tests/plotting stopped. Baseline and Taskflow share one benchmark binary, selected by engine flag; candidates replace only Core. As the candidates are unpublished, builds use generated external `-modfile` files with local source replacements. Production and shipped benchmark `go.mod`/`go.sum` files are unchanged. Exact commits, preparation commands, pre/post binary SHA256 and raw commands/output are retained.

Evidence: [complete dataset and reproduction](https://github.com/Luo-root/pulse/tree/perf/422-optimization-evidence/benchmarks/results/2026-10-17-optimization) · [raw JSONL](https://github.com/Luo-root/pulse/blob/perf/422-optimization-evidence/benchmarks/results/2026-10-17-optimization/samples.jsonl) · [all configurations CSV](https://github.com/Luo-root/pulse/blob/perf/422-optimization-evidence/benchmarks/results/2026-10-17-optimization/comparison.csv) · [slower observations](https://github.com/Luo-root/pulse/blob/perf/422-optimization-evidence/benchmarks/results/2026-10-17-optimization/regression-observations.json). Final raw JSONL SHA256: `7e76785fb76a682f48f888c2f5fd34961d280722a5dfbef20d4ab1807d3458b3`.

The following original baseline remains as measured in its original sampling run.

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
