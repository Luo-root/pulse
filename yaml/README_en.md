[English](README_en.md) | [中文](README.md)

# yaml

`github.com/Luo-root/pulse/yaml` assembles YAML into a reusable `pulse.Graph` and a run-input plan, `SeedPlan`. It also offers static analysis without a host registry. This page references APIs, fields and assembly contracts. For a step-by-step tutorial, see the [declarative assembly guide](https://luo-root.github.io/pulse/en/guide/assembly).

YAML declares the topology. Host-registered named factories supply only a node's `pulse.RunFunc`; they do not return a Node or decide its wiring. This package provides YAML entry points, with no separate JSON entry point.

The dependency direction is `pulse` ← `yaml`. This package depends on the root `pulse` package, `gopkg.in/yaml.v3` and the standard library. The root package itself depends only on the standard library.

## Version and installation

Requires Go 1.25.0 or later. This page describes the development API; the latest release tag remains `v0.4.1`. Install merged interfaces from main and retain the resolved pseudo-version. For unmerged changes, use matching local source as described in [installation](../site/en/guide/installation.md):

```bash
go get github.com/Luo-root/pulse@main
```

## Complete example

This program embeds usable YAML and registers one `[]string` input key and one counting factory. It needs no files, environment variables or external services. A successful run prints `count=3`. Registration, Load, Inputs and Run errors are explicitly checked.

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

const document = `
version: 1
description: Count a literal list
seeds:
  - key: {name: items, type: "[]string"}
    from: {kind: literal, value: [alpha, beta, gamma]}
nodes:
  - id: count
    description: Print the number of items
    uses: demo.count
    requires: [{name: items, type: "[]string"}]
    provides: []
    timeout: 2s
    retry: {attempts: 3, delay: 10ms}
`

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	items := pulse.NewKey[[]string]("items")
	reg := pulse.NewRegistry()
	if err := pulse.RegisterKey(reg, items); err != nil {
		return err
	}
	if err := reg.Register("demo.count", func(rc *pulse.RunCtx) error {
		values, err := pulse.Get(rc, items)
		if err != nil {
			return err
		}
		if _, err := fmt.Printf("count=%d\n", len(values)); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}

	g, plan, err := pulseyaml.Load([]byte(document), reg, pulseyaml.LoadOptions{
		GraphID: "demo",
	})
	if err != nil {
		return err
	}
	in, err := plan.Inputs(nil)
	if err != nil {
		return err
	}
	if err := g.Run(context.Background(), in); err != nil {
		return err
	}
	return nil
}
```

The YAML decoder produces a generic list, which Inputs converts to the registered `[]string` type. This is literal shape alignment and needs no resolver.

## APIs and assembly stages

| Entry | Result | Use when |
|---|---|---|
| `Load(data, reg, opts)` | `(*pulse.Graph, *SeedPlan, error)` | YAML bytes and a host Registry are available, ready for execution |
| `LoadFile(path, reg, opts)` | Same as Load | Read a YAML file, then assemble under Load's rules |
| `Analyze(data, opts)` | `(*pulse.Topology, error)` | Inspect the document and static structure without a host Registry |
| `plan.Inputs(resolve)` | `(pulse.Inputs, error)` | Obtain this run's inputs before each execution |

Load requires a non-nil Registry. GraphID is required by both Load and Analyze. Load creates no Execution, runs no factories or aspects, and resolves no root-level external seeds. On failure it returns `nil, nil, err`; check the error before using either result. A root seed conflicting with a node's output is rejected in the check tree, before the real tree constructs ObserverFor instances.

| Stage | Main checks |
|---|---|
| Registry registration | Nonempty, unique factory names and non-nil bodies; keys sharing a name must have the same actual Go type |
| Load | Document version and node shapes, registered factories and keys, type tags, binds, graph references and reference cycles, child fixed inputs, and static problems at each child mount |
| SeedPlan.Inputs | Root resolver results, literal assignability, nil values and duplicate inputs |
| Graph.Validate | Sources, outlets, dependency cycles and other static problems in the whole definition; success seals the definition tree |
| Run / NewExecution / Start | Required inlets have supplied or explicitly skipped inputs for this run; Start rechecks static structure and seals the definition |

Load does not judge every root-level static problem in advance, such as missing root sources or dependency cycles. The host may still add root input declarations afterward. Call `g.Validate()` after completing those declarations, or let startup validate them. A child's interface is already determined by its reference site, so Load reports its static problems with the mount path.

Each SeedPlan.Inputs call creates an independent input table, allowing executions of one definition to receive different values. Table isolation is not a deep copy: pointers, maps and other mutable objects inside host values remain the host's responsibility.

### LoadOptions

| Field | Purpose | Rules |
|---|---|---|
| `GraphID` | Root definition identity | Required; the engine separately creates an execution identity for each run |
| `Graph` | Root `[]pulse.Option` | For WithObserver, WithAspects and similar options; children inherit the effective execution environment rather than redeclaring them at each layer |
| `MaxRunning` | Shared running-node quota for the declaration tree | Prefer configuring the quota here; defaults to unlimited, and mount nodes consume no quota themselves |
| `ObserverFor` | `func(path string) pulse.Observer` | Selects separate observers at child mount sites; omit when destinations need not differ |

A positive MaxRunning overrides WithMaxRunning in Graph. MaxRunning <= 0 does not override it. An int zero cannot distinguish an unspecified option from an explicit 0; for the default unlimited tree, avoid separately setting WithMaxRunning in Graph.

ObserverFor is called once per child mount during actual graph assembly, with paths such as `step` or `outer/inner`. Configure the root observer through Graph, not this callback. Returning nil leaves that site to inherit the parent execution's observer. The build-only validation tree and Analyze do not call ObserverFor or emit observation events. An inherited observer can still distinguish layers from every callback's Site.Path.

LoadOptions has no context field. A graph definition has no lifecycle. Run / NewExecution supplies the execution context, and child execution contexts derive from the mounting node's context.

## Registry and Go types

### Registration interfaces

| Interface | Contract |
|---|---|
| `pulse.NewRegistry()` | Creates an independent assembly-time registry |
| `pulse.RegisterKey(reg, key)` | Registers a Key name and actual Go type; repeating the same name and type is allowed, a different type is rejected |
| `reg.Register(name, run)` | Registers a named pulse.RunFunc, shaped as func(*pulse.RunCtx) error; duplicate names are rejected |
| `reg.TypeTagOf(name)` | Returns (string, bool), supplying the type tag to use in the document |
| `reg.ResolveKey(name, typeTag)` | Reconciles a name and nonempty type tag; unregistered keys or mismatched types are rejected |
| `pulse.MustRegisterKey` / `reg.MustRegister` | Panic versions of registration, for known-valid static initialization |

A "factory" here is a named Run body, not a constructor returning Node. Load finds it and builds a node from the YAML requires / provides declarations. The body uses declared keys through pulse.Get / pulse.Set; missing declarations and type mismatches remain subject to core validation.

### Type tags

Load requires KeySpec.type to exactly match the registered key's reflect.Type.String(). It is not an arbitrary nickname or a Go import alias.

| Registered Go type | Typical type tag |
|---|---|
| `string` | `string` |
| `[]string` | `[]string` |
| `map[string]int` | `map[string]int` |
| `Doc` defined in the current main package | `main.Doc`, or `[]main.Doc` for a list |
| `Doc` defined in package model | `model.Doc`, or `[]model.Doc` for a list |
| `any` | `interface {}` |

Named types retain their package and type names. A type alias uses the actual type it denotes, and import aliases do not change reflection tags. When diagnosing a mismatch, use TypeTagOf or the expected tag in the error instead of guessing abbreviations such as `[]Doc`.

## YAML field reference

These fields match the current [Document / NodeSpec / SeedSpec](load.go) and [GraphSpec](subgraph.go). Node declaration order does not determine execution order; data dependencies determine when Run can begin.

Load and Analyze accept exactly one YAML document. Appending another with `---`, including an empty document, returns an error. Trailing whitespace, comments, and a single document's end marker remain valid. Unknown configuration fields also return an error, including misspelled `timeuot` or `retry.attmpts`, so important settings cannot silently disappear. Custom `from.kind` values and arbitrary business fields inside `from.value` remain host-defined and are not restricted by the configuration field list.

Numeric values for `version` and `retry.attempts` at every node level must be YAML integer scalars within the host Go `int` range. Fractions, integral floats such as `1.0` or `2e0`, numeric strings, and out-of-range values return errors rather than being truncated. Omitted fields and `null` remain unset; version 0 / 1 and the default for attempts <= 0 keep their existing rules. This restriction does not apply to business `from.value` data, whose numbers follow the literal conversion rules below.

### Document and graph declarations

| Location / field | Syntax | Meaning |
|---|---|---|
| `version` | Integer; prefer 1 | Omitted, 0 or 1 is accepted; other versions are rejected |
| `description` | String | Human-readable root description in Topology / Snapshot; no execution semantics |
| `nodes` | NodeSpec list | At least one root node |
| `seeds` | SeedSpec list | Root inlet declarations and SeedPlan entries, retaining document order |
| `graphs` | Name → GraphSpec | Flat document-wide table of reusable child graphs |
| `graphs.<name>.description` | String | Description of each assembled child instance |
| `graphs.<name>.nodes` | NodeSpec list | At least one node per child graph |
| `graphs.<name>.seeds` | SeedSpec list | Fixed inputs at child mounts; only literal or skip |
| `observer` | String | Documentation hint only, ignored by Load; configure observers through LoadOptions |

GraphSpec has no further graphs table. Nesting references names in the top-level graphs table.

### Nodes

| Field | Factory node (uses) | Graph node (graph) |
|---|---|---|
| `id` | Required, unique in its graph, with no `/` | Same; also determines the mount path |
| `description` | Optional node description | Description of the parent-side mount node; GraphSpec describes the child itself |
| `uses` | Required registered factory name | Cannot coexist with graph |
| `graph` | Cannot coexist with uses | Required child graph name |
| `requires` | [{name, type}, ...]; omitted means empty | Nonempty declarations rejected; inputs derive from in |
| `provides` | [{name, type}, ...]; omitted means empty | Nonempty declarations rejected; outputs derive from out |
| `in` | Nonempty declarations rejected | {child key: parent key}, supplying parent values to the child |
| `out` | Nonempty declarations rejected | {child key: parent key}, bridging child outputs to the parent |
| `timeout` | Go duration string, such as 30s | Limits the entire child-graph step |
| `retry` | {attempts: 3, delay: 100ms} | Retries the entire child-graph step |

Every node must choose exactly one of uses and graph. A graph node needs at least one in or out bind. Explicit [] for requires / provides makes an empty input or output set visible.

The current syntax has no top-level output-port field or `fix:` field. Provides declares the keys a node may write; it does not automatically declare a root host outlet. To read a host result, declare the outlet with Go's DeclareOutput before execution and retain an Execution as shown in the [ReadOutput example](../examples_test.go).

### timeout and retry

Durations use Go's time.ParseDuration form, such as `30s` or `100ms`, not bare numbers. Timeout installs an aspect only when greater than 0. A present retry installs Retry; attempts includes the first try, values <= 0 mean one attempt, and delay <= 0 means no wait.

Assembly order is fixed: **Timeout outside, Retry inside**, regardless of YAML field order. Timeout therefore covers waiting for inputs, all attempts and their delays; it is not a separate budget for each attempt. Timeout is cooperative cancellation: the inner body must observe context. If it ignores cancellation, the framework still waits for it to return and does not forcibly stop its goroutine.

Normal skipping and an already-canceled context do not trigger retries. Retry stages engine outputs for each attempt, withholding failed-attempt outputs from downstream. External effects such as HTTP requests, database writes and files are not automatically rolled back. See the root Retry contract for further rules.

### KeySpec, SeedSpec and SeedFrom

| Field | Meaning |
|---|---|
| `key: {name, type}` | Required registered key name and type tag |
| `skip: true` | Explicitly skips this run's input; from is unused |
| `from.kind` | Literal or an empty string takes value directly; other values go to the host resolver |
| `from.value` | YAML literal value |
| `from.env` | Variable name for an env resolver |
| `from.path` | Path for a file resolver |
| `from.key` | Host key for a context or similar resolver |

At the root, env, file and context describe data passed to the resolver; they are not built-in framework IO. Other non-literal kinds are also left to the resolver. Inputs takes `func(from pulseyaml.SeedFrom) (any, error)`: the host decides how to read data and handle absence, and returning an error terminates Inputs. A closure can retain the request context when needed.

Literal and skip entries never call the resolver; nil is sufficient for a plan containing only those entries. A non-literal entry without a resolver fails. LoadFile reads the YAML document itself, not files referenced by seeds.

### Literal shape alignment

Inputs recursively aligns decoded YAML values to registered types using the same rules as pulse.SeedByName. Child literals are aligned by FixByName during Load.

| YAML value → registered type | Result |
|---|---|
| [a, b] → []string | Element-by-element conversion |
| {a: 1} → map[string]int | Key-by-key and value-by-value conversion |
| Matching-length list → array | Element conversion; lengths must match |
| Numeric value → numeric type | Range checks; float-to-integer requires an integral value and never truncates a fraction |
| Map → struct | No field guessing; fails, so return a correctly typed struct from a resolver |
| String ↔ number | No automatic conversion; fails |

Numeric conversion to floating point follows Go conversion rules, so integers may lose precision. A nil interface is not a host input value; explicitly skip missing values. Duplicate root inputs fail during Inputs. Duplicate child seeds fail during Load / Analyze.

## Analyze: static views without a Registry

This analysis helper can be placed in a program importing fmt and this package:

```go
func describe(doc []byte) error {
	top, err := pulseyaml.Analyze(doc, pulseyaml.LoadOptions{GraphID: "demo"})
	if err != nil {
		return err
	}
	if len(top.Problems) > 0 {
		return fmt.Errorf("static problems: %v", top.Problems)
	}
	if _, err := fmt.Print(top.Mermaid()); err != nil {
		return err
	}
	return nil
}
```

Analyze shares decoding, assembly, ports, binds and core static checks that do not depend on real key types with Load, and returns *pulse.Topology. It creates no Execution, runs no factories or aspects, resolves no external seeds and invokes neither ObserverFor nor Observer. Graph options are still applied; their preinstalled nodes, ports and Sub binds also become `any` placeholders so they can assemble with same-name YAML keys. Labels propagate across non-fixed host Sub binds; host internal keys are scoped by mount position, and a fixed input does not connect a same-name parent key. Host Go types do not participate in document label checks; Load / Validate / Start must still check actual types and runtime configuration.

| Check | Analyze | Load / Inputs / Validate / Start |
|---|---|---|
| YAML shapes, nodes, references and binds | Checked | Checked |
| Root static problems independent of real key types | Retained in Topology.Problems; the caller must inspect them | Load does not judge every root problem early; Validate / Start checks them |
| Child and assembly errors within that scope | Returned as errors | Returned as errors |
| Known type labels on same-name keys and bind ends | Must agree and propagate along binds | Checked against actual Registry types |
| Registered factories | Not checked; unknown names can pass | Checked by Load |
| A label denotes a real Go type | Not proved | Load checks the Registry |
| A literal is assignable to its target type | Not inferred or checked | Root Inputs and child Load check it |
| Stream consumers, shared quotas and lifecycle constraints that depend on real types | Not checked, even with explicit Stream / channel labels | Load uses real types and checks children; inspect root Topology.Problems and check through Validate / Start |
| Streaming behavior inside a factory | Not inferred | Cannot infer it from Run either |

Explicit KeySpec declarations still require type. Analyze preserves labels such as `example.Item` verbatim and does not infer a type from a literal. A key appearing only in wiring, whose type cannot be inferred from any known declaration, has an empty returned Type. Analysis assembles every key using an `any` placeholder; restoring labels before returning does not rerun checks that need real types. **Type labels are not proof of static safety.** For example:

```yaml
nodes:
  - id: writer
    uses: write
    provides: [{name: stream, type: "pulse.Stream[int]"}]
```

This document has no reader, yet Analyze returns empty Problems and preserves the stream label. Register a real `Key[pulse.Stream[int]]` and Load it: the same root graph's Topology.Problems reports the missing reader, and Validate rejects it, without running the factory. Successful Analyze still needs Load with a Registry, Inputs and Validate / Start to check real types, values and stream constraints.

Topology preserves every mount's complete input, output and pass-through interface; internal keys do not automatically become outlets. Go ports follow first successful declaration order. YAML in / out maps are sorted by key, root seeds follow document order, and child fixed inputs are declared after in ports in key-name order. For **graphs without streams**, known labels and matching identities, wiring and static options allow Analyze, Load and hand-built Go topologies to be compared field by field. Graphs with streams still support assembly previews, but their Problems need not match. Label checks also happen earlier, so documents with multiple errors are not guaranteed the same first diagnostic.

YAML does not declare Go stream-sugar markers such as Produce / Consume / Tee. Static checks cannot infer hidden channel waits or manual close timing inside factories or aspects.

## Reusable static subgraphs

Graphs declares topology templates; nodes[].graph statically mounts one at a step. This template is mounted twice. The child prefix is fixed, while topic comes from each site's in wiring:

```yaml
version: 1
graphs:
  enrich:
    description: Add a prefix
    seeds:
      - key: {name: sg.prefix, type: string}
        from: {kind: literal, value: "Summary: "}
    nodes:
      - id: work
        uses: demo.enrich
        requires:
          - {name: sg.topic, type: string}
          - {name: sg.prefix, type: string}
        provides: [{name: sg.summary, type: string}]
seeds:
  - key: {name: root.input, type: string}
    from: {kind: literal, value: "slot contract"}
nodes:
  - id: first
    graph: enrich
    in: {sg.topic: root.input}
    out: {sg.summary: root.first}
  - id: second
    graph: enrich
    in: {sg.topic: root.input}
    out: {sg.summary: root.second}
```

To run this fragment, register six string keys: root.input, root.first, root.second, sg.topic, sg.prefix and sg.summary, plus the demo.enrich body. The body reads sg.topic and sg.prefix and writes sg.summary.

Each mount creates an independent definition during Load and expands into the engine's static Sub. In / out derives the parent mount node's Requires / Provides. The complete subtree is visible through `g.Topology().Nodes[i].Sub` without running. Child IDs are `<root id>/<mount path>`; paths are first, second or outer/inner.

At run time, every mount and every retry attempt creates an independent child Execution with its own slots, first error and cancellation domain. The child context derives from the mount node. Parent cancellation propagates; bodies must still respond cooperatively.

### Wiring and fixed inputs

| Declaration | Direction and rules |
|---|---|
| `in: {child: parent}` | Parent read → child input; child becomes an inlet at this reference site |
| `out: {child: parent}` | Child outlet → parent output; child becomes an outlet at this reference site |
| Child seeds literal | Becomes a Fix bind at the mount; fixed values do not enter the definition |
| Child seeds skip | Becomes a FixSkip bind at the mount; from is ignored |
| The same child key in both in and out | Pass-through: the parent value is directly the child outlet value, with no internal reader or producer required |

Both keys must be registered and their actual types must match. A non-pass-through in key must be required by the child; a non-pass-through out key must be provided by it. An in key cannot also be produced inside the child or declared in that child's seeds, since either would give the input two sources. Pass-through still forbids an internal producer.

Non-skip child seeds support only literal or an omitted kind. Env / file / context and other kinds are rejected during assembly. Resolve external values through the root SeedPlan and pass them with in. Duplicate child seeds, unknown references, empty nodes, invalid binds and reference cycles are reported during assembly. A reference cycle includes a concrete path. Nesting itself has no fixed depth limit.

### Validation per reference site

Each reference site's in / out / seeds independently determines the spec's interface. If one of two mounts wires an input and the other does not, the second still lacks a source; the diagnostic points to that mount path. Validation does not merge the sites' inputs to excuse the missing bind.

Each child's sources, declared outlets and dependencies use the core Validate / Start criteria. An out-declared child outlet with no producer is therefore rejected during Load, without waiting until execution to discover that it never arrived.

Every graphs declaration is checked. A spec with no references has no mount interface: an unresolved Requires could become a future in inlet, so missing sources are not judged there. Known literal / skip seeds are still real external sources; a node providing the same key makes both Load and Analyze reject it with ErrDuplicateSource. Empty nodes, duplicate IDs, known source conflicts, reference cycles and dependency cycles remain checked. Load also checks its registered factories and actual types. Concrete nested references inside templates are still checked against their wiring.

An unreferenced template's own seeds retain their fixed-input facts too: Load translates them into the same Fix / FixSkip binds as a real mount on a build-only temporary host. Unknown inlets use ordinary In binds, remain unknown, and do not require future run inputs. Without effective aspects, the core propagates definite skips through ordinary-value relays and nested binds, avoiding missing-reader diagnostics for streams that cannot be produced. Literal-ready or unknown inputs can still produce streams; effective aspects can also write outputs, so skipping the body alone does not exempt reader checks. Analyze still does not prove these real stream constraints from type labels.

An unreferenced template borrows only the validation root's applied default declarations (Defaults). It never replays Graph Options or imports host-preinstalled nodes, ports, or Sub wiring; the temporary host does not declare those defaults a second time. Template count does not increase root Option calls, and root and template nodes may have the same ID in their separate definitions. Load retains quota and graph-aspect facts for static stream checks. Template checks read Topology.Problems without creating an Execution or calling the sealing Validate operation. The temporary host appears in neither the returned root graph nor SeedPlan; neither success nor a later template failure seals a shared host child through this path. The loader does not roll back arbitrary external effects of root Options themselves.

### Subgraph timeout and retry

Timeout / retry on a graph node applies to the parent mount node, controlling the entire child-graph step. Configuration on a factory node inside the child controls only that internal node.

Each whole-child retry attempt receives a new child Execution. Failed-attempt outputs bridged toward the parent are discarded with that attempt's write set. The parent receives outputs committed by a successful or normally skipped attempt, without mixing versions from different attempts. Multi-key commits become visible key by key, not atomically as a whole batch at one instant. External side effects are not automatically rolled back.

## Common questions

| Question | Action |
|---|---|
| Analyze succeeds, but Load reports an unknown factory? | Analyze does not use the host Registry; register the RunFunc named by uses |
| Why does []Doc mismatch the registry? | Obtain the reflection tag from TypeTagOf; named types typically include a package name |
| Load succeeds, but Run reports a missing source or input? | Load allows later root assembly and per-run input supply; check inlet declarations, Inputs and root Topology.Problems |
| Why does YAML observer have no effect? | It is a hint; configure the root observer with LoadOptions.Graph and separate child sinks with ObserverFor |
| How can a child read an external file? | Resolve a correctly typed root value, then pass it with in |
| How do I declare a host output? | Use Go DeclareOutput and read through Execution + ReadOutput; provides is not a host port |
| Does skip fail the whole run? | Normal skip is an arrival without a failure error; real errors retain their failure identity |

## Related documentation

- [Declarative assembly guide](https://luo-root.github.io/pulse/en/guide/assembly)
- [Load, SeedPlan and document fields](load.go), [Analyze](analyze.go), [subgraph assembly](subgraph.go)
- [Registry and type registration](../key.go), [input conversion](../inputs.go), [Retry / Timeout](../aspect.go)
- [Core static subgraph contract](../sub.go), [host result example](../examples_test.go)
- The assembly sections in the [design document](../docs/design/pulse.md)
