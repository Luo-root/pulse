# 流式处理

本页面向连续生产数据、逐项处理数据或需要广播同一条流的开发者。需要 Go 1.25+，并了解[编排与额度](./orchestration.md)。流式辅助函数使用开发版 API。

流的键是 `Key[<-chan T]`。槽位到达时发布的是 channel；channel 中的数据仍会继续到达，生产节点要在发送完毕后才结束。普通值可以直接被多个节点读取，channel 的多个读取者则会竞争其中的元素。

## 完整示例：广播给两个消费端

程序产生 `1`、`2`、`3`，通过 Tee 复制为两条流，每个消费端都计算完整序列的和。保存为一个空目录中的 `main.go`：

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
	g, err := pulse.New("stream-demo", "广播数字流", pulse.WithMaxRunning(4))
	if err != nil {
		return err
	}
	if err := pulse.Produce(g, "source", stream,
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
	if err := pulse.Tee(g, "broadcast", stream, pulse.Keys(left, right)); err != nil {
		return err
	}
	var leftSum, rightSum int
	if err := pulse.Consume(g, "left-sink", left,
		func(rc *pulse.RunCtx, value int) error {
			leftSum += value
			return nil
		}); err != nil {
		return err
	}
	if err := pulse.Consume(g, "right-sink", right,
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

在该目录运行：

```sh
go mod init example.com/pulse-streams
go get github.com/Luo-root/pulse@1f67045d6c42
go run .
```

预期输出：

```text
left=6 right=6
```

示例有生产端、Tee 和两个消费端，需要四个共享运行名额。每个和只有自己的消费节点写入，宿主在 Run 等全部节点退出后读取。它们是示例的宿主状态，不适合原样共享给同一定义的并发执行；可复用流程应把结果组织为每次执行的输出，或由宿主提供独立且同步的状态。

## 参数与关闭责任

| 函数 | 调用参数 | 回调职责 |
|---|---|---|
| `Produce` | `g, id, out, fn, opts...` | `fn(*RunCtx, func(T) error) error`，检查每次 `send` 的错误 |
| `Consume` | `g, id, in, fn` | `fn(*RunCtx, T) error`，处理一个元素 |
| `Tee` | `g, id, in, []Key[<-chan T], opts...` | 无业务回调，复制每个元素到全部出口 |
| `FanOut` | `g, id, in, outs, fn, aspects...` | `fn(*RunCtx, int, I) (O, error)`，普通输入共享；channel 输入显式竞争 |

Produce 与 Tee 的可选参数类型是 `StreamOption`，例如 `pulse.WithBuffer(8)`。Consume 没有缓冲或切面参数。FanOut 的末尾参数是 `Aspect`，不是 `StreamOption`。

Produce 创建并发布 channel，回调在节点内发送，节点返回时负责关闭；成功、错误和取消都会走关闭路径。不要保留 `send` 交给后台 goroutine，在回调结束后继续发送。

Consume 等待元素或取消信号，channel 正常关闭后结束。回调错误成为节点错误并取消本次执行；生产者失败也保留为执行失败，不会因消费者读到关闭而变成成功。

空流与跳过不同：生产者正常返回但一次未发送，channel 仍是 ready，消费者执行零次回调后完成。上游槽位为 skipped 时，消费者不进入业务体并正常跳过。

## 广播与竞争消费

Tee 为每个下游创建一条独立 channel，每个下游获得完整、同序的数据。最慢下游会对 Tee 施加背压，进而影响生产者。缓冲用于吸收短时速度差，不能解决没有消费者或额度不足的问题。

若每个元素只需要由一个 worker 处理，用一组 FanOut 竞争读取同一条 channel。以下片段用于替换完整示例的 Tee 与两个 Consume；需要已有 `g`、生产者的 `stream` 和 `r1`、`r2`（`Key[int]`），放在返回 `error` 的装配函数内：

```go
if err := pulse.FanOut(g, "worker", stream, pulse.Keys(r1, r2),
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

这两个结果各自的数值不固定，但总和为 6。后续可以用 Join 汇总普通输出。两个独立 Consume 共读一条已知流出口会被拒绝；一组 FanOut 是显式的竞争声明。不要把广播与竞争当作同一种行为。

## 静态检查与额度边界

装配辅助函数只做局部的快速检查。`Validate` 和 `Start` 会展开整棵静态挂载树，追踪 `In`、`Out` 与透传造成的 channel 别名，检查已知流的实际读取者、额度池和生命周期等待关系。Start 使用本次执行的有效额度与已提供的跳过输入。

每条已知 Produce/Tee 出口需要真实读取者，包括声明为根图出口的流。`ReadOutput` 在 Wait 后读取结果，不能作为运行中的流消费者。同一流的独立多读取者会被拒绝；同一挂载中的一组 FanOut worker 可以竞争。

共享池中的生产、转发和读取节点必须有足够同时存活的容量。Sub 不占名额；示例的两个 Consume 放进共享额度的子图，仍需要四个名额。子图自己声明额度时使用独立池，相同数字不代表同一个池，多处挂载也分别计数。

子图 Out 要等整个子执行结束后才桥回，Consume 要等 channel 关闭，Tee 的发送又可能等下游开始读取。这些可见关系如果形成等待环，会在 Validate/Start 被拒绝；无限额度或较大缓冲不能解除该规则。

静态容量检查是必要条件。Pulse 不会为一组流节点一次性预留额度，也不推断回调或切面内的隐藏等待、手写关闭时机、实际 Fix channel 指针别名。其他节点长时间占用额度时，流仍可能等待；宿主应提供取消上下文，并避免把隐藏阻塞混入同一额度池。

## 重试限制与常见错误

流的消费位置不能回退。Produce/Tee 在 Retry 写集内会因延迟发布 channel 而在运行期拒绝；消费者重试也不能重放已经消费的元素。不要对整张流图统一安装 Retry。检查点模式拒绝 stream/channel，包括接口或容器中隐藏的实际 channel。

遇到“无人消费”，添加 Consume 或 Tee 下游，而非只声明出口；遇到“名额不足”，按真实并发参与者设置额度，而非增大缓冲；遇到生命周期环，调整跨子图的输入输出，使读取不依赖自身所等待的完成事件。

## 下一步

运行诊断与图示见[观测与诊断](./observability.md)。精确错误处理见[错误参考](../reference/errors.md)，持久化能力与流限制见[检查点参考](../reference/checkpoint.md)。
