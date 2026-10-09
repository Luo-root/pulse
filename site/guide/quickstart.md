# 快速开始

Pulse 是一个**一次性运行的图引擎**，外加一层**图观测**。本页跑通最短链路：**声明 Key → 装节点 → `Run` → 挂观测**。

## 环境要求

- **Go 1.25.0+**（工具链缺失时自动下载）
- 没有别的依赖：引擎只用标准库；本页示例只需要根包

## 安装

```bash
go get github.com/Luo-root/pulse
```

## 一张最小的图

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse"
)

var (
	Docs    = pulse.NewKey[[]string]("docs")
	Summary = pulse.NewKey[string]("summary")
	Report  = pulse.NewKey[string]("report")
)

func main() {
	g, err := pulse.New(context.Background(), "docs-pipeline")
	if err != nil {
		panic(err)
	}

	// Seed：运行前写入外部输入，声明 docs 的来源是宿主。
	_ = pulse.Seed(g, Docs, []string{"pulse 只做编排与观测", "槽位三态：跳过也是到达"})

	// summarize 等 docs 到达后才进入 Run。
	must(g.Add(pulse.NewNode("summarize",
		pulse.Requires(Docs),
		pulse.Provides(Summary),
		func(rc *pulse.RunCtx) error {
			docs, err := pulse.Get(rc, Docs)
			if err != nil {
				return err
			}
			return pulse.Set(rc, Summary, fmt.Sprintf("%d 段 / %d 字", len(docs), len([]rune(strings.Join(docs, "")))))
		})))

	// report 自己拿结果：Graph 没有 Run 之后的公开读槽，产物归调用方。
	var report string
	must(g.Add(pulse.NewNode("report",
		pulse.Requires(Summary),
		pulse.Provides(Report),
		func(rc *pulse.RunCtx) error {
			summary, err := pulse.Get(rc, Summary)
			if err != nil {
				return err
			}
			report = "报告：" + summary
			return pulse.Set(rc, Report, report)
		})))

	if err := g.Run(); err != nil {
		panic(err)
	}
	fmt.Println(report)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
```

保存为 `main.go`，`go run ./main.go`：

```text
报告：2 段 / 24 字
```

这段代码里出现了全部四个基本概念：

| 你写的 | 它是什么 |
|---|---|
| `pulse.NewKey[T]("docs")` | **Key**：类型化数据槽。名字用于诊断与 YAML 对账，类型给编译期安全；同名必须以同一个 `T` 注册 |
| `pulse.Seed(g, Docs, …)` | **Seed**：运行前写入外部输入——seed 与节点都是 Key 的来源，但每个 Key 只允许一个来源（两处都产 → `ErrDuplicateSource`） |
| `pulse.NewNode(id, Requires, Provides, run)` | **Node**：只声明读哪些槽、写哪些槽。它**不声明**下一个节点是谁 |
| `g.Run()` | **Graph**：提交全部节点并阻塞到全部终止；返回首错，**不含跳过** |

## 数据到达即调度

- 节点在**自己的 goroutine** 里阻塞在输入槽上，输入到达即进入 `Run`——你不需要给节点排序，也不存在拓扑排序这一步；
- 拓扑是**隐式的**：谁写 `summary`、谁读 `summary`，依赖就在那里。没有边对象，没有调度循环；
- `Requires` 是 **AND** 前置：全部输入到达（就绪或跳过）才判门——**到几个收几个**，只要有一条真的到了值就执行（一条值都没到才不执行，见[核心概念](/guide/concepts)）。

## 一次运行一个世界

`Graph` 是**模板的一次实例**，不是可重跑的容器。这一条是对外契约，不是实现细节：

- `Start()` 第二次调用返回 `ErrGraphStarted`；
- 图启动后 `Seed` 同样被拒；槽位一旦到达即关闭，不会重开；
- 同一张图要跑第二次，正确做法是再 `New` 一次——就像 CI/CD 的 workflow 定义被 run 无数遍，每遍是一个独立 run。

跨运行的状态（历史、缓存、会话）**归调用方**，引擎不持有。理由见[核心概念](/guide/concepts)。

## 加一层观测：三行

```go
sink := observe.NewLineSink(os.Stdout, observe.WithImmediate())
defer sink.Flush()

obs, err := observe.NewRecordObserver(observe.ObserveConfig{
	Sink:    sink,
	HostID:  "quickstart",
	TraceID: observe.NewTraceID(),
})
g, err := pulse.New(ctx, "docs-pipeline", pulse.WithObserver(obs))
```

加上这三行后，同一张图的真实输出（运行级两条夹住节点四条；时间戳 / trace / 耗时随运行变化）：

```text
PULSE | 2026/10/09 - 10:42:50.372 | running    |         - | pulse.graph_started | source=observe | pulse.graph=docs-pipeline | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | completed  |         - | pulse.node_run_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=docs-pipeline pulse.node=report | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | completed  |         - | pulse.node_run_finished | source=observe | pulse.graph=docs-pipeline pulse.node=report | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | completed  |   15.80ms | pulse.graph_finished | source=observe | pulse.graph=docs-pipeline | host=quickstart | trace=1791513770372309700-d47dc250-1
```

引擎每一轮另有两条运行级回调（开始 / 结束），`observe` 把它们折成**运行级两条**：`pulse.graph_started` 排在任何节点记录之前、`pulse.graph_finished` 排在全部之后，`Status` 是运行终态（跳过是节点级的事实，有节点跳过**不**让整轮变失败）。节点两条是**分段计时**——等待段（等输入花了多久）与执行段（`Run` 花了多久）：等待段的 `Status` 恒为 `running`，`Duration` 是等待耗时（`summarize` 的输入由 `Seed` 预先写入，所以是 `-`）；执行段的 `Status` 是结束原因，`Duration` 是 `Run` 本身的耗时。

这轮两个节点本身都没花时间（节点段全被取整成 `-`），整轮那条却报了 `15.80ms`：运行级的窗口覆盖「提交节点 → 全部终止」，运行时与出口写出的开销都在里面——它的口径与边界差别见[图观测](/guide/observability)。

## 下一步

- **核心概念**：Key / Node / Graph / 槽位三态 / 切面 → [核心概念](/guide/concepts)
- **编排**：分支、汇聚、超时、重试、限流 → [编排](/guide/orchestration)
- **声明式装图**：拓扑归 YAML → [声明式装图](/guide/assembly)
- **图观测**：出口选择、自带列、隐私边界 → [图观测](/guide/observability)
- **逐包 API**：三个包的完整文档（与仓库 README 同源） → [包文档](/packages/)
