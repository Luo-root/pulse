# Whole-graph scale and comparable engines

This directory measures how construction, execution and allocation costs change for the same business task at 1, 10, 100, 1000 and 10000 nodes. See [#418](https://github.com/Luo-root/pulse/issues/418) and the persistence companion, [Durable #37](https://github.com/Luo-root/pulse-durable/issues/37).

`dag/` is a separate Go module: comparison libraries do not enter the production Pulse module. Durable/SQLite adapters live in the separate `benchmarks/workflow/` module in the Durable repository. Both suites use the sampling/plotting tools here; their results are not combined into one speed ranking.

Chinese: [README.md](README.md).

## Separate #422 candidate remeasurement

[Configuration comparisons](reports/2026-10-17-optimization.csv), [stage statistics](reports/2026-10-17-optimization-stages.csv) and [version summary](reports/2026-10-17-optimization.json) retain concise statistics from 1380 successful samples. The 30 instrumented large-graph checks, complete raw samples, 12 plot pairs and diagnostics remain in an external evidence archive rather than the source repository. Original baseline, first candidate and final candidate are separate runs and never mixed.

The layers are [#423](https://github.com/Luo-root/pulse/pull/423), [#424](https://github.com/Luo-root/pulse/pull/424) and [#425](https://github.com/Luo-root/pulse/pull/425). The measured commit is `f6049c4aac4d2d5316e0ced5959dc7bc2a257b46`. SQLite, reliable-execution and recovery measurements are in the [unified production comparison](../site/en/reference/production-comparison.md).

## Identical business tasks

Every engine uses the same indices, dependencies, integer inputs and outputs. N counts actual business nodes, with no synthetic source or aggregation nodes.

| Shape | Dependencies | Meaning at N=1 |
|---|---|---|
| `chain` | Each node depends on its predecessor | One independent node |
| `wide` | N independent nodes | One independent node |
| `diamonds` | Layers alternate widths 1, 2, 1, 2; all nodes in one layer feed every node in the next; truncate the last layer to exactly N nodes | One node in the first layer |

A node's input is `1 + sum(predecessor outputs)`, or 1 at a root. Arithmetic wraps as uint64. The business function is:

```text
x = input XOR ((id + 1) * 0x9e3779b97f4a7c15)
repeat rounds times:
    x = x XOR (x >> 12)
    x = x XOR (x << 25)
    x = x XOR (x >> 27)
    x = x * 0x2545f4914f6cdd1d
return x
```

`rounds=1` emphasizes construction/scheduling overhead in small tasks; `rounds=256` adds fixed business computation. The checksum is the uint64 sum of all terminal outputs. Go adapters check results; an independent Python implementation checks checksums, edge counts and critical path lengths again. Empty callbacks do not qualify as the same task.

Direct Go computation is a business-cost reference, not a fourth workflow engine. Comparing their common successful path does not equate their error, skip, data-delivery, retry or observation guarantees.

## Fixed versions and environment

| Project | Version pinned by current reproduction code |
|---|---|
| Pulse | `v0.4.2-0.20261016143631-7d70e1e14771` |
| Azure/go-workflow | `v0.1.14-0.20260504053439-7eff231079d7` |
| noneback/go-taskflow | `v1.2.0` |

The current Azure dependency pins [upstream fix `7eff231`](https://github.com/Azure/go-workflow/commit/7eff231079d7691d749787a8874514968acd15c7), which returns concurrency capacity before signaling completion to prevent lost wakeups ([#523](https://github.com/Luo-root/pulse/issues/523)). The adapter still uses native `MaxConcurrency` with unchanged timing boundaries. Historical reports retain their original `v0.1.13` identity and timeout statuses. This deadlock does not establish the cause of every historical timeout, and correctness regressions establish no new performance results. Check out the matching historical commit to reproduce old reports; save results from current code as a separate run without overwriting or mixing old samples.

Do not use `@latest` to reproduce pinned versions. Retain `go.sum` and module versions, download checksums and binary SHA256 in the sampling manifest. Supplementary `module-origins.json` queries actual Origin metadata from the same-version public module proxy; missing/failed entries are explicit, with no invented hashes. Results describe these versions, not every version of a project.

The full local matrix fixes `GOMAXPROCS=4` and a business concurrency limit of 4. Threads, concurrent business nodes and total graph size are different dimensions; save a separate matrix when changing any of them. Do not run compilers, race tests, profilers or other benchmark processes alongside timing samples. Power, temperature and unrelated workstation activity can still introduce variation; the measurements are not a production SLO.

## Build and correctness

From the repository root, run each command on its own line:

```sh
cd benchmarks/dag
go mod download
go build -o dag.exe .
go vet ./...
go test -race -count=1 -timeout=180s ./...
```

On Linux/macOS, use `dag` as the binary name. Set `GOWORK=off` (`$env:GOWORK = 'off'` in PowerShell or `export GOWORK=off` in Bash). Do not change the production module or add cross-repository replacements. Timing uses a non-race build; race tests are correctness gates only.

## Sample and plot

After correctness checks, sample serially from the repository root:

```sh
python benchmarks/tools/run_matrix.py --suite dag --module benchmarks/dag --binary benchmarks/dag/dag.exe --out benchmarks/results/local-dag --sizes 1,10,100,1000,10000 --rounds 1,256 --repeats 5 --procs 4 --parallelism 4
python benchmarks/tools/capture_origins.py benchmarks/results/local-dag/manifest.json --out benchmarks/results/local-dag/module-origins.json
python benchmarks/tools/summarize.py benchmarks/results/local-dag/samples.jsonl --out benchmarks/results/local-dag/figures
```

For powers of two, use e.g. `--sizes 1,2,4,8,16,32,64`. `--shapes`, `--modes` and `--engines` select subsets. Each CLI process runs one case. The sampler rotates engines between repetitions while executing them serially. Existing sample data is never overwritten by default; `--resume` requires identical configuration and binary contents.

Plotting requires Python matplotlib. Sampling and CSV summaries need only the standard library; `--no-plots` skips figures. Nothing automatically installs system software. Use the adapter's `-help` for its current CLI and native Go benchmark entry points.

The same sampler runs the Durable adapter from the other repository:

```sh
python benchmarks/tools/run_matrix.py --suite workflow --module ../pulse-durable/benchmarks/workflow --binary ../pulse-durable/benchmarks/workflow/workflow.exe --out ../pulse-durable/benchmarks/results/local-workflow --sizes 1,10,100,1000,10000 --rounds 1 --repeats 5 --procs 4 --parallelism 4
```

Use absolute paths if the repositories are not siblings. File-backend scratch data lives in the output directory's `work/`. Retain raw JSONL, full environment manifests, diagnostic logs and generated plot sets locally or in a separate evidence archive; `benchmarks/results/` is not tracked. Do not commit checkpoints, databases or binaries either. The source repository keeps reproduction tools, concise statistics/version summaries in `benchmarks/reports/`, and one PNG for each chart actually used by the site.

## Interpret the data

- `manifest.json` contains fixed configuration, Go/OS, module versions/checksums, binary SHA256 and source status. Compilation and module downloads are outside timing. Separate `module-origins.json` records supplementary metadata, its capture time, and missing/failed statuses.
- `samples.jsonl` retains each planned sample's original stdout/stderr, command, exit status and parsed fields. The Go program measures its internal operation time; `wall_seconds` also includes process launch and sampler waiting and is a different metric.
- `summary.csv` / `summary.json` group full configurations and report valid sample counts, median and observed minimum/maximum. Repeated batch means are not individual request p95/p99 latency.
- SVGs show whole-task time, time per business node and cumulative Go allocations. Error bars show observed min–max, not confidence intervals.
- `tools/storage_plot.py` plots storage growth for one fixed `shape/rounds/parallelism/procs` workload, allowing different node counts, engines and backends. Every record, including `not_repeated`, must carry a complete workload identity. Missing, invalid or conflicting fields are rejected before any CSV or plot is written. Split different workloads into separate input files and plot them separately.

`bytes_per_op` and `allocs_per_op` come from cumulative Go allocation counters. They are not peak heap, RSS or physical disk writes. Per-node time and throughput are derived from whole-task duration, not independently observed node latency.

Timeout, resource-limit and unsupported cases retain their status without fabricated values. After a limit, remaining planned repetitions are marked `not_repeated`; they are not replaced with zero. Limits describe measurement boundaries, not engine pass/fail criteria. Invalid output, checksum or topology stops the matrix after retaining the raw failing sample.

Read the [unified production comparison](../site/en/reference/production-comparison.md) for timing boundaries, executor reuse conditions and actual results, and inspect adapter `notes`.
