# #422 optimization candidates: final same-run evidence

This directory records the three steps for [#422](https://github.com/Luo-root/pulse/issues/422). Chinese: [README.md](README.md). It is separate from the original `../2026-10-17-dag/` baseline; do not calculate gains by mixing runs. See the [English performance report](../../../site/en/reference/performance.md) for results and costs.

## Measured code

| Variant | Exact Core commit | Meaning |
|---|---|---|
| baseline | `7d70e1e1477192669d5008d47729e3c8c19a336f` | Public pseudo-version `v0.4.2-0.20261016143631-7d70e1e14771` |
| index | `3a4c890334786b6d64c6904e24bbbf57c6b86e84` | #423, create the ID index only when a successful commit adds a second node |
| validation | `e33b5cbb27d038fcf6594e402c604037f01dac4b` | #424, also reuse ordinary-graph static checks and reduce input-map allocation |
| optimized | `f6049c4aac4d2d5316e0ced5959dc7bc2a257b46` | #425, also classify truly nil errors without allocating |
| taskflow | `v1.2.0` | Same benchmark binary as baseline, selected by engine flag |

Adapters and business computation are pinned to `benchmarks/dag` at `3ac6dca7aa4d4325acc4e6b20a72d39a603a779f`. Candidates are unpublished revisions, measured using generated external `-modfile` files with local Core source replacements. Production and shipped benchmark `go.mod`/`go.sum` files are unchanged. Binaries and generated replacement files are not committed. `prepare-commands.json` preserves exact preparation commands/paths; `manifest.json` and `acceptance.json` retain binary SHA256.

## Files and interpretation

- `plan.json` / `samples.jsonl`: 1380 unique planned identities and their original commands, stdout/stderr, exit codes and parsed results. All succeeded.
- `summary.json`: 276 groups of five successful samples, with min/median/max for every metric.
- `comparison.csv` / `comparison.json`: 90 baseline/final-candidate/Taskflow configurations covering three shapes × five sizes × two compute loads × three modes. Intermediate stages add only the three modes for chain/N10000/rounds1.
- `regression-observations.json`: every median more than 10% slower (none in this run), and the two candidate min–max ranges above baseline. Unfavorable points are retained.
- `large-correctness.json`: 30 instrumented ten-thousand-node checks across five variants × three shapes × two workloads, verifying node counts, values, concurrency and two runs of reused definitions before formal timing.
- `figures/`: 12 SVG/PNG pairs. Each shape/load pair shows the three modes for time or cumulative Go allocation. Bands are min–max across five batch means.
- `checks/`: raw final Core build/vet/full-race/dependency/whitespace evidence, mutation controls and Durable full race with the final Core, including process exit/recovery.
- `tools/`: original preparation, serial rotated sampling and summary/plotting scripts. They preserve this workstation's layout, not an installer. Adjust worktree locations for another machine and use a fresh output directory.
- `previous-candidate/`: versions, raw data, summaries and regression observations from the first formal candidate run. That run exposed additional N=1 index allocation, prompting delayed index creation. It is retained history and never enters final statistics or improvements.
- `evidence-checks.json`: hashes and identity counts for generated evidence in this directory; not a test runner.

Raw `samples.jsonl` SHA256: `7e76785fb76a682f48f888c2f5fd34961d280722a5dfbef20d4ab1807d3458b3`.

Time is the median of Go internally timed batch means. Process wall time includes startup and sampler waits and is a separate metric. Cumulative allocation is not peak heap/RSS; min–max is not a confidence interval; subtracting mode/stage medians is not a profile breakdown. No Observer/observe aspect or log sink is installed. Internal engine status and lifecycle maintenance remains timed.

## Reproduce the final candidate

Check out the exact commits above and confirm clean trees. Use Go 1.25.0, `GOWORK=off` and `GOFLAGS=-buildvcs=false`. Copy the pinned adapter's `benchmarks/dag/go.mod` and `go.sum` to a directory outside the project as `candidate.mod` and `candidate.sum`. From `benchmarks/dag`, substitute machine-local absolute paths:

```text
go mod edit -modfile=<external-directory>/candidate.mod -replace=github.com/Luo-root/pulse=<candidate-core-directory>
go build -modfile=<external-directory>/candidate.mod -o <external-directory>/candidate.exe .
<external-directory>/candidate.exe -engine pulse -shape chain -nodes 10000 -rounds 1 -mode reuse -parallelism 4 -procs 4 -iterations 1 -min-time 1ms -timeout 60s -verify=true
<external-directory>/candidate.exe -engine pulse -shape chain -nodes 10000 -rounds 1 -mode reuse -parallelism 4 -procs 4 -iterations 0 -min-time 100ms -timeout 60s -verify=false
```

Build the other variants using `prepare-commands.json`; baseline/Taskflow do not use candidate replacements. Finish all compilation, tests and instrumented verification before freezing binaries, then execute raw commands serially in `plan.json` order. Use `tools/measure_optimizations.py` with the same-version `benchmarks/tools/run_matrix.py` to retain the independent uint64 oracle, Windows JobObject watchdog, identity/exit checks, raw output and pre/post hashes. One copied CLI command is not reproduction of the full acceptance process.

Retain the CLI's 60-second case context and sampler's 65-second watchdog; case preparation and warmup also consume the case budget. Only run summaries and matplotlib after timing finishes. Save new runs to new directories. Changes in business computation, toolchain, concurrency or candidate commits also require a separate run.
