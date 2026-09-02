# flow orchestration

`kernel/flow` is a data-ready node orchestrator: nodes declare **what they need / what they produce**, and the graph schedules on slot readiness — topology order does not matter, slots do.

## Three core types

```go
g := flow.New(ctx)

kIn := flow.NewKey[string]("in")     // Key: typed slot handle
kOut := flow.NewKey[string]("out")

node := flow.NewNode("pass",         // node name
	flow.Requires[string](kIn),      // Requires: which slots (AND semantics)
	flow.Provides[string](kOut),     // Provides: which slots it produces
	func(rc *flow.RunCtx) error {    // node fn: runs only when data is ready
		v, err := flow.Get(rc, kIn)
		if err != nil {
			return err
		}
		return flow.Set(rc, kOut, v)
	},
)
_ = g.Add(node)
_ = flow.Seed(g, kIn, "war payload") // Seed: initial value for a source slot
err := g.Run()                       // blocks until all nodes terminate (one-shot)
```

## Three slot states

Every slot is in exactly one of three states: **pending → ready → skipped**.

- Skip is **arrival**, not failure — an upstream node produces a skip value and downstream decides the skip path by contract;
- A node error cancels the whole graph and is **never** rewritten as skip;
- An AND join = a node Requires multiple keys, executed only when all are ready.

## Fan-out / fan-in (DAG)

1 source → 2 parallel branches → AND join:

```go
kA := flow.NewKey[string]("a")
kB := flow.NewKey[string]("b")
join := flow.NewNode("join", flow.Requires[string](kA, kB), nil, func(rc *flow.RunCtx) error {
	a, _ := flow.Get(rc, kA)
	b, _ := flow.Get(rc, kB)
	out = a + b // join closure writes the terminal result
	return nil
})
```

Terminal results are written via the join closure — the Graph has no public post-Run slot reading, a contract workaround declared in the flow README.

## Composing with kernel

flow **does not import kernel** and runs graphs standalone; when needed, the assembly layer injects the kernel host / services into node closures via Go capture, and orchestration steps consume registered capabilities directly. Three orthogonal usages:

1. **Standalone** (as in the eval/war orchestration comparison);
2. **Kernel assembly** (node closures capture `*kernel.Context`; steps call `kernel.Get` for services);
3. **YAML declarative loading** (`kernel/flow/yaml`, E2):

```yaml
nodes:
  - name: a
    requires: [in]
    provides: [a]
  - name: join
    requires: [a, b]
```

## Performance: the price of a graph executor

Same-machine, same-task cross-framework numbers (see [Benchmarks](/en/eval)):

| Task | Pulse flow | Eino compose | Multiplier |
|---|---|---|---|
| T3 linear chain (3 passthrough nodes) | 8.6–8.9 µs / 73 allocs | 17.9–18.1 µs / 323 allocs | ~2.0× |
| T4 fan-out/fan-in DAG | 9.0–9.3 µs / 73 allocs | 30.3–37.9 µs / 411–462 allocs | ~3.3–4.1× |

**AND slots make fan-out free**: the DAG costs the same as the linear chain, same allocs. The counterpart's join scheduling runs ~1.7× over its own linear chain, and its field-mapping layer adds another +15–20%.

See the [flow package docs](/en/packages/kernel/flow/) and [flow/yaml package docs](/en/packages/kernel/flow/yaml/).
