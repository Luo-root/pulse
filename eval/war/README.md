[English](README.md) | [中文](README_zh.md)

# eval/war: Cross-Framework Benchmarks

Same machine, same task set, equally-thin stub models — measuring the infrastructure overhead of each agent framework (step one, phase two of the evaluation strategy, [Issue #103](https://github.com/Luo-root/pulse/issues/103)). **Standalone go.mod**: framework dependencies (Eino etc.) live only in this module; the Pulse main module stays pollution-free.

## Run

```powershell
cd eval/war
go test -bench . -benchmem -run '^$' .          # add -count=2 to observe variance
go test -run TestWarSanity -count=1 .           # correctness sentinel: every task really runs
```

## Contestants & Equivalence Declaration

| | Pulse | Eino v0.9.19 |
|---|---|---|
| Production entry | `loop.Agent.Run` | `adk.Runner.Run` (ChatModelAgent) |
| Production assembly | kernel host + observability.Bootstrap (nopSink) + llm.Registry (observed wrapper) + request scope | ChatModelAgent (compose tool node built in) + Runner |
| Stub model | `llm.NewScripted` (mutex + index + shallow copy) | `einoStubModel` (same semantics) |
| Tool | `MemToolSet` (counter + fixed JSON) | `tool.InvokableTool` (same semantics) |
| Orchestrator | `kernel/flow` Graph (Add + Seed + Run, AND slots) | `compose.Chain` (T3) + `compose.Graph` (AllPredecessor, T4) + `compose.Workflow` (T4 variant: field mapping) |
| T3/T4 assembly depth | flow runs standalone (`flow.New` + Add/Seed/Run, no kernel host — flow is usable on its own) | Chain/Graph/Workflow standalone Compile/Invoke (also no container) |
| Construction accounting | cold-start variant (full rebuild each run) + reused variant (assemble once) | cold-start variant (agent+runner rebuilt each run) + reused variant (assemble once) |

## Results (i9-14900HX / Go 1.25 / Windows, T1/T2 two rounds, T3/T4 three rounds; **run-to-run variance is significant — compare magnitudes and multiplier ranges, not single digits**)

```
BenchmarkWar_PulseTextRound-32          	  126460	     10497 ns/op	    8369 B/op	     139 allocs/op
BenchmarkWar_PulseTextRoundReused-32    	  348265	      3596 ns/op	    2368 B/op	      22 allocs/op
BenchmarkWar_EinoTextRoundReused-32     	   33534	     36595 ns/op	   27480 B/op	     407 allocs/op
BenchmarkWar_PulseToolRound-32          	   73598	     15871 ns/op	   11729 B/op	     177 allocs/op
BenchmarkWar_EinoTextRound-32           	   31104	     38789 ns/op	   28999 B/op	     425 allocs/op
BenchmarkWar_EinoToolRound-32           	   10000	    116405 ns/op	   89804 B/op	    1364 allocs/op
BenchmarkWar_PulseFlowChain-32          	   136314	      8861 ns/op	    5794 B/op	      73 allocs/op
BenchmarkWar_EinoChain-32               	    66663	     17918 ns/op	   24369 B/op	     323 allocs/op
BenchmarkWar_PulseFlowDAG-32            	   132871	      9084 ns/op	    5830 B/op	      73 allocs/op
BenchmarkWar_EinoDAGGraph-32            	    38792	     30312 ns/op	   30009 B/op	     411 allocs/op
BenchmarkWar_EinoDAG-32                 	    34663	     34770 ns/op	   35433 B/op	     462 allocs/op
```

| Task | Pulse | Eino | Multiplier range |
|---|---|---|---|
| T1 text round (reused: assemble once, pure runtime) | 3.6 µs / 22 allocs | 36.6–40.9 µs / 407 allocs | **~10–11×** |
| T1 text round (cold start: full rebuild each run) | 10.5–10.7 µs / 139 allocs | 38.8–39.0 µs / 425 allocs | **~3.7×** |
| T2 tool round-trip (cold-start upper bound) | 15.1–15.9 µs / 177 allocs | 116.4–117.7 µs / 1364 allocs | **~7.4–7.7×** |
| T3 linear chain (3 passthrough nodes, cold start) | 8.6–8.9 µs / 73 allocs | 17.9–18.1 µs / 323 allocs | **~2.0×** |
| T4 fan-out/fan-in (1 source → 2 branches → AND join, cold start) | 9.0–9.3 µs / 73 allocs | Graph keyed fan-in 30.3–31.6 µs / 411 allocs; Workflow field mapping 34.8–37.9 µs / 462 allocs | **~3.3–4.1×** |

## Side-by-side with the #102 Layered Baseline (same machine, same family)

| Layer | ns/op | allocs/op | Accounting |
|---|---|---|---|
| L0 bare Generate | ~23 | 1 | no framework |
| L1 Registry assembly | ~73 | 3 | + observed wrapper |
| L2a Agent text turn (reused) | ~2.6 µs | 22 | + loop turn machinery |
| L2b Agent tool round (rebuilt) | ~3.6 µs | 51 | + tool round-trip |
| L3 session bookkeeping | ~7.5 µs | 53 | + event-sourced accounting |
| **war T1 reused** | **3.6 µs** | **22** | ≈ L2a same-accounting rerun ✓ |
| **war T1 cold start** | **10.6 µs** | **139** | L2a + kernel assembly cold start |

The war T1 reused number cross-checks #102's L2a under the same accounting (3.6 µs vs 2.6 µs, same magnitude; the war side adds one `kernel.Use` chain check and the Bootstrap assembly constant) — the cross-PR numbers are self-consistent.

## Reading the Numbers

1. **Eino's cost lives in the per-run path, not construction** — the reused variant (36.6–40.9µs) ≈ the cold-start variant (38.8–39.0µs). Its ADK path runs a compose graph + event-stream iteration + callback dispatch on every Run; that machinery is the carrier for multi-agent orchestration and interrupt/resume. Pulse's direct-call path has no such layer and does not currently need one.
2. **Both are negligible against a real LLM call** (38µs vs seconds). This comparison quantifies the base price of an architectural choice (direct calls vs a graph executor); the gap compounds in high-frequency / high-concurrency / long multi-step scenarios. It is not a "Eino is unusable" verdict.
3. **T1 reused (pure runtime) 10–11× vs cold start 3.7×** — the gap's main body is the per-run path; the cold-start gap narrows because Eino's construction is comparatively cheap (same as point 1).
4. Upper-bound accounting: cold-start T2 rebuilds the full assembly every run (forced by the scripted-exhaustion semantics, same on both sides).
5. **Orchestrators (T3/T4): Pulse's fan-out is free; Eino pays for both join scheduling and field mapping** — the task set is pure zero-compute passthrough (measuring the graph executor: scheduling, data transfer, join synchronization). Pulse flow's three-node chain is 8.6–8.9µs / 73 allocs and the fan-out/fan-in DAG is 9.0–9.3µs / 73 allocs (AND slots make fan-out nearly free); Eino Chain is ~18µs / 323 allocs (~2.0×), the Graph keyed fan-in (`WithOutputKey` + default map merge) is 30.3–31.6µs / 411 allocs — join scheduling/merge costs ~1.7× over its own Chain; the Workflow field-mapped variant is 34.8–37.9µs / 462 allocs — another +15–20% over Graph (the field-mapping/type-inference tax, paid on both Compile and Invoke). Still negligible against a real LLM call; this quantifies the base price of an architectural choice.
6. **Reuse-semantics asymmetry**: a flow Graph runs once (`Run` submits all nodes and blocks until they all terminate; the instance is dead afterwards); an Eino `Runnable` can Compile once and Invoke N times. T3/T4 are single-run figures (Eino's Compile is included in every round) — in hosts that reuse a Runnable, Eino's per-run numbers would land below the table.

## Adding a Contestant

Implement three pieces (see `contestants.go`): `runXxxTextRound` / `runXxxToolRound` (production assembly + stub-alignment declaration) + benchmarks + a Sanity assertion; orchestration contestants add `runXxxFlowChain` / `runXxxFlowDAG` (task set: zero-compute passthrough). Assembly equivalence (stub thinness, task alignment, production entry) goes in the comments.
