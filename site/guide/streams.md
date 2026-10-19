# 流式处理

需要连续产生数据、逐条确认或跨子图传递实时结果时，使用 `Key[pulse.Stream[T]]`。槽位发布端点，端点管理消息序号、读取组、背压和生产终态。

本页 API **已合入 `main`，尚未随正式 tag 发布**；最新正式 tag `v0.4.1` 不包含受管 Stream。按[安装页](./installation.md#选择版本)选择主线开发版或与文档匹配的本地检出。

## 完整示例：一个额度，广播到两个消费端

程序发送 `1`、`2`、`3`，经 `TeeStream` 复制，两路各得到完整序列。保存为 `main.go`：

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
	g, err := pulse.New("stream-demo", "受管广播", pulse.WithMaxRunning(1))
	if err != nil {
		return err
	}
	if err := pulse.ProduceStream(g, "source", "生成数字", numbers,
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
	if err := pulse.TeeStream(g, "broadcast", "复制到两路", numbers, pulse.Keys(left, right)); err != nil {
		return err
	}
	var leftSum, rightSum int
	if err := pulse.ConsumeStream(g, "left-sink", "累计左路", left,
		func(rc *pulse.RunCtx, value int) error {
			leftSum += value
			return nil
		}); err != nil {
		return err
	}
	if err := pulse.ConsumeStream(g, "right-sink", "累计右路", right,
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

在保存 `main.go` 的新示例目录中，获取已合入主线的开发版并运行：

```sh
go mod init example.com/pulse-streams
go get github.com/Luo-root/pulse@main
go run -race .
```

`go get` 会将本次解析的具体版本写入 `go.mod`。如果要验证本地修改，可在已初始化的示例模块中改用本地替换；把 `../pulse` 调整为与本文档匹配的检出路径：

```sh
go mod edit -replace=github.com/Luo-root/pulse=../pulse
go mod tidy
go run -race .
```

输出：

```text
left=6 right=6
```

四个节点共享一个业务额度也能推进，因为阻塞的受管发送、接收与提交等待会暂时归还额度，让其他节点继续工作。等待结束后，返回业务代码、取消处理或 panic 展开前都要重新取得额度。其他节点不协作地长期持额时，取消返回也可能等待；引擎不会识别任意 HTTP、channel 或业务锁等待。

分别创建的根 Execution 各有自己的额度池。未自行声明 `WithMaxRunning` 的子图继承父执行的**同一份额度池**，不是每层各得一份；子图显式声明该选项时会另建自己的池。挂载 Sub 的步骤本身不占业务额度。

两份 sum 是仅运行一次的宿主示例状态，各由一个消费节点写、Run 完成后读。复用定义并发执行时，应改为独立的每次执行状态或输出；闭包不会自动隔离。

## 三个薄糖与选项

| 入口 | 回调或输出 | 处理边界 |
|---|---|---|
| `ProduceStream` | `fn(*RunCtx, *StreamWriter[T]) error` | 打开一个输出，回调逐条发送 |
| `ConsumeStream` | `fn(*RunCtx, T) error` | 每条 Receive → HandleMessage → 成功 Ack |
| `TeeStream` | `outs []Key[Stream[T]]` | 一条输入的一批分支输出确认后 Ack 输入 |

三者均为 `g, id, description, ...`，使用普通 `NewNode` 链。`WithStreamAspects(pulse.Timeout(time.Second))` 配置节点切面；图级与执行级切面仍在节点切面之外，重复选项按声明顺序追加，先声明的在外。选项复制切片，闭包中的业务状态仍共享。Timeout 等内层退出，真实错误和 panic 保留为失败。

`WithBuffer(n)` 控制**排队 + 已领取未确认**的总在途条数，受管流默认上限为 1；非正数不改变配置。Receive 不释放容量，Ack 才释放。ConsumeStream 不创建缓冲，传正数在装配期拒绝。TeeStream 为每个出口各设容量；慢支提供有界背压，不等待后代业务全部完成，也不承诺多分支同一瞬间可见。

`WithMessageHandling(pulse.WithMessageRetry(3, 0), pulse.WithMessageLimit(8))` 配置 ConsumeStream/TeeStream 的当前消息尝试；ProduceStream 没有输入消息，会拒绝非空消息配置。`WithStreamSource(id, resumable)` 声明来源续读能力，后文说明其责任。

## 手写 NewNode、Receive 和 Ack

以下函数放在已有 Go 程序中，需导入 `fmt`、`io` 和 `pulse`；输入键由上游提供。写端必须在自己的节点调用链内使用，不能留给节点返回后的 goroutine；同一 RunCtx（包括 Fork）的 I/O 串行使用。

```go
func addReader(g *pulse.Graph, key pulse.Key[pulse.Stream[int]]) error {
	return g.Add(pulse.NewNode("reader", "逐条处理并确认", pulse.Requires(key), nil,
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

生产端的等价形状是在声明 `Provides(key)` 的节点中调用 `writer, err := pulse.OpenStream(rc, key, pulse.WithBuffer(1))`，检查错误后 `writer.Send(value)`。节点本身的 Aspect 直接传给 NewNode；OpenStream 不接受 WithStreamAspects 或 WithMessageHandling。

Delivery 从序号 1 开始。Ack 对同一有效交付幂等；Nack 作废这份交付，将同一序号重投到同一读取组，不释放容量、不推进确认前沿。Nack/Stop 后旧交付再确认返回 `ErrDeliveryExpired`。取消不会自动 Ack；已完成的 Send/Receive 即使在重新排队取额时取消，也保留已成功的结果。

## 原生 select：Messages 配合 WaitStream

Messages 返回同一消息队列的 `<-chan Delivery[T]`，不创建转发 goroutine。它仅支持单读取者；FanOut 多 worker 必须用 Receive 登记交付归属。以下函数需要 `context` 与 `fmt`：

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
		fmt.Println(delivery.Value) // 取回额度后才执行业务。
		if err := delivery.Ack(); err != nil {
			return err
		}
	}
}
```

WaitStream 回调**只能等待和记录结果**，不能处理业务、重入或并发使用同一 RunCtx。channel 关闭后要检查 Snapshot.Err；关闭本身不能证明成功。

## 结束、停止与读取义务

`Snapshot()` 给出 `Terminal`、`Err`、`LastSequence`、连续确认前沿 `AckedThrough`、`InFlight` 和 `Stopped`，不含业务值。`Done()` 在实际生产节点**完整 Aspect 链**结束后关闭；完成、失败、取消、停止分别为 completed/failed/canceled/stopped。Done 不表示尾部已消费；别名共享同一本进度账，祖先 Sub 不是 EOF 边界，child 正常清理不会撤销尾部。

Receive 仅在正常生产结束且交付义务已清，或明确 Stop 后返回 io.EOF；生产失败返回原因。空流是 ready 端点、零条消息，正常成功；skipped 是没有端点，沿普通缺值语义处理。读取者应继续 Receive 并 Ack，直到 io.EOF，或明确 Stop。仅 InFlight=0 不代表生产结束，生产者仍可能继续发送；最后一个读取者此时提前返回会成为读取义务错误。若实际生产已正常结束且消息全部 Ack，收尾已满足条件，不强制额外读取一次 EOF。

流读者检查只豁免**输出确定 skipped、没有端点**的情况，不能只看业务体会不会跳过。全输入 skipped 时，节点、图级、继承或执行级切面仍可能在 `next` 前发布流，因此这类输出仍须接读者；例如容量 1 无人读取时，第一条 Send 可完成，第二条会等待容量。缓冲不是读者，静态检查也不推断切面实际会发送几条。无有效切面的正常 skip 仍可成功；`WithoutAspects()` 只截断继承，不移除本地、节点或本轮显式添加的切面。Topology/Validate 使用定义和 FixSkip 的事实，Start 还使用本轮 SkipSeed 与有效执行级切面；宿主出口的定义期容许仍按下节规则处理。

`stream.Stop(rc)` 显式结束整个读取组，作废剩余消息并唤醒生产者，不把清理算作 Ack。ConsumeStream 回调直接返回**精确的** `pulse.ErrStreamStopped` 表示当前条成功：先 Ack 当前条，再 Stop。包装的停止错误仍是失败。ProduceStream 仅在写端真的已 Stop 且回调返回精确 ErrStreamStopped 时正常结束；手写生产节点需自己处理正常停止，真实包装错误不能洗掉。TeeStream 单支 Stop 后其余支继续，全支 Stop 后正常 Stop 上游。

## 宿主在运行中订阅

给 `g` 声明 `DeclareOutput(pulse.NewPort(key))` 并添加生产者后，可用以下函数消费。需要导入 `errors`；skipped 出口也是正常结束。

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

SubscribeStream 必须在 NewExecution 后、Start 前登记，参与别名和单读取组校验；同一流不能再由独立图内消费者瓜分。Wait 包括全部节点和已登记必需宿主读取组的完成、Ack 或 Stop，**不会替宿主排空**。不读时应显式 Stop 或 Cancel。订阅的单次 Receive 上下文取消仅取消该次等待，不自动停止执行或确认。

仅宿主读取的根流应走上述 NewExecution → SubscribeStream → Start。Graph.Validate/Topology 允许已声明的 managed 根出口留待宿主连接，只省略该处缺读者诊断，不把出口算作真实 host；未导出流和原生 channel 的检查不变。因而持久集成可以先 Validate 冻结 schema，再于 BeforeStart 登记订阅。Execution.Start 仍按实际订阅严格检查无人消费和静默分流。ReadOutput 仍只在 Wait 后读结果。输入输出同键透传一旦被订阅，就必须为本轮 Seed 或 SkipSeed；缺值在 Start 前报错，明确 skip 正常。没有启动后补订阅、补历史或隐式无限缓冲。

## 跨 Sub 实时输出

以下装配函数可配合 readHost 使用：父图创建后调用它，声明父出口，再调用 readHost。子图内部没有输入也能生产。

```go
func addSubSource(parent *pulse.Graph, out pulse.Key[pulse.Stream[int]]) error {
	inside := pulse.NewKey[pulse.Stream[int]]("child.numbers")
	child, err := pulse.New("source-module", "可复用流来源")
	if err != nil {
		return err
	}
	if err := child.DeclareOutput(pulse.NewPort(inside)); err != nil {
		return err
	}
	if err := pulse.ProduceStream(child, "source", "产生三个数字", inside,
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
	return pulse.Sub(parent, "module", "挂载实时来源", []pulse.SubBind{pulse.Out(inside, out)}, child)
}
```

Sub 没有整体输入门。In 绑定稳定来源槽，即使仍 pending，内部节点也按自己的 Requires 激活；全入口 skipped 仍创建 child，独立自产分支照常运行。Stream 和原生 channel 的 Out 在 child.Wait 前绑定，普通值 Out 仍待 child.Wait 成功后桥回；挂载步骤本身仍等 child 最终结束，不占业务额度。

## 有限消息重试与渐进来源

有限变换用 HandleMessage：在回调外打开输出并取得 delivery，回调内只计算这一条的有限成果。以下函数显式设置 `WithMessageRetry(3, 0)`，表示当前交付**总共最多尝试三次：首次处理加最多两次重试**。失败时仍处理这份交付，不会先读取下一条：

```go
func transform(rc *pulse.RunCtx, delivery pulse.Delivery[int], writer *pulse.StreamWriter[int]) error {
	return pulse.HandleMessage(rc, delivery, func(rc *pulse.RunCtx, value int) error {
		return writer.Send(value * 2)
	}, pulse.WithMessageRetry(3, 0), pulse.WithMessageLimit(8))
}
```

不配置消息选项时，默认只尝试一次，最多暂存 64 条输出。每个输出批次还必须不超过该输出流容量：例如输出 `WithBuffer(1)` 时，一次尝试向它 Send 两条会当场失败，即使 `WithMessageLimit(8)` 允许更大的总条数。提交时会等待下游释放足够容量，但不会靠等待容纳一个超过整条流容量的批次。

若处理 2 失败一次，**回调调用顺序**是 1、2、**2**、3；上游仍只发送 1、2、3。失败尝试的 Set/Skip/Send 不正式发布；成功后确认输入，零输出和正常 NoValue/Skip 也成功。耗尽或提交失败会锁存失败，吞掉返回错误也不能成功。不能在回调内再 Receive、OpenStream、Stop、Ack/Nack、嵌套 HandleMessage、套普通 Retry 或用 SendAt。闭包、引用对象修改和外部副作用不会回滚。

逐 token 或其他渐进输出放在 HandleMessage **之外**逐条 Send/SendAt，已经外发的前缀不撤回。`WithStreamSource("feed-v1", true)` 只是承诺：业务必须读取 `writer.Position().Cursor` 并按该位置续读，使用 SendAt 将消息和来源游标一起确认。本地模式 SendAt 等同 Send，不保存可恢复游标。无法续读时明确失败或发起新 Execution，不自动整节点重试后把两个版本拼起来。

普通 Retry 对外接实际流输入/输出在 I/O 前明确拒绝；确定跳过、没有数据的路径仍是正常缺失。完整有限流水线封在 Sub 内、仅桥普通值时，可以整段重试；不要把 Retry 继承到内部流节点。增量检查点还需要[完整 StreamHooks](../reference/checkpoint.md)，存储与进程恢复由 Durable 提供，使用匹配本地检出；发布状态以实际 tag/release 为准。

## 从原生 channel 迁移

| 关注点 | 原生兼容入口 | 受管入口 |
|---|---|---|
| 键与糖 | `Key[<-chan T]`；Produce/Consume/Tee | `Key[Stream[T]]`；ProduceStream/ConsumeStream/TeeStream |
| 自写节点 | Set channel、select、发送端 close | OpenStream、Send、Receive、Ack/Nack |
| 缓冲 | 默认无缓冲；只限制排队 | 默认 1；限制排队和未确认总量 |
| 额度 | 整段持额，保守要求参与者可同时存活 | 受管等待让额，纯受管流水线额度 1 可推进 |
| 终态 | close 不是生产成功证明 | Snapshot.Err/Terminal 与 Done 独立记录 |
| 正常跳过 | Consume 回调返回 NoValue 后停止业务回调，按 Context 排空至关闭，整节点 skipped | ConsumeStream 的 NoValue 只跳过当前消息，确认后继续下一条 |
| 重试 | 无 Ack、无自动倒带或消息可靠性 | 显式有限 HandleMessage；渐进来源按游标续读 |
| 检查点 | 原生 channel 及嵌套 channel 仍拒绝 | 可选 StreamHooks，descriptor 与消息记录分开 |

旧三个糖保留，支持 WithStreamAspects；它们不能使用 WithStreamSource 或 WithMessageHandling 来静默获得新语义。受管流同一组 FanOut worker 用 Receive 竞争；广播用 TeeStream，多个独立读取者会被校验拒绝。静态校验追踪 In/Out/透传别名和实际额度池，不能推断业务或 Aspect 里的隐藏等待。YAML 登记同一 Key[Stream[T]] 并在 Run 工厂调用这些 core API 即可共用执行器；拓扑必须显式声明 Requires/Provides，藏在工厂里的 channel 不能当成声明齐全，见 [YAML 装配](./assembly.md)。
