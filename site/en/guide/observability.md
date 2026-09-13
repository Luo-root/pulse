# Observability

`observability` is v2's official observability package: **Bootstrap + Record + Sink** — three things plus the default per-request TraceID generator (`NewTraceID`), kernel-only dependency, zero business imports (never imports llm/loop/flow).

## The three things

1. **Bootstrap**: the observability plugin, **used first** (a complete trajectory requires observability before every business plugin); subscribes to `fiber_state` / `loader_action` lifecycle events and prints the assembly-time snapshot banner;
2. **Record**: constructs `Record` (structured observation records with two-tier trace — `host_id` / `trace_id`, assembly-time / request-time); business dimensions ride the open `Attrs` section, whose key contract is defined by the owning package (`llm.AttrModel` / `loop.AttrTool` / `flow.AttrNode`);
3. **Sink**: the synchronous entry for constructed Records, a single `Write(observability.Record)` method — the only interface you implement; or just use a built-in exit.

## Built-in exits

| Exit | Shape | Use when |
|---|---|---|
| `SlogSink` | `log/slog`, Text / JSON handler | You already have a logger, or need JSON structure |
| `LineSink` | Self-buffered logfmt-style text, **never through slog** | High-frequency single-host / file paths (recommended) |
| `MemorySink` | In-memory collection | Test assertions and demos |
| `MultiSink` | A `[]Sink` slice fanning out to several exits | Landing to file **and** collecting |

`AsyncSink` is a **wrapper**, not an exit: it wraps any slow exit (file / network) and moves delivery off the request path — `Write` only deep-copies `Attrs` and enqueues. It is a pessimization for already-fast exits (e.g. `MemorySink`), so don't apply it by default; when the sustained rate exceeds the exit's capacity the queue back-pressures to the exit's rate, which is the price of never dropping a record.

## Design points

- **Side-band events**: observability subscribes via kernel On/Emit and **never enters** Waterfall chains — observation never changes business behavior;
- **Snapshot after subscription**: the Bootstrap banner is a post-subscription state snapshot (late-mounting observers don't miss history, because the snapshot rebuilds current state);
- **Two-tier trace**: `host_id` (assembly-time identity) + `trace_id` (request-time identity), threading four layers at runtime (host → scope → model → tools);
- **`Attrs` is an insertion-ordered slice, not a map**: the first write reserves capacity for six entries, so common records allocate once; overwriting a key keeps its original position; the write surface is only the generic `Set[T AttrValue]` — there is no `map[string]any` escape hatch;
- **Request-level services are local bindings**: the `CollectorKey` mounted by `AttachCollector` is reachable only via `Get` from the request scope or a descendant — parents / siblings / other concurrent requests cannot see it (no cross-talk); it **does not participate in fiber dependency resolution**, so a plugin declaring `kernel.Require(CollectorKey)` sits silently in `inactive` — diagnose with `FiberSnapshots().WaitingFor`.

## Shortest usage

```go
sink := observability.NewLineSink(os.Stdout) // or &observability.MemorySink{}
defer sink.Flush()                           // Flush before closing (the last batch sits in the buffer)

host := kernel.New()
defer host.Dispose()                         // LIFO: Dispose runs before Flush

// observability loads first; Sink is the only interface you implement — or use a built-in exit
if _, err := kernel.Use(host, observability.Bootstrap("myapp", sink)); err != nil {
	return err
}
// then load business plugins: llm.Plugin(), toolset.Plugin() …
```

llm / loop / flow forward their events as Records via assembly-layer bridges (model calls, tool calls, node states) — the host gets a complete trajectory without hand-written instrumentation.

See the [observability package docs](/en/packages/observability/) (exit selection and accounting included).
