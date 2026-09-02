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
| Construction accounting | cold-start variant (full rebuild each run) + reused variant (assemble once) | cold-start variant (agent+runner rebuilt each run) + reused variant (assemble once) |

## Results (i9-14900HX / Go 1.25 / Windows, -count=2; **run-to-run variance is significant — compare magnitudes and multiplier ranges, not single digits**)

```
BenchmarkWar_PulseTextRound-32          	  126460	     10497 ns/op	    8369 B/op	     139 allocs/op
BenchmarkWar_PulseTextRoundReused-32    	  348265	      3596 ns/op	    2368 B/op	      22 allocs/op
BenchmarkWar_EinoTextRoundReused-32     	   33534	     36595 ns/op	   27480 B/op	     407 allocs/op
BenchmarkWar_PulseToolRound-32          	   73598	     15871 ns/op	   11729 B/op	     177 allocs/op
BenchmarkWar_EinoTextRound-32           	   31104	     38789 ns/op	   28999 B/op	     425 allocs/op
BenchmarkWar_EinoToolRound-32           	   10000	    116405 ns/op	   89804 B/op	    1364 allocs/op
```

| Task | Pulse | Eino | Multiplier range |
|---|---|---|---|
| T1 text round (reused: assemble once, pure runtime) | 3.6 µs / 22 allocs | 36.6–40.9 µs / 407 allocs | **~10–11×** |
| T1 text round (cold start: full rebuild each run) | 10.5–10.7 µs / 139 allocs | 38.8–39.0 µs / 425 allocs | **~3.7×** |
| T2 tool round-trip (cold-start upper bound) | 15.1–15.9 µs / 177 allocs | 116.4–117.7 µs / 1364 allocs | **~7.4–7.7×** |

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

## Adding a Contestant

Implement three pieces (see `contestants.go`): `runXxxTextRound` / `runXxxToolRound` (production assembly + stub-alignment declaration) + benchmarks + a Sanity assertion. Assembly equivalence (stub thinness, task alignment, production entry) goes in the comments.
