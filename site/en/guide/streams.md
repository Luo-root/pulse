# Streams

Use `Key[pulse.Stream[T]]` for continuous production, individual acknowledgements, or live results across subgraphs. The slot publishes an endpoint; the endpoint manages sequences, a reading group, backpressure, and production status.

These APIs **are merged into `main` but have not shipped in a release tag**. The latest released tag, `v0.4.1`, does not include managed Stream. Follow [installation and versions](./installation.md#choose-a-version) to select the main branch or a local checkout matching these docs.

## Complete example: one quota slot, two broadcast consumers

The program sends `1`, `2`, and `3` through `TeeStream`. Both consumers receive the complete sequence. Save it as `main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Luo-root/pulse"
)

func run() error {
	numbers := pulse.NewKey[pulse.Stream[int]]("numbers")
	left := pulse.NewKey[pulse.Stream[int]]("left")
	right := pulse.NewKey[pulse.Stream[int]]("right")
	g, err := pulse.New("stream-demo", "Managed broadcast", pulse.WithMaxRunning(1))
	if err != nil {
		return err
	}
	if err := pulse.ProduceStream(g, "source", "Produce numbers", numbers,
		func(rc *pulse.RunCtx, writer *pulse.StreamWriter[int]) error {
			for _, value := range []int{1, 2, 3} {
				if err := writer.Send(value); err != nil {
					return err
				}
			}
			return nil
		}, pulse.WithBuffer(1)); err != nil {
		return err
	}
	if err := pulse.TeeStream(g, "broadcast", "Copy to both branches", numbers, pulse.Keys(left, right)); err != nil {
		return err
	}
	var leftSum, rightSum int
	if err := pulse.ConsumeStream(g, "left-sink", "Sum the left branch", left,
		func(rc *pulse.RunCtx, value int) error {
			leftSum += value
			return nil
		}); err != nil {
		return err
	}
	if err := pulse.ConsumeStream(g, "right-sink", "Sum the right branch", right,
		func(rc *pulse.RunCtx, value int) error {
			rightSum += value
			return nil
		}); err != nil {
		return err
	}
	if err := g.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := g.Run(ctx, pulse.Inputs{}); err != nil {
		return err
	}
	fmt.Printf("left=%d right=%d\n", leftSum, rightSum)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
```

In a new example directory containing `main.go`, fetch the development version already merged into the main branch and run it:

```sh
go mod init example.com/pulse-streams
go get github.com/Luo-root/pulse@main
go run -race .
```

`go get` records the concrete version resolved by this invocation in `go.mod`. To test local changes, use a local replacement in the initialized example module instead. Adjust `../pulse` to the checkout matching these docs:

```sh
go mod edit -replace=github.com/Luo-root/pulse=../pulse
go mod tidy
go run -race .
```

Output:

```text
left=6 right=6
```

Four nodes can progress with one shared business quota slot because blocked managed sends, receives, and commit waits temporarily release quota so other nodes can work. They reacquire it before business code, cancellation handling, or panic unwinding resumes. Cancellation may therefore wait for another node that holds quota without cooperating. The engine cannot detect arbitrary HTTP, channel, or business-lock waits.

Each independently created root Execution has its own quota pool. A child graph without its own `WithMaxRunning` declaration inherits the parent's **same quota pool**, rather than getting another pool per level. An explicit child declaration creates its own pool. The Sub mount itself consumes no business quota.

The sums are host demonstration state for one run, each written by one consumer and read after Run. Concurrent reuse of a definition needs separate per-execution state or outputs; captured closure state is not isolated automatically.

## Three thin helpers and their options

| Entry | Callback or outputs | Processing boundary |
|---|---|---|
| `ProduceStream` | `fn(*RunCtx, *StreamWriter[T]) error` | Open one output and send incrementally |
| `ConsumeStream` | `fn(*RunCtx, T) error` | Receive → HandleMessage → Ack on success |
| `TeeStream` | `outs []Key[Stream[T]]` | Confirm one input's output batches, then Ack that input |

All take `g, id, description, ...` and use the ordinary `NewNode` chain. `WithStreamAspects(pulse.Timeout(time.Second))` configures node aspects. Graph and execution aspects remain outside local aspects; repeated options append in declaration order, earlier aspects outermost. Options copy slices, while captured business state remains shared. Timeout waits for inner work to exit; real errors and panics remain failures.

`WithBuffer(n)` limits **queued + received but unacknowledged** messages. Managed streams default to 1; nonpositive sizes leave configuration unchanged. Receive does not release capacity; Ack does. ConsumeStream creates no buffer and rejects a positive size during assembly. TeeStream gives each output its own capacity. A slow branch applies bounded backpressure; it does not wait for every descendant's business work to finish or promise simultaneous visibility across outputs.

`WithMessageHandling(pulse.WithMessageRetry(3, 0), pulse.WithMessageLimit(8))` configures the current-message attempts of ConsumeStream/TeeStream. ProduceStream has no input message and rejects nonempty message configuration. `WithStreamSource(id, resumable)` declares source continuation capability, with the responsibilities described below.

## Manual NewNode, Receive, and Ack

Add this function to an existing Go program with `fmt`, `io`, and `pulse` imports; an upstream producer supplies the input key. Use writers within their owning node's call chain, never in a goroutine that outlives the node. Serialize I/O on one RunCtx, including its Forks.

```go
func addReader(g *pulse.Graph, key pulse.Key[pulse.Stream[int]]) error {
	return g.Add(pulse.NewNode("reader", "Handle and acknowledge each item", pulse.Requires(key), nil,
		func(rc *pulse.RunCtx) error {
			stream, err := pulse.Get(rc, key)
			if err != nil {
				return err
			}
			for {
				delivery, err := stream.Receive(rc)
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				fmt.Println(delivery.Sequence, delivery.Value)
				if err := delivery.Ack(); err != nil {
					return err
				}
			}
		}))
}
```

The matching producer calls `writer, err := pulse.OpenStream(rc, key, pulse.WithBuffer(1))` inside a node declaring `Provides(key)`, checks the error, and calls `writer.Send(value)`. Attach aspects directly to NewNode; OpenStream rejects WithStreamAspects and WithMessageHandling.

Delivery sequences start at 1. Ack is idempotent on the same acknowledged delivery. Nack expires that delivery and requeues the same sequence in the same reading group; it releases no capacity and advances no acknowledgement frontier. An old delivery after Nack/Stop returns `ErrDeliveryExpired` on acknowledgement. Cancellation never automatically Acks. Completed Send/Receive operations retain success even if cancellation arrives while they are reacquiring quota.

## Native select: Messages with WaitStream

Messages returns the same queue as `<-chan Delivery[T]>` without a forwarding goroutine. It supports one reader only; competing FanOut workers must use Receive to register delivery ownership. This function needs `context` and `fmt`:

```go
func readNative(rc *pulse.RunCtx, stream pulse.Stream[int]) error {
	messages, err := stream.Messages(rc)
	if err != nil {
		return err
	}
	for {
		var delivery pulse.Delivery[int]
		var ok bool
		if err := rc.WaitStream(func(ctx context.Context) error {
			select {
			case delivery, ok = <-messages:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}); err != nil {
			return err
		}
		if !ok {
			return stream.Snapshot().Err
		}
		fmt.Println(delivery.Value) // Business work resumes with quota held.
		if err := delivery.Ack(); err != nil {
			return err
		}
	}
}
```

The WaitStream callback **only waits and records results**. It must not perform business work, reenter, or concurrently use the same RunCtx. Check Snapshot.Err after channel close; close alone does not prove success.

## Completion, stopping, and reading obligations

`Snapshot()` reports `Terminal`, `Err`, `LastSequence`, the continuous `AckedThrough` frontier, `InFlight`, and `Stopped`, without business values. `Done()` closes after the actual producer's **complete aspect chain** finishes. Production states are completed/failed/canceled/stopped. Done does not mean the tail has been consumed. Aliases share one ledger; an ancestor Sub is not an EOF boundary, and normal child cleanup does not withdraw the accepted tail.

Receive returns io.EOF only after normal production completion with no remaining delivery obligations, or explicit Stop. Production failure returns its cause. An empty stream is a ready endpoint with no messages and succeeds. A skipped slot has no endpoint and follows normal absence semantics. Keep calling Receive and Ack until io.EOF, or explicitly Stop. InFlight=0 alone does not mean production has ended: more messages may still arrive, and the final reader returning at that point causes an outstanding-reading-obligation error. Once actual production has completed normally and every message is Acked, finalization is satisfied without requiring one more Receive for EOF.

The reader check exempts only **an output proven skipped, with no endpoint**, not merely a skipped business body. With all inputs skipped, node, graph, inherited, or execution aspects can still publish a stream before `next`, so these outputs still need a reader. For example, with capacity 1 and no reader, the first Send can finish while the second waits for capacity. A buffer is not a reader, and static checks do not infer how many messages an aspect sends. Normal skipping without effective aspects still succeeds. `WithoutAspects()` cuts inheritance only, not local, node, or explicitly supplied execution aspects. Topology/Validate use definition and FixSkip facts; Start also uses this run's SkipSeed and effective execution aspects. The definition-only allowance for host outlets still follows the next section.

`stream.Stop(rc)` explicitly ends the entire reading group, expires remaining messages, and wakes the producer without counting cleanup as Ack. Returning **exactly** `pulse.ErrStreamStopped` from a ConsumeStream callback means the current item succeeded: Ack it first, then Stop. A wrapped stop error remains failure. ProduceStream finishes normally only when the writer really was stopped and its callback returns exact ErrStreamStopped. Manual producers handle normal stopping themselves; real wrapped failures must remain failures. A stopped TeeStream branch leaves other branches running; all branches stopped causes a normal upstream Stop.

## Subscribe from the host during execution

Declare `g.DeclareOutput(pulse.NewPort(key))` and add a producer first, then use this function. It also needs `errors`; a skipped output is a normal outcome.

```go
func readHost(ctx context.Context, g *pulse.Graph, key pulse.Key[pulse.Stream[int]]) error {
	e, err := g.NewExecution(ctx, pulse.Inputs{})
	if err != nil {
		return err
	}
	defer e.Cancel()
	subscription, err := pulse.SubscribeStream(e, key)
	if err != nil {
		return err
	}
	if err := e.Start(); err != nil {
		return err
	}
	for {
		delivery, err := subscription.Receive(ctx)
		if err == io.EOF || errors.Is(err, pulse.ErrSkipped) {
			break
		}
		if err == nil {
			fmt.Println(delivery.Value)
			err = delivery.Ack()
		}
		if err != nil {
			e.Cancel()
			_ = e.Wait()
			return err
		}
	}
	return e.Wait()
}
```

Register SubscribeStream after NewExecution and before Start. It participates in alias and single-reading-group checks; an independent graph consumer cannot divide the same stream with it. Wait includes all nodes and the completion, Ack, or Stop of required registered host reading groups; it **does not drain for the host**. Explicitly Stop or Cancel when no longer reading. Canceling one Receive context only interrupts that wait, without stopping the execution or acknowledging messages.

A root stream read only by the host follows NewExecution → SubscribeStream → Start. Graph.Validate/Topology allow declared managed root outlets to await host connection: only their missing-reader diagnostic is omitted, without counting an outlet as an actual host reader. Unexported streams and native channels retain their checks. Durable integrations can therefore Validate and freeze schema before registering subscriptions in BeforeStart. Execution.Start still strictly checks missing readers and silent splitting against actual subscriptions. ReadOutput still reads results only after Wait. Once an input/output pass-through is subscribed, this run must Seed or SkipSeed it. A missing value fails before startup; explicit skip is normal. There is no subscription after startup, historical catch-up, or implicit unbounded buffer.

## Live outputs across Sub

This assembly function works with readHost: create a parent, call addSubSource, declare the parent output, then call readHost. The child can produce independently without an input.

```go
func addSubSource(parent *pulse.Graph, out pulse.Key[pulse.Stream[int]]) error {
	inside := pulse.NewKey[pulse.Stream[int]]("child.numbers")
	child, err := pulse.New("source-module", "Reusable stream source")
	if err != nil {
		return err
	}
	if err := child.DeclareOutput(pulse.NewPort(inside)); err != nil {
		return err
	}
	if err := pulse.ProduceStream(child, "source", "Produce three numbers", inside,
		func(rc *pulse.RunCtx, writer *pulse.StreamWriter[int]) error {
			for _, value := range []int{1, 2, 3} {
				if err := writer.Send(value); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
		return err
	}
	return pulse.Sub(parent, "module", "Mount the live source", []pulse.SubBind{pulse.Out(inside, out)}, child)
}
```

Sub has no aggregate input gate. In binds a stable source slot, including pending slots, and internal nodes activate according to their own Requires. Even with every inlet skipped, the child is created and independent source branches may run. Stream and native-channel Out bindings are installed before child.Wait. Ordinary values still bridge only after successful child.Wait. The mount itself waits for final child completion and consumes no business quota.

## Finite message retry versus progressive sources

Use HandleMessage for finite transforms: open outputs and receive the delivery outside its callback, then compute only this item's finite results inside. This function explicitly sets `WithMessageRetry(3, 0)` for **at most three attempts in total: the initial attempt plus up to two retries**. Failed attempts handle the same delivery without reading the next one:

```go
func transform(rc *pulse.RunCtx, delivery pulse.Delivery[int], writer *pulse.StreamWriter[int]) error {
	return pulse.HandleMessage(rc, delivery, func(rc *pulse.RunCtx, value int) error {
		return writer.Send(value * 2)
	}, pulse.WithMessageRetry(3, 0), pulse.WithMessageLimit(8))
}
```

Without message options, the defaults are one attempt and at most 64 staged output messages. Each output batch must also fit that output's capacity. For example, with output `WithBuffer(1)`, a second Send in one attempt fails immediately even if `WithMessageLimit(8)` allows a larger total. Commit waits for downstream readers to free enough capacity. A batch larger than the stream's entire capacity is rejected immediately.

If processing 2 fails once, the **callback invocation order** is 1, 2, **2**, 3; the upstream producer still sends only 1, 2, 3. Failed attempts publish no Set/Skip/Send results. Success confirms the input, including zero outputs and normal NoValue/Skip. Exhaustion or commit failure latches failure; swallowing the returned error cannot report success. A callback cannot Receive again, OpenStream, Stop, Ack/Nack, nest HandleMessage, use ordinary Retry, or call SendAt. Closures, referenced objects, and external effects are not rolled back.

Send progressive tokens or similar output individually with Send/SendAt **outside** HandleMessage. Already published prefixes remain visible. `WithStreamSource("feed-v1", true)` is a promise: business code must read `writer.Position().Cursor`, continue from that location, and use SendAt to confirm each message with its source cursor. In local mode SendAt is Send and saves no recoverable cursor. If continuation is impossible, fail explicitly or start a new Execution; whole-node retries must not splice two generated versions together automatically.

Ordinary Retry explicitly rejects actual external stream inputs/outputs before I/O. Proven skipped paths carrying no data remain normal absence. A complete finite pipeline inside a Sub that bridges only ordinary values can be retried as a whole; do not inherit Retry into its internal stream nodes. Incremental checkpoints also need [complete StreamHooks](../reference/checkpoint.md). Durable provides storage and process recovery. Use matching local checkouts; actual tags/releases determine release status.

## Migrating from native channels

| Concern | Native compatibility entry | Managed entry |
|---|---|---|
| Keys and helpers | `Key[<-chan T]`; Produce/Consume/Tee | `Key[Stream[T]]`; ProduceStream/ConsumeStream/TeeStream |
| Manual nodes | Set a channel, select, sender-owned close | OpenStream, Send, Receive, Ack/Nack |
| Buffering | Default unbuffered; queued messages only | Default 1; queued plus unacknowledged total |
| Quota | Held throughout; conservative simultaneous-participant checks | Managed waits release quota; a purely managed pipeline can progress with 1 |
| Terminal result | close alone does not prove producer success | Snapshot.Err/Terminal and Done record separate facts |
| Normal skip | NoValue from Consume stops callbacks, drains under Context until close, then skips the whole node | NoValue from ConsumeStream skips and acknowledges only this message, then continues |
| Retry | No Ack, automatic rewind, or message reliability | Explicit finite HandleMessage; progressive sources continue by cursor |
| Checkpoints | Native and nested channels remain rejected | Optional StreamHooks; descriptors separate from message records |

The original three helpers remain and support WithStreamAspects. They reject WithStreamSource and WithMessageHandling rather than silently adopting new semantics. Managed FanOut workers compete using Receive; use TeeStream for broadcast. Independent multiple readers are rejected. Static checks trace In/Out/pass-through aliases and actual quota pools; they cannot infer hidden business or aspect waits. YAML can register the same Key[Stream[T]] and use these core APIs in Run factories with the same execution engine. Requires/Provides must remain explicit: a channel hidden inside a factory does not count as a complete declaration. See [YAML assembly](./assembly.md).
