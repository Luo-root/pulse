# 观测与诊断

本页面向需要查看运行进度、记录执行结果或排查流程问题的开发者。需要 Go 1.26+，并了解[输入与输出](./inputs-outputs.md)。示例对应开发版。

Pulse 提供三种互补入口：`Topology` 查看定义，`Snapshot` 查看某次运行当前状态，`Observer` 接收生命周期回调。业务结果另由 `ReadOutput` 提供；结构与进度视图都不包含业务值。

## 完整示例：记录两次独立运行

下面的程序复用一张图运行两次。每次都创建新的观察者配置和 TraceID，以便把请求关联与执行身份分开处理。保存为一个空目录中的 `main.go`：

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
)

func run() error {
	result := pulse.NewKey[int]("result")
	g, err := pulse.New("observe-demo", "记录独立运行")
	if err != nil {
		return err
	}
	if err := g.DeclareOutput(pulse.NewPort(result)); err != nil {
		return err
	}
	if err := g.Add(pulse.NewNode("answer", "生成结果", nil,
		pulse.Provides(result), func(rc *pulse.RunCtx) error {
			return pulse.Set(rc, result, 42)
		})); err != nil {
		return err
	}
	for i := 1; i <= 2; i++ {
		sink := &observe.MemorySink{}
		obs, err := observe.NewRecordObserver(observe.ObserveConfig{
			Sink: sink, HostID: "docs", TraceID: observe.NewTraceID(),
		})
		if err != nil {
			return err
		}
		e, err := g.NewExecution(context.Background(), pulse.Inputs{},
			pulse.WithExecObserver(obs))
		if err != nil {
			return err
		}
		defer e.Cancel()
		if err := e.Start(); err != nil {
			return err
		}
		if err := e.Wait(); err != nil {
			return err
		}
		value, state, err := pulse.ReadOutput(e, result)
		if err != nil {
			return err
		}
		progress := e.Snapshot()
		fmt.Printf("run=%d status=%s value=%d state=%s records=%d\n",
			i, progress.Graph.Status, value, state, sink.Len())
	}
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
go mod init example.com/pulse-observation
go get github.com/Luo-root/pulse@main
go run .
```

预期输出：

```text
run=1 status=completed value=42 state=ready records=4
run=2 status=completed value=42 state=ready records=4
```

每次记录包括图开始、节点等待结束、节点执行结束、图结束四条。两次执行的图 ID 相同，Execution ID 和 TraceID 不同。`Wait` 返回时，图结束回调也已经返回；异步出口的实际排空仍需宿主执行 Flush 或 Close。

## 识别定义、运行与挂载位置

每条引擎回调都带 `pulse.Site`：

| 字段 | 含义 | 记录属性 |
|---|---|---|
| `Graph` | 定义身份，由 New 指定 | `pulse.graph` |
| `Execution` | 本次执行身份，进程内每个执行实例唯一 | `pulse.execution` |
| `Path` | 挂载位置；根为空，嵌套如 `outer/inner` | 非空时写 `pulse.path` |

同一子图在两处挂载时，Graph 可以相同，Path 和 Execution 不同；同一位置重试创建新的子执行时，Execution 也不同。Execution ID 不能充当跨进程的持久 RunID。

观察者默认由子图继承。一个出口可以接收整棵执行树的记录，无需按层创建观察者。`ObserveConfig.Path` 只是 Site 没有路径时的兜底，普通引擎运行通常留空；非空兜底也会作用于根记录。

## 直接看懂 Retry 与 Timeout

已有节点保持 `pulse.Retry(3, time.Second)` 的装法；按上例安装 RecordObserver 后，会额外收到 `pulse.aspect` 记录。比如第一次失败、第二次成功，阶段依次是：

```text
started
attempt_started  attempt=1 origin=new
attempt_finished attempt=1 status=failed
backoff_started → backoff_finished
retry           attempt=2 origin=new
attempt_started  attempt=2 origin=new
attempt_finished attempt=2 status=completed
finished        status=completed
```

attempt 包括首次，limit 是总上限。首次成功只有一次尝试，没有 retry 或退避记录；正常跳过以 skipped、Err=nil 收尾，不重试。嵌套 Retry 的边界号在同一 Execution + NodeID 内唯一，父边界与父尝试单独关联，不能只按节点名混加次数。原五种 Observer 回调次数不变，节点执行段仍覆盖全部尝试。

Timeout 自动报告自己的 started、deadline（若本层确实到期）、finished。deadline 不是业务退出：协作取消仍等内层返回，最终错误可能是保留下来的 panic 或可靠失败。父上下文取消、外层 Timeout 到期不会被算成本层 deadline。

独立 RPC 返回 context.Canceled / DeadlineExceeded，而执行域仍有效时，终态记录为 failed；业务返回后的 `defer rc.Cancel()` 清理不会把它改成 canceled。宿主取消、外层控制时限实际到期，或引擎已经锁存的明确取消来源仍记录为 canceled。未标记的局部 Fork 取消不能仅凭最终 context 状态推断为宿主中断。

自定义切面用 `pulse.NamedAspect("approval", approvalAspect)` 显式包装进出；裸函数不会被猜名，也不会自动猜审批原因。切面可用 `rc.Site()` 和 `rc.NodeID()` 读取真实位置；Fork 和 Timeout 保留同一 Site，返回的是不带业务值的副本。

要直接消费 Core 事实，可让原 Observer 另实现 `pulse.AspectObserver`，或填 `pulse.ObserverFunc.Aspect`。通过同一观察者选项装配，MultiObserver 隔离每个成员的 panic；额外回调与 Sink 重入 Snapshot 不持有引擎状态锁。无观察者时不写日志。

检查点编号来自实际持久游标，`origin` 区分 new、resumed；桥未提供可选 `CheckpointAttempt.Resumed` 时为 unknown。认回第 2 次不等于刚发生一次新重试；完整恢复的节点不产生历史切面记录。Scope / ParentScope 来自桥，稳定 RunID 由 Durable 的 Sink 适配补充。字段、耗时与每个阶段见 [observe 包参考](../packages/observe/index.md)。

## 查看进度与 Mermaid

以下片段用于已有 `g`、`e` 的宿主程序，需要 `fmt` 导入：

```go
progress := e.Snapshot()
fmt.Println(progress.Graph.Status)
fmt.Println(g.Topology().Mermaid())
fmt.Println(g.Topology().Mermaid(pulse.WithMermaidSnapshot(progress)))
```

`Topology` 包含完整声明端口、节点、绑定、静态子树和 `Problems`。读取不会封闭定义；它在尚未完成装配时也能返回问题列表。Mermaid 的结构来自 Topology，没有快照时不叠加运行状态。

快照中的节点按声明顺序排列。waiting 节点如果还有 pending 输入，就是在等数据；全部输入已到达时，是在等进入业务体的机会，可能受额度或切面影响。不能据此断言它正处于某个特定队列。

快照不是所有层和槽位在同一瞬间的原子采样，也不包含业务值。running 节点可能已有 ready 输出，其余输出仍 pending。Retry 中未发布的暂存不出现在快照中。

`NodeState.Child` 是最近一次已经创建的子执行句柄，失败或取消时仍保留，尚未创建则为 nil。调用 Child 的 Snapshot 会读取另一个时刻的状态。带快照的 Mermaid 按图 ID、Path、Node ID 匹配状态，子执行分别采样；固定值和错误正文不会进入图示。

## 选择记录出口

`observe.NewRecordObserver` 将引擎回调转换为 `Record`，通过 `Sink.Write(Record)` 输出。

| 出口 | 适用场景 | 收尾 |
|---|---|---|
| `MemorySink` | 测试、演示、断言 | Snapshot 查看记录 |
| `LineSink` | 终端或文本日志 | 检查 Flush 的返回错误 |
| `SlogSink` | 使用宿主 slog logger，输出文本或 JSON | 按宿主 handler 处理 |
| `MultiSink` | 同时写多个目的地 | 各目的地各自收尾 |
| `AsyncSink` | 包装较慢出口，有界队列 | 使用带上下文的 Flush/Close |

以下辅助函数演示 LineSink 收尾。需要 `context`、`errors`、`os`、`pulse` 和 `observe` 导入；`g` 是已装配图：

```go
func runLogged(ctx context.Context, g *pulse.Graph, in pulse.Inputs) error {
	sink := observe.NewLineSink(os.Stdout)
	obs, err := observe.NewRecordObserver(observe.ObserveConfig{
		Sink: sink, HostID: "app", TraceID: observe.NewTraceID(),
	})
	if err != nil {
		return err
	}
	runErr := g.Run(ctx, in, pulse.WithExecObserver(obs))
	return errors.Join(runErr, sink.Flush())
}
```

观察者回调同步发生在执行路径上，慢出口会延长记录的墙钟耗时。AsyncSink 可将落盘移到后台，队列满时默认回压；`DropOnFull` 允许丢弃，应检查 Dropped。Flush 只等待调用时已经入队的边界，后续写入不延长该范围；Close 停止接收并执行最终收尾。取消等待不能强制终止内层不接受上下文的阻塞操作。

## 记录、回调与恢复

一次正常完成的节点通常产生等待段和执行段两条记录。全部输入跳过的节点没有执行段；业务体返回 NoValue 时仍有执行段，只是终态为 skipped。切面在等待前失败时，记录为 `pulse.node_finished`，耗时为未测量的零值。精确回调次数、时序与事件表见[生命周期参考](../reference/lifecycle.md)。

Observer 没有返回错误的接口，panic 被隔离，不改变节点结果。输出失败应通过出口自己的错误接口检查。不要在回调中调用同一执行的 Start 或 Wait，它们在同一调用路径上，可能等待自身。

检查点恢复的已确认节点只发一次 Finished，不重跑业务体、切面或 Commit，也不伪造 Waiting、Running 和未创建的子图事件。RecordObserver 不为这种 completed/skipped 的单独 Finished 生成业务耗时记录；恢复事实由 Durable 自身的恢复事件和持久记录提供。

## 属性与数据边界

业务维度写入 `Attrs`，键名建议使用 `<组件>.<字段>`。值仅支持 string、int64、float64、bool 及其命名类型；记录字段不承担任意业务对象存储。字符串和 error 仍可能携带敏感内容，宿主应传入经过处理的属性和错误摘要。

`TraceID` 由宿主为一次运行生成或从入站追踪信息提取，不要跨独立请求复用。`observe.NewTraceID` 的格式不是 W3C trace-id；需要 W3C 或 OTel 关联时，由宿主传入自己的追踪 ID 与 span 语义。

## 常见错误与下一步

日志混入多个运行时，先按 Execution 和 TraceID 分组；缺少子图记录时检查观察者覆盖与路径；运行显示 running 而节点已结束时，确认宿主调用了 Wait。不要把快照当作恢复协议或业务结果容器。

[生命周期参考](../reference/lifecycle.md)给出精确事实，[错误参考](../reference/errors.md)说明诊断后的处理步骤。[YAML 装配](./assembly.md)还提供不执行业务的 Analyze 入口。
