# Unified production workflow comparison

[中文](README.md). This isolated module provides the comparison method and reproduction commands for **Pulse (with Durable for reliable execution), Eino and Temporal** in the Pulse v0.5.1 documentation.

Current reruns ([#502](https://github.com/Luo-root/pulse/issues/502), related Durable #81) pin public releases: Core `v0.5.0`, Durable `v0.1.0`, and `pulse-durable/sqlitestore v0.1.0`, with Go 1.26 and no replace. Eino `v0.9.21` and Temporal SDK `v1.50.0` / API `v1.63.5` remain unchanged. Durable actually uses the maintained official SQLite backend. Write new results to fresh directories; frozen results stay unchanged.

Results and measured identities are available in the [unified production comparison](../../site/en/reference/production-comparison.md) and each dataset's `identity.json`.

## Workload and adapters

- chain/wide/diamonds; 1, 10, 100, 1000, 10000 actual tasks; identical uint64 computation with 1 or 256 rounds. Input is `1 + sum(direct predecessor outputs)`; the result sums terminal outputs. Fixed Go golden values and an independent Python big-integer oracle check results.
- Shared spec/oracle preparation is outside adapter setup timing. One node is one real task, with no dummy source/sink tasks and no whole graph hidden in one Activity.
- Pulse reuses Graph with a fresh Execution. Eino explicitly uses AllPredecessor and unique output keys, merging only direct predecessors. Temporal uses a real Server and worker, one regular Activity per node, waiting for dependencies before acquiring a workflow semaphore; MaximumAttempts=1.
- GOMAXPROCS is 4 in each Go process, including the independent Server, and business concurrency is 4. This is not a four-CPU affinity limit shared by all processes. Eino's host semaphore bounds business bodies, not scheduling goroutines. Its host cancels and joins its own admitted bodies on return; this does not promise that all Eino internals have exited.
- plain retains native behavior. observe adds public Observer/callback/interceptor atomic counters without external logging. Event coverage and counts differ, so these are not prices for identical observability features.

## Guarantees and measurement scope

| Dimension | Pulse / Durable | Eino | Temporal |
| --- | --- | --- | --- |
| Native execution | Pulse in memory | compose Graph in memory | Always includes native persistence and service calls |
| Reliable record | Durable Store extension, confirmation before complete-node publication | No per-node save on ordinary success; checkpoint on active interrupt | Native event history and Activity completion confirmation |
| Restart | Kill host after confirmed prefix, reopen SQLite and resume | Exit after active checkpoint; rebuild in a new process and read the blob | Kill worker after confirmed prefix with no in-flight Activity; separately reopen Server |
| SQLite | Maintained official sqlitestore v0.1.0 | Benchmark CheckPointStore preserving the framework blob | Real Server embedded in CLI |

All SQLite hosts use WAL/FULL, retaining native transaction layouts, write counts and checkpoint granularity. Durable's official sqlitestore saves complete RunRecord JSON with CAS and owns OS run locks, without distributed leases or fencing. The thin wrapper adds only an independent inspection connection; all runtime persistence delegates to the official backend. Eino's blobStore retains its separate benchmark DB helper. Official v1 still supports independent reads of `durable_runs.record`, but strictly validates schema / `user_version=1` and rejects old benchmark databases: use fresh files. Locks moved from historical `.locks` to `.runlocks/<sha256>.lock`, plus `.init.lock`; do not delete lock files to unlock runs. Eino's active interrupt does not establish arbitrary-crash recovery. Restart probes intentionally choose an already confirmed boundary with no in-flight external effect; they do not establish arbitrary exactly-once effects.

CLI `start-dev` runs a real Temporal Service for this local experiment; it does not test production clusters, HA, network latency or production databases. Native scaling explicitly raises `limit.historyCount.error` to 200000 and warn to 100000. These are not default-configuration scores.

`server_setup_ns` isolates service preparation. `adapter_setup_ns` includes graph construction/Compile, or Dial/registration/worker.Start; these responsibilities differ. `first_ns` measures the first complete execution attempt (not a completed score when it fails). `run_ns` averages the subsequent reuse batch. Embedded engines batch small tasks to reduce timer noise; Temporal defaults to one reuse run. Actual batch lengths remain in raw data. Medians are medians of repeated batch averages, not online p50, and min–max is not a confidence interval.

Go allocations cover only the execution process during the reuse batch; Temporal's independent Server is excluded, preventing a whole-engine allocation ranking. A separate resource matrix uses `--iterations 1` for every engine: one first run and one reuse run per case, including the common host, preparation, GC and the actual service. Windows Job CPU includes exited descendants. RSS is the peak concurrent process-tree RSS sampled every 25ms. The common binary links all three libraries, so RSS includes its common harness baseline and is not a minimum standalone deployment footprint. Store snapshots sum DB/WAL/SHM file lengths, not cumulative writes, live-object sizes or per-task record costs.

Windows timings use QPC because this machine's Go time.Now granularity of about 0.5ms can otherwise record zero for small first runs. The 60s whole-case context includes setup and the first run; the parent watchdog is 65s. Partial runs retain first-run completion and planned/completed reuse counts, while incomplete-batch averages remain null. Following a limit, remaining repeats stay not_repeated; missing values never become zero. The Python recovery controller and Job CPU sampling target Windows in this version. Windows Job containment begins before suspended children run. Formal samples rotate engines serially, without concurrent builds, tests, profiles, plots or site builds.

## Reproduce

Use Go 1.26.0, Python 3.13 and psutil 7.2.2. Dependencies pin public versions without replace. Download Temporal CLI 1.9.1 (Server 1.32.0) into the workspace from its official release and verify SHA256 against checksums.txt; no global installation is required.

```powershell
$env:GOWORK = 'off'
$env:GOPROXY = 'https://proxy.golang.org,direct'
go mod download
go test -mod=readonly -race -count=1 -timeout=120s ./...
go build -mod=readonly -trimpath -o ../results/production-bench.exe .
# Start a real Server independently before enabling live tests:
$env:PULSE_TEMPORAL_ADDRESS = '127.0.0.1:7233'
go test -mod=readonly -race -count=1 -timeout=120s -run '^TestTemporal' -v .
# Replace <CLI-path>; choose a fresh ignored results or external output directory.
python ../tools/run_production.py --binary ../results/production-bench.exe --temporal-bin <CLI-path> --out ../results/production-native
python ../tools/run_production.py --binary ../results/production-bench.exe --temporal-bin <CLI-path> --out ../results/production-resources --iterations 1 --nodes 1,100,1000 --rounds 1 --modes plain --repeats 3
python ../tools/run_production.py --binary ../results/production-bench.exe --temporal-bin <CLI-path> --out ../results/production-checkpoint --engines pulse --modes checkpoint --rounds 1 --repeats 3
python ../tools/run_recovery.py --binary ../results/production-bench.exe --temporal-bin <CLI-path> --out ../results/production-recovery
```

New runs refuse to overwrite results. Native-matrix resume requires identical identity, binary and sampler hashes. Do not commit raw JSONL, stdout/stderr, databases, diagnostics or full machine inventories. Track only reproduction code/locks, compact summaries, version identity and figures actually referenced by pages.

After all four datasets finish, use `summarize_production.py --native <native-dir> --resources <resources-dir> --checkpoint <checkpoint-dir> --recovery <recovery-dir> --out <new-dir>`. It starts no engine; plotting needs matplotlib. If Go VCS auto-detection fails for an external linked worktree, add `-buildvcs=false` and retain explicit commit, source diff and hashes rather than omitting provenance.
