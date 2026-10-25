# Compose business flows with existing mechanisms

This page is for developers building approvals, batches, or Agent tool loops. Identify what the application needs to retain and wait for, then compose Graphs, nodes, aspects, and Durable. A business name does not require a new engine node type.

## Branches, joins, and batches

Use `Set` / `Skip` to distinguish a value from normal absence. The default join waits for every input to arrive and enters the body if at least one carries a value; use `TryGet` to handle individual missing inputs. Explicitly return `WaitAll`'s result when every input must carry a value. Normal skipping can complete successfully; real failures retain their errors.

Use `FanOut` or ordinary nodes for a fixed number of parallel steps. When this run's input determines the item count, pass a slice to a processing node and iterate in its body, or let the host create independent Executions per batch. Assemble a graph for a batch before execution only when each item needs to appear as a static node. Do not append nodes to a sealed shared Graph or reopen slots.

Independent Executions have independent quotas. `Sub` inherits the parent execution's quota pool by default. To share that pool across the whole static subtree, do not declare `WithMaxRunning` on child definitions. An explicit child quota overrides inheritance, including 0 to opt out of the limit. Independent root executions do not automatically share a tree's quota or checkpoint scope.

## Put approvals before aspect advancement

For example, build first, wait for approval, then deploy. An approval aspect checks the approval record before `next(rc)` and advances the body only after approval. Waiting here has not acquired the node's business quota, so another independent node can run even with quota 1.

Short waits should watch `rc.Context().Done()` so the host can cancel. `pulse.NamedAspect("approval", gate)` can record actual aspect entry and exit, but does not create an application approval task. Application services still own approval state and approver identities.

For a long wait that releases the process, the host can compose Durable:

1. Reliably record the business wait with a stable RunID and step identity.
2. Cancel this Execution and wait for the run to return and release resources.
3. On approval, rebuild the same definition version and inputs, then Resume with the original RunID.
4. The aspect checks approval again; Durable restores confirmed upstream results and re-enters unfinished steps.

The application task can remain waiting while this Execution is canceled. Do not replace this protocol with an ordinary "still waiting" error: ordinary errors consume failure budget. Normal skip also means no result this run, rather than a pending approval.

The host protocol owns interruption windows between registration, cancellation, and wake-up, repeated events, and recovery scans. See the [Durable composition guide](https://github.com/Luo-root/pulse-durable/blob/main/site/en/guide/composition.md) for details and real process-recovery tests. Durable waits and a built-in wake-up service remain future capabilities. For multi-machine whole-workflow coordination, use `Submit/Worker` in the experimental [distributed module](https://github.com/Luo-root/pulse-durable/blob/main/distributed/README.md) on main rather than the local `Run/Resume` protocol above. It provides PostgreSQL-backed claiming, leases, fencing, and failover through the same checkpoint bridge, but does not provide an approval-wait protocol or dispatch individual workflow nodes across machines. The module has not been released.

## Call Graph tools from a loop node

A ReAct-shaped node can retain its current question, tool results, and stop condition inside the body. A tool registry maps a name to a call function. That function supplies this call's Inputs to a reusable tool Graph, creates a fresh Execution, waits for completion, and reads declared outputs with `ReadOutput`.

Every call creates a new execution without modifying the tool definition. Use `rc.Context()` for the loop, handle every call's error, and set an application stop condition or round limit. The mechanism requires no model service and accepts decisions from either a model or ordinary Go code.

This composition provides looping and tool selection. A single node's loop variables do not automatically become per-round durable state. To avoid repeating already completed tool calls after process exit, provide stable round/call identities and durable records, for example independent Durable runs managed by the host. Independent tool roots also do not automatically become part of the caller's static Sub tree.

## Continuous messages and progressive output

Compose pipelines with `ProduceStream` / `TeeStream` / `ConsumeStream`. Managed waits release business quota; Ack releases in-flight capacity. Register `SubscribeStream` before Start for live host reading. Use `HandleMessage` for finite message processing and `SendAt` with explicit positions for progressive sources. Whole-node Retry does not automatically rewind already sent data.

See [streams](streams.md) for complete programs. Application idempotency or transaction protocols still protect external database, HTTP, and file effects.

## Executable composition evidence

The repository's [composition tests](https://github.com/Luo-root/pulse/blob/main/tests/composition_scenarios_test.go) check two facts: an approval aspect does not occupy the only business quota while waiting, and a loop node calls the same tool Graph twice with independent execution results.

```sh
go test -race -count=1 -run '^(TestApprovalAspectLeavesQuotaAvailableBeforeNext|TestLoopNodeCallsReusableGraphTool)$' ./tests
```

These deterministic substitutes establish the composition mechanism. Model quality, external approval services, and per-round recovery each need their own application acceptance. Cross-process claims require Durable's real exit/restart tests.
