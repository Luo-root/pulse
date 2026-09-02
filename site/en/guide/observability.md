# Observability

`observability` is v2's official observability package: **Bootstrap + Record + Sink** — three things, kernel-only dependency, zero business imports (never imports llm/loop/flow).

## The three things

1. **Bootstrap**: the observability plugin, **used first** (a complete trajectory requires observability before every business plugin); subscribes to `fiber_state` / `loader_action` lifecycle events and prints the assembly-time snapshot banner;
2. **Record**: constructs `Record` (structured observation records with two-tier trace — `host_id` / `trace_id`, assembly-time / request-time);
3. **Sink**: the synchronous entry for constructed Records, a single `Write(observability.Record)` method; benchmarks use a black-hole sink (nopSink), production wires any backend.

## Design points

- **Side-band events**: observability subscribes via kernel On/Emit and **never enters** Waterfall chains — observation never changes business behavior;
- **Snapshot after subscription**: the Bootstrap banner is a post-subscription state snapshot (late-mounting observers don't miss history, because the snapshot rebuilds current state);
- **Two-tier trace**: `host_id` (assembly-time identity) + `trace_id` (request-time identity), threading four layers at runtime (host → scope → model → tools).

## Shortest usage

```go
host := kernel.New()
defer host.Dispose()

// observability loads first; Sink is the only interface you implement
if _, err := kernel.Use(host, observability.Bootstrap("myapp", mySink)); err != nil {
	return err
}
// then load business plugins: llm.Plugin(), toolset.Plugin() …
```

llm / loop / flow forward their events as Records via assembly-layer bridges (model calls, tool calls, node states) — the host gets a complete trajectory without hand-written instrumentation.

See the [observability package docs](/en/packages/observability/).
