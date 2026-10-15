# Streams

This page is for developers producing data continuously, processing it item by item, or broadcasting a stream. It requires Go 1.25+ and familiarity with [orchestration and quotas](./orchestration.md). Stream helpers use development APIs.

A stream key is `Key[<-chan T]`. Slot arrival publishes a channel; elements keep arriving afterward, and the producer ends only after sending finishes. Several nodes can read the same ordinary value, but multiple channel readers compete for elements.

## Complete example: broadcast to two consumers

The program produces `1`, `2`, and `3`, copies them through Tee, and lets both consumers sum the complete sequence. Save it as `main.go` in an empty directory:

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
	stream := pulse.NewKey[<-chan int]("numbers")
	left := pulse.NewKey[<-chan int]("left")
	right := pulse.NewKey[<-chan int]("right")
	g, err := pulse.New("stream-demo", "Broadcast a number stream", pulse.WithMaxRunning(4))
	if err != nil {
		return err
	}
	if err := pulse.Produce(g, "source", "Produce a number stream", stream,
		func(rc *pulse.RunCtx, send func(int) error) error {
			for _, value := range []int{1, 2, 3} {
				if err := send(value); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
		return err
	}
	if err := pulse.Tee(g, "broadcast", "Broadcast the number stream", stream, pulse.Keys(left, right)); err != nil {
		return err
	}
	var leftSum, rightSum int
	if err := pulse.Consume(g, "left-sink", "Sum the left stream", left,
		func(rc *pulse.RunCtx, value int) error {
			leftSum += value
			return nil
		}); err != nil {
		return err
	}
	if err := pulse.Consume(g, "right-sink", "Sum the right stream", right,
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

Run in that directory. For examples from an unmerged branch, use the matching source checkout with the [installation page's local replacement steps](./installation.md); `@main` below only installs APIs already merged into main.

```sh
go mod init example.com/pulse-streams
go get github.com/Luo-root/pulse@main
go run .
```

Expected output:

```text
left=6 right=6
```

The producer, Tee, and two consumers need four shared quota slots. Each sum has one writing consumer, and the host reads it after Run waits for all nodes to exit. These are host-owned demonstration variables, unsuitable for unchanged reuse across concurrent executions of one definition. Reusable flows should organize results as execution outputs or use independent, synchronized host state.

## Parameters and close ownership

| Function | Arguments | Callback responsibility |
|---|---|---|
| `Produce` | `g, id, description, out, fn, opts...` | `fn(*RunCtx, func(T) error) error`; check every `send` error |
| `Consume` | `g, id, description, in, fn` | `fn(*RunCtx, T) error`; handle one element |
| `Tee` | `g, id, description, in, outs, opts...` | `outs` is `[]Key[<-chan T]`; no business callback; copy every element to all outputs |
| `FanOut` | `g, id, description, in, outs, fn, aspects...` | `fn(*RunCtx, int, I) (O, error)`; ordinary inputs are shared, channel inputs explicitly compete |

`description` is preserved as the node description, and an empty string is valid. All FanOut workers share that description and remain distinguished by `id-1` through `id-N`.

Tee copies the `outs` wiring list during assembly. Changing the caller's slice afterward does not change the graph's output declarations or broadcast destinations. This copies Key wiring only; business values in the stream are not deep-copied.

Produce and Tee accept `StreamOption` values such as `pulse.WithBuffer(8)`. Consume accepts no buffer or aspect arguments. FanOut's final arguments are `Aspect`, not `StreamOption`.

Produce creates and publishes the channel, invokes the callback inside the node, and closes the channel on return. Success, errors, and cancellation all take the close path. Do not retain `send` for a background goroutine that outlives the callback.

Consume waits for elements or cancellation and ends after normal channel close. Callback errors become node errors and cancel the execution. Producer failures remain execution failures; consumers seeing close do not convert them into success.

An empty stream is distinct from a skip. A producer returning normally without sending still publishes a ready channel; the consumer completes with zero callbacks. A skipped upstream slot instead skips the consumer without entering its body.

## Broadcast versus competition

Tee creates one independent channel per downstream, and every downstream receives the complete sequence in order. The slowest downstream backpressures Tee and therefore the producer. Buffers absorb temporary speed differences; they cannot fix missing consumers or insufficient quota.

When each element should be handled by one worker, use one FanOut group competing for the same channel. This fragment replaces the complete example's Tee and both Consume calls. It needs existing `g`, the producer's `stream`, and `r1`, `r2` (`Key[int]`), inside an assembly function returning `error`:

```go
if err := pulse.FanOut(g, "worker", "Sum assigned numbers", stream, pulse.Keys(r1, r2),
	func(rc *pulse.RunCtx, shard int, ch <-chan int) (int, error) {
		sum := 0
		for {
			select {
			case <-rc.Context().Done():
				return 0, rc.Context().Err()
			case value, ok := <-ch:
				if err := rc.Context().Err(); err != nil {
					return 0, err
				}
				if !ok {
					return sum, nil
				}
				sum += value
			}
		}
	}); err != nil {
	return err
}
```

Individual sums vary, but their total is 6. A subsequent Join can combine the ordinary outputs. Two independent Consume nodes reading one known stream output are rejected; one FanOut group explicitly declares competition. Broadcast and competition are different behaviors.

## Static checks and quota limits

Assembly helpers perform local fast checks only. `Validate` and `Start` expand the entire static mount tree, trace channel aliases through `In`, `Out`, and pass-through, and check actual readers, quota pools, and visible lifecycle waits. Start uses effective execution quotas and supplied skipped inputs.

Every known Produce/Tee output needs an actual reader, including a stream declared as a root output. `ReadOutput` reads after Wait and is not a live stream consumer. Independent readers of one stream are rejected; one FanOut group within a mount may compete.

A shared pool must have enough capacity for the producing, forwarding, and reading nodes to remain live together. Sub consumes no quota. Moving the example's two Consume nodes into children sharing the quota still requires four slots. Child quota declarations create separate pools; identical numbers do not imply the same pool, and separate mounts count separately.

Subgraph Out bridges only after the whole child execution finishes. Consume waits for channel close, while Tee may wait for a downstream reader to start. Cycles in these visible waits are rejected by Validate/Start; unlimited quota or large buffers do not bypass that rule.

Static capacity checks are necessary conditions. Pulse does not reserve quota for a group of stream nodes in one operation, infer hidden waits inside callbacks or aspects, infer manual close timing, or detect actual Fix channel pointer aliases. Other nodes can hold quota for a long time and leave a stream waiting. Supply a cancelable host context and avoid hidden blocking in the same pool.

## Retry limits and common errors

Stream consumption cannot be rewound. Produce/Tee inside a Retry write set is rejected at runtime because it delays channel publication. Consumer retries cannot replay elements already consumed. Do not apply Retry uniformly to a stream graph. Checkpoint mode rejects streams and channels, including actual channels hidden in interfaces or containers.

For “nothing consumes it,” add Consume or a Tee downstream rather than merely declaring an output. For insufficient quota, count actual concurrent participants instead of increasing buffers. For a lifecycle cycle, change cross-subgraph wiring so reading does not depend on the completion it is waiting for.

## Next steps

See [observation and diagnostics](./observability.md) for progress and diagrams, the [error reference](../reference/errors.md) for handling failures, and the [checkpoint reference](../reference/checkpoint.md) for persistence and channel limits.
