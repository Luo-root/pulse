# Performance benchmarks

Framework infrastructure overhead is quantified with same-machine, same-task comparisons: `eval/war` (standalone nested module, [Issue #103](https://github.com/Luo-root/pulse/issues/103)) — Pulse full production assembly vs Eino v0.9.19's official production entry, equally-thin stub models, and a correctness sentinel asserting every task really runs.

!!! Accounting: i9-14900HX / Go 1.25 / Windows, `-count=5` (the war table is `-count=2`). **Compare magnitudes and multiplier ranges, not single digits** (run-to-run variance is significant); **allocation counts are stable run-to-run and are the first-class evidence — absolute ns are not**.

## Cross-framework (eval/war)

| Task | Pulse | Eino v0.9.19 | Multiplier range |
|---|---|---|---|
| T1 text round (reused: assemble once, pure runtime) | 3.2–3.7 µs / 22 allocs | 31.2–36.9 µs / 407 allocs | **~8.5–11.4×** |
| T1 text round (cold start: full rebuild each run) | 8.8–10.9 µs / 125 allocs | 30.5–31.3 µs / 425 allocs | **~2.8–3.6×** |
| T2 tool round-trip (cold-start upper bound) | 12.8–13.9 µs / 163 allocs | 94.2–96.0 µs / 1364 allocs | **~6.8–7.5×** |
| T3 linear-chain orchestration (3 passthrough nodes) | 8.3–8.4 µs / 73 allocs | 17.6–17.9 µs / 323 allocs | **~2.1×** |
| T4 fan-out/fan-in DAG (1 source → 2 branches → AND join) | 8.7 µs / 73 allocs | 30.5–35.7 µs / 411–462 allocs (Graph keyed fan-in / Workflow field-mapping variants) | **~3.5–4.1×** |

## Cross-checked against the layered baseline

The eval layered benchmarks (#102, L0–L3) cross-check the war numbers on the same machine:

| Layer | ns/op | allocs/op | Accounting |
|---|---|---|---|
| L0 bare Generate | ~25 | 1 | no framework |
| L1 Registry assembly | ~95 | 3 | + observed wrapper |
| L2a Agent text turn (reused) | ~2.9 µs | 22 | + loop turn machinery |
| L2b Agent tool round (rebuilt) | ~4.0 µs | 51 | + tool round-trip |
| L3 session bookkeeping | ~7.2 µs | 53 | + event-sourced accounting |
| **war T1 reused** | **3.2–3.7 µs** | **22** | ≈ L2a same-accounting rerun ✓ |

## Reading the numbers

1. **Eino's cost lives in the per-run path** — reused ≈ cold start. Its ADK path runs a compose graph + event-stream iteration + callback dispatch on every Run; that machinery carries multi-agent orchestration / interrupt capabilities. Pulse's direct-call path has no such layer and does not currently need one.
2. **Orchestration fan-out is free**: flow's AND slots make branch joins nearly free — the DAG (T4) costs the same as the linear chain (T3), same allocs; the counterpart's join scheduling runs ~1.7× over its own linear chain, and Workflow field mapping adds another +12–17%.
3. **Reuse-semantics asymmetry**: a flow Graph runs once; an Eino Runnable can Compile once and Invoke N times — T3/T4 are single-run figures, and reusable-host scenarios would lower the counterpart's per-run numbers.
4. **All gaps are negligible against a real LLM call (seconds)** — this quantifies the **base price of an architectural choice**; the gap compounds in high-frequency / high-concurrency / long-turn scenarios. It is not an "unusable" verdict.

## Reproduce

```bash
cd eval/war
go test -bench . -benchmem -run '^$' .   # add -count=2 to observe variance
go test -run TestWarSanity -count=1 .    # correctness sentinel
```

Plus **19 property invariants** (four themes in eval) continuously verifying the vocabulary / fold / registry engineering properties. See the [eval package docs](/en/packages/eval/) and [eval/war package docs](/en/packages/eval/war/).
