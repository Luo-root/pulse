[English](README_en.md) | [中文](README.md)

# observe

`github.com/Luo-root/pulse/observe` 把 Pulse 引擎的 `Observer` 回调转换为结构化 `Record`，再交给宿主选择的 `Sink`。本页是配置、事件和出口的包参考；逐步接入方法见[图观测指南](https://luo-root.github.io/pulse/guide/observability)。

依赖方向为 `pulse` ← `observe`。本包只依赖根包 `pulse` 和标准库；只使用根包时，无须引入观测包。

## 版本与安装

需要 Go 1.25.0 或更高版本。本页描述当前 `main` 的 API，其中部分接口尚未随正式 tag 发布。最新正式 tag 为 `v0.4.1`；要运行本页示例，请固定到包含这些接口的提交：

```bash
go get github.com/Luo-root/pulse@1f67045d6c42
```

## 完整示例

以下程序创建一个不需要输入的节点，将这一轮的记录写到标准输出。所有错误都向上传递；包括运行失败的路径，也会在退出前刷新缓冲并检查刷新错误。

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() (err error) {
	sink := observe.NewLineSink(os.Stdout)
	defer func() {
		if flushErr := sink.Flush(); flushErr != nil {
			err = errors.Join(err, fmt.Errorf("flush records: %w", flushErr))
		}
	}()

	g, err := pulse.New("demo", "A single observed node")
	if err != nil {
		return err
	}
	if err := g.Add(pulse.NewNode("work", "Complete one step", nil, nil,
		func(*pulse.RunCtx) error { return nil },
	)); err != nil {
		return err
	}

	obs, err := observe.NewRecordObserver(observe.ObserveConfig{
		Sink:    sink,
		HostID:  "demo-host",
		TraceID: observe.NewTraceID(),
	})
	if err != nil {
		return err
	}
	if err := g.Run(context.Background(), pulse.Inputs{}, pulse.WithExecObserver(obs)); err != nil {
		return err
	}
	return nil
}
```

正常运行会输出 `pulse.graph_started`、`pulse.node_wait_finished`、`pulse.node_run_finished` 和 `pulse.graph_finished`。时间、耗时与执行身份来自实际运行，每次都可能不同。

`g.Run` 包含创建 Execution、Start 和 Wait，因此返回前已发出运行结束回调。`LineSink` 默认缓冲 32 KiB，最后一批未必达到阈值；应在生产者停止写入后、关闭底层 writer 或退出进程前调用 `Flush()` 并检查结果。需要每条即时可见时，可在构造时加 `observe.WithImmediate()`，仍应检查最后的 `Flush()`。

## 配置与身份

`NewRecordObserver(cfg)` 返回 `(pulse.Observer, error)`；`cfg.Sink` 为 nil 时返回 `ErrNilSink`。

| `ObserveConfig` 字段 | 用途 | 使用规则 |
|---|---|---|
| `Sink` | 记录出口 | 必填，实现必须遵守下文的并发与只读契约 |
| `HostID` | 宿主的稳定身份 | 由宿主提供，例如服务或实例标识；空值不输出 |
| `TraceID` | 宿主的一次请求或逻辑运行的关联身份 | 由宿主单一生成源提供；同一次逻辑运行的适配器共享，独立运行应换新值 |
| `Path` | 回调没有路径时的兜底路径 | 通常留空；优先使用 `Site.Path`，非空兜底也会作用于根图记录 |

上述身份与引擎的执行身份各有职责：

| 身份 | 来源 | 区分什么 |
|---|---|---|
| `pulse.graph` | `Site.Graph` | 图定义 |
| `pulse.execution` | `Site.Execution` / `Execution.ID()` | 每一份独立 Execution；同一图定义跑两次会得到不同值 |
| `pulse.path` | `Site.Path`，否则 `ObserveConfig.Path` | 子图的挂载位置，例如 `step`、`outer/inner` |
| `HostID` | 宿主配置 | 哪个宿主产生记录 |
| `TraceID` | 宿主配置 | 跨图、跨组件的业务关联 |

节点记录还带 `pulse.node`；运行级记录不带节点属性。根图的 `Site.Path` 为空，普通配置下不写 `pulse.path`。所有引擎运行记录都带 `pulse.execution`，不能仅按 graph + path 汇总并发运行。

观察者内部按执行身份记账，可安全服务多个图和并发执行。但复用一个带固定 TraceID 的配置，会把独立请求关联到一起。对可重复运行的图，建议像示例一样每轮创建配置与观察者，用 `pulse.WithExecObserver(obs)` 注入；Sink 可以共享。定义级默认观察者使用 `pulse.WithObserver(obs)`。这两个选项都是覆盖单个观察者；需要组合宿主观察者时，用 `pulse.MultiObserver`。

子图未声明自己的观察者时继承父执行的有效观察者，每条回调已携带挂载位置，因此一个 Sink 可以接收整棵执行树的记录。树形结构可从 Topology 和 Execution 快照读取，Record 本身保持平面结构。

`NewTraceID()` 返回不透明的关联字符串，进程内并发唯一，跨进程通过随机段区分。它不是 W3C trace-id 格式；需要接入已有 trace context 时，应注入宿主自己的 trace ID。span、parent-id 和 trace-flags 由宿主管理。

## Record 字段

| 字段 | 类型 | 含义 |
|---|---|---|
| `Time` | `time.Time` | 记录时间；零值由 LineSink / MemorySink 补当前时间，SlogSink 使用 logger handler 的时间 |
| `HostID` / `TraceID` | `string` | 宿主身份与关联身份 |
| `Source` | `Source` | 本包产生的记录为 `SourceObserver`（`"observe"`）；宿主直写时可使用自己的来源 |
| `Event` | `string` | 事件名，见下表 |
| `Duration` | `time.Duration` | 实际观测到的分段或整轮墙钟耗时；未计时为零 |
| `Status` | `string` | `running` 或终止原因；宿主直写时可用自己的状态词表 |
| `Err` | `error` | 失败或取消的关联错误；正常跳过为 nil |
| `Attrs` | `Attrs` | 开放的标量属性，业务维度通过此字段加入 |

### Attrs

`Attrs` 零值可用；通过 `observe.Set(&attrs, key, value)` 写入，通过 `observe.Get[T](attrs, key)` 读取，并检查返回的 `ok`。值域为 string、int64、float64、bool，以及底层类型属于这四类的命名类型。

属性名使用 `<组件>.<字段>`，例如 `app.model`。引擎属性的常量由 `pulse` 定义。首次写入决定顺序，同名覆盖保持原位置；`Range`、LineSink 和 SlogSink 按插入序输出，`MarshalJSON` 按键排序。

Record 不提供任意对象或业务值容器。标量类型约束不能识别字符串中的敏感内容；需要过滤、截断或限制属性数量时，由宿主 Sink 处理。

## 事件参考

以下事件常量由 observe 定义。一次成功启动并通过 Wait 收尾的 Execution 有两条运行级记录；节点记录数量取决于实际经过哪些阶段。

### 运行级事件

| 常量 / 事件名 | 产生时机 | `Duration` | `Status` |
|---|---|---|---|
| `EventGraphStarted` / `pulse.graph_started` | 成功通过启动校验，在提交任何节点 goroutine 前 | `0` | `running` |
| `EventGraphFinished` / `pulse.graph_finished` | 全部节点终止后、Wait 返回前 | 两个图回调之间的整轮耗时 | `completed` / `failed` / `canceled` |

两条记录将本 Execution 的节点记录夹在中间。启动校验失败不会发出 started；只调用 Start、不调用 Wait 的宿主收不到 finished。重复或并发 Wait 不会重复发出 finished。

### 节点事件

| 常量 / 事件名 | 产生时机 | `Duration` | `Status` |
|---|---|---|---|
| `EventNodeWaitFinished` / `pulse.node_wait_finished` | 等待段结束：进入 Run，或等待中终止 | 等待段耗时 | 进入 Run 时为 `running`，否则为节点终态 |
| `EventNodeRunFinished` / `pulse.node_run_finished` | 已进入 Run 的节点终止 | 执行段耗时 | `completed` / `skipped` / `failed` / `canceled` |
| `EventNodeFinished` / `pulse.node_finished` | 只收到 Finished 的失败或取消，例如切面在调用 next 前拒绝执行 | `0`，未观测到分段 | `failed` / `canceled` |

Observer 的五种回调与上述五种 Record 事件并非一一对应：Waiting 和 Running 启动或结束计时，Finished 负责收尾。Retry 不重复发 Waiting / Running，因此重试不会变成多套节点分段记录，执行段可包含多次尝试及间隔。

### 跳过、短路与恢复

| 节点经历 | 产生的节点记录 |
|---|---|
| 全部输入跳过，未进入业务体 | 一条 `skipped` 的等待结束记录 |
| 业务体进入 Run 后返回 `pulse.NoValue()` | 等待结束记录 + `skipped` 的执行结束记录 |
| 切面在 next 前失败或取消 | 一条无计时的 node_finished，保留完整图、执行、路径和节点归因 |
| 切面在 next 前成功或正常跳过 | 不伪造等待或执行记录 |
| 检查点恢复的 completed / skipped 节点 | 不伪造业务计时记录；恢复事实由 Durable 事件报告 |

正常跳过的 `Record.Err` 为 nil；即使全部节点跳过，运行仍是 `completed`。可靠检查点故障保留失败身份与错误链，即使错误链包含 `ErrSkipped`，也不能按这个错误标记过滤为正常跳过。

### 耗时口径

耗时是墙钟时间，不是 CPU 时间。回调在调用方 goroutine 上同步执行：节点回调在节点 goroutine 上，图回调在 Start / Wait 调用路径上。落在计时窗口内的 Sink 开销会计入相应分段或整轮耗时；慢出口也会直接延迟调用方。AsyncSink 延后内层写入，但排队和入队本身仍在回调路径上。

零耗时既可能表示没有计时，也可能是计时精度不足；判断是否进入某阶段应看事件类型。LineSink 内置版式用 `-` 显示零耗时。SlogSink 的 `duration_ms` 是数值毫秒，保留亚毫秒精度，例如 585 微秒输出为 `0.585`；零耗时省略该键。

## 状态视图与事件历史

| 入口 | 回答的问题 | 边界 |
|---|---|---|
| `g.Topology()` | 图声明了哪些节点、键、端口和子树 | 静态结构，不创建执行 |
| `e.Snapshot()` | 这一轮目前进行到哪里 | 不依赖 Observer、不产生事件，不带业务值 |
| `MemorySink.Snapshot()` | 出口已经收到了哪些记录 | 返回记录切片的浅拷贝，不是执行状态快照 |

执行状态在回调前迁移，所以回调内可以读取 `e.Snapshot()`。其中 `Graph.Execution` / `Graph.Path` 与回调的 Site 和 Record 的归因属性一致。`waiting` 节点若还有 pending 输入，就在等输入；输入全部到达或没有输入时，在等进入 Run 的机会，不能仅据此认定它在等并发名额。

Snapshot 分别采样定义、节点状态与槽位，不保证某一瞬间的原子视图。`Child` 是最近一次已创建尝试的活子 Execution，失败或取消后仍保留；继续调用 `Child.Snapshot()` 会得到另一个时刻的读数。图在 Wait 收尾时转为终态，只 Start 不 Wait 时仍为 running。

可用 `g.Topology().Mermaid(pulse.WithMermaidSnapshot(e.Snapshot()))` 把当前状态叠加到静态结构上。无快照时只画结构；固定值和错误正文不进入 Mermaid 图。

## Sink 选择与契约

| 出口 | 适合场景 | 生命周期与错误检查 |
|---|---|---|
| `NewLineSink(w, opts...)` | 给人阅读的列式文本 | 自带缓冲；结束时检查 `Flush()`，运行中可检查 `Err()` |
| `SlogSink{Logger: logger}` | 接宿主 logger、输出 JSON、接日志采集器 | Logger 为 nil 时用 `slog.Default()`；handler 生命周期由宿主管理 |
| `&MemorySink{}` | 测试断言、演示、收集事件 | 用 `Len()` / `Snapshot()` 读取；记录保留在内存中 |
| `MultiSink{sink1, sink2}` | 同一记录写到多个出口 | 按序调用、跳过 nil 成员；宿主负责分别刷新或关闭叶子出口 |
| `NewAsyncSink(inner, opts...)` | 将较慢出口的写入交给单个后台 worker | 用 `Flush(ctx)` / `Close(ctx)` 检查结果，并查看 `Dropped()` |

`Sink` 只要求一个方法：

```go
type Sink interface {
	Write(Record)
}
```

实现必须并发安全，且不应长时间阻塞；接口没有 context 或返回错误。需要截止时间、缓冲或错误上报的实现，应自行提供相应方法或内部队列。普通 Observer 的 panic 不会升级为节点失败，因此宿主必须单独检查出口错误。

引用语义的责任如下：

- 产生方为每条记录构造独立 Attrs，交给 Write 后不再修改。
- Sink 只读消费 Record 及其 Attrs。MultiSink 会把相同的 Attrs 交给多个成员。
- 异步实现持有记录前须复制需要的引用字段；内置 AsyncSink 会复制 Attrs 条目。
- MemorySink.Snapshot 只复制外层记录切片，应继续按只读契约使用其中的 Attrs。

### LineSink 选项

| 选项 | 默认值 / 行为 |
|---|---|
| `WithBufSize(n)` | 32 KiB；n 必须大于 0 |
| `WithImmediate()` | 等价于阈值 1 字节，每条记录立即写到底层 writer |
| `WithPrefix(text)` | 默认 `PULSE`；空字符串关闭前缀 |
| `WithColor(on)` | 未指定时，仅对 `*os.File` 字符设备启用 ANSI 颜色 |
| `WithRenderer(fn)` | 替换行体渲染器；fn 不得为 nil |

默认版式依次输出时间、状态、带单位耗时、事件、来源、属性、宿主、错误和 trace。状态列宽为下限，长值不截断；属性按插入序保留，文本值按需加引号。颜色依据错误与耗时，不根据自由状态文本猜测业务语义。默认渲染复用缓冲、不经过 slog；自定义渲染器的分配由实现自身决定。

底层 writer 为 nil、缓冲大小非法或渲染器为 nil，属于构造时的编程错误，会 panic。LineSink 会串行调用 writer，所以 writer 无须因这个 Sink 而额外支持并发。写错误以首错为准，`Err()` 和 `Flush()` 都可返回它；失败批次会清空以避免无限积累。Flush 只负责写出缓冲，不关闭底层 writer。

## AsyncSink 参考

### 队列与背压

| 选项 / 方法 | 语义 |
|---|---|
| `WithCapacity(n)` | 预分配有界队列，默认 1024 条；n 必须大于 0 |
| 默认满队列策略 | 阻塞生产者，保留背压 |
| `DropOnFull()` | 满时丢弃新记录，计入 Dropped |
| `Dropped()` | 合计未送达的记录，包括满队列丢弃、关闭后写入、关闭唤醒的阻塞写入、关闭超时残留和 inner.Write panic |

inner 为 nil 或容量非法时，NewAsyncSink 会 panic。单个 worker 按 FIFO 调用 inner.Write，并串行执行内层刷新与错误查询；AsyncSink 不会让同一个 inner 的 Write / Flush / Err 相互并发。

### Flush 与 Close

| 调用 | 等待范围 | 取消后的结果 |
|---|---|---|
| `Flush(ctx)` | 调用时已入队的快照，包含在途记录，然后刷新内层出口 | 返回 ctx.Err()；记录保留，worker 继续处理 |
| `Close(ctx)` | 先停止接收，再排空、刷新并停止 worker | 首次调用超时确定最终关闭结果；队列剩余计入 Dropped，worker 在能返回内层调用后退出 |

后续写入不会扩大较早 Flush 的等待范围。同目标的等待者共享刷新任务，不同快照按目标顺序登记；未开始且已无等待者的边界在取消后撤销，边界数量至多为容量加一。Flush 不会为每个等待者再开 goroutine。

刷新由 worker 在边界处暂停消费后执行：内层实现 `Flush() error` 时调用它，否则检查可选的 `Err() error`。两者不重复调用。包装 LineSink 时，这一步会真正写出它自己的缓冲；只将记录交给 inner.Write 并不代表内层缓冲已经写出。包装 MultiSink 时，它没有刷新方法，叶子出口仍需由宿主分别刷新。

Close 会唤醒正在等队列空位的写入者，这些记录和关闭后的新 Write 都只计入 Dropped，不 panic。第一次 Close 的 ctx 控制排空和最终结果；结果确定后，重复 Close 返回该结果。结果尚未确定时，并发调用者可以用自己的 ctx 停止等待，但不会改变首次关闭的结果。关闭后的 Flush 等待同一个关闭结果。

ctx 也限制等待内层 Flush / Err 的时间，但不能强制中断没有 context 参数的 inner.Write / Flush / Err。取消等待可以立即返回；已开始的内层调用由原 worker 继续处理。若它始终不返回，该 worker 也无法退出。

### 内层 panic

| 来源 | 处理 |
|---|---|
| inner.Write panic | 捕获、该条计入 Dropped，worker 继续处理后续记录 |
| inner.Flush / Err panic | 返回固定错误 `observe: AsyncSink inner Flush/Err panicked`，不格式化 panic 正文 |

Flush / Err 的 panic 不会把已经交给 inner.Write 的记录计为 Dropped。若 panic payload 是 error，返回错误的 Unwrap 会保留它，可通过 errors.Is / errors.As 检查；固定 Error 文本不调用 payload.Error，即使该方法本身会 panic 也能安全报告。worker 仍可处理后续记录。

进程关闭路径应显式检查 Flush 或 Close；异步队列不会在进程退出后继续存在。需要停止 worker 时使用 Close。

## 自定义行体渲染

`LineRenderer` 的形态为 `func(dst []byte, r Record, color bool) []byte`。通过 `WithRenderer` 安装后，只负责追加行体；前缀、换行、颜色判定、缓冲、写出时机和错误收集仍由 LineSink 负责。

六个导出原语与内置版式使用相同的编码口径：

| 原语 | 用途与边界 |
|---|---|
| `AppendDuration(dst, d)` | 追加带单位耗时；ns、µs、ms、s 按量级选，µs 一位小数，ms / s 两位；零值原语输出 `0ns`，内置缺耗时占位由版式另行处理 |
| `AppendTextValue(dst, text)` | 对空字符串、空格、等号、引号和控制字符按需加 Go 字符串引号；空字符串为 `""`，不是缺值 |
| `AppendAttrs(dst, attrs)` | 按插入序追加 `k=v`，组内单空格分隔，无前导分隔符；空组输出零字节 |
| `AppendAttrsExcept(dst, attrs, keys...)` | 同一编码规则，跳过已放进固定列的键；不存在的键无影响，全被跳过时输出零字节 |
| `AppendPadding(dst, n)` | 追加 n 个空格；n <= 0 不追加 |
| `DisplayWidth(text)` | 计算近似终端显示列数，配合 Padding；常见东亚字符按两列、组合与零宽字符按零列 |

这些原语保证相应片段与内置版式逐字节一致。DisplayWidth 使用常见 Unicode 区段的近似表，罕用宽字符或 emoji 与实际终端可能存在宽度差异。

渲染器在 Sink 的内部锁内调用，必须避免回调该 Sink 的 Write / Flush / Err，也不应阻塞。`Get[T]` 的 ok 位用于区分缺属性与真实零值。Attrs 分隔符由宿主版式添加；使用 AppendAttrsExcept 时，按实际追加长度判定是否为空，不能仅看 Attrs.Len()。单列标量可复用字符串原语或 `strconv.AppendInt` / `AppendFloat(..., 'g', -1, 64)` / `AppendBool`，保持与属性段同形。

完整可运行实现见 [`Example_hostRenderer`](linelog_example_test.go)：

```bash
go test -run '^Example_hostRenderer$' ./observe
```

## 常见问题

| 问题 | 排查与处理 |
|---|---|
| Run 返回了，终端仍没有日志？ | 默认 LineSink 可能尚未达到缓冲阈值；检查 Flush，或使用 WithImmediate |
| 为什么一个普通节点只有两条 Record？ | 三种节点回调用于计时和收尾，Record 通常只有等待结束、执行结束两条 |
| 为什么节点没有业务计时记录？ | 检查输入跳过、切面短路或检查点恢复；参考事件表判断实际阶段 |
| Sink 出错为什么 Run 仍成功？ | Observer 是只读观测边界；检查 LineSink.Err / Flush、AsyncSink.Flush / Close 和 Dropped |
| 怎么查询正在等哪些输入？ | 持有 Execution，读 Snapshot 中节点的 Inputs 和到达状态 |
| 多次运行怎么关联与区分？ | Execution 区分每一轮；TraceID 承担宿主业务关联，不可替代执行身份 |

## 相关资料

- [图观测指南](https://luo-root.github.io/pulse/guide/observability)
- [`ObserveConfig`](config.go)、[`Record / Attrs / Sink`](record.go)、[事件适配器](graph_record.go)
- [LineSink 与编码原语](linelog.go)、[SlogSink / MemorySink](sink.go)、[AsyncSink](async.go)
- [执行快照](../snapshot.go)、[Observer 与 Site](../observer.go)
- [设计说明](../docs/design/pulse.md)中的观测部分
