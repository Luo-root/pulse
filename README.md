[English](README_en.md) | [中文](README.md)

<p align="center">
  <img src=".github/assets/logo.svg" alt="Pulse" width="260">
</p>

<p align="center">Go 的数据驱动工作流执行库</p>

<p align="center">
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.25%2B-2563eb" alt="Go 1.25+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-2563eb" alt="MIT license"></a>
  <a href="https://luo-root.github.io/pulse/"><img src="https://img.shields.io/badge/docs-中文%20%7C%20English-2563eb" alt="双语文档"></a>
</p>

Pulse 将一组 Go 函数组合为可复用的工作流。每个节点声明需要读取和产生的数据，执行依赖由这些声明推导：输入到达后，符合条件的节点开始工作。你可以在已有服务、命令行程序或 Agent 应用中使用它，无需运行独立服务。

图定义 `Graph` 与单次运行 `Execution` 分离。同一份定义可以处理不同请求、并发执行，或作为子图在多个位置复用；每次执行都有独立的输入、状态和取消域。核心包只使用 Go 标准库。

## 适用场景

- **多步骤业务处理**：将下载、解析、校验、生成等步骤按数据依赖组合，独立步骤可以并行执行。
- **分支与结果汇聚**：明确表示某一路有结果或正常跳过，按业务选择收集已有结果或要求全部结果。
- **可复用的流程模块**：通过声明端口组合子图，保持内部数据与外部接线分离。
- **连续数据处理**：用受管流逐条发送、确认、广播，跨子图实时传递，并在运行中向宿主交付。
- **需要可观测的执行过程**：查看结构、运行快照与节点事件，定位等待、执行、失败和取消。

需要在进程退出后继续工作时，使用独立的 [pulse-durable](https://github.com/Luo-root/pulse-durable)。它在 Pulse 的检查点桥上提供存储、编码和 Run / Resume；受管流的存储与进程恢复也由 Durable 提供。按 [Durable 安装说明](https://github.com/Luo-root/pulse-durable/blob/main/site/guide/installation.md)使用已验证的依赖组合，发布以实际 tag/release 为准。未确认步骤可能重新执行，外部副作用仍需业务提供幂等或事务。

## 安装

需要 **Go 1.25.0 或更新版本**。

> 本 README 和文档站描述 `main` 的开发版 API，包含受管 Stream、实时 Sub 和增量检查点。最新正式 tag 为 **v0.4.1**，其 API 与本文不同。以下命令安装主线并在 go.mod 固定解析出的伪版本；试用尚未合并的修改时，按[安装与版本](site/guide/installation.md)使用对应本地源码。

在 Go 模块中执行：

```sh
go get github.com/Luo-root/pulse@main
```

## 第一个工作流

下面的完整程序接收一个整数，执行加倍，并从本次执行读取结果。保存为 `main.go` 后运行 `go run .`。

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Luo-root/pulse"
)

func main() {
	input := pulse.NewKey[int]("input")
	output := pulse.NewKey[int]("output")
	g, err := pulse.New("double", "将输入加倍")
	if err != nil {
		log.Fatal(err)
	}
	if err := g.DeclareInput(pulse.NewPort(input)); err != nil {
		log.Fatal(err)
	}
	if err := g.DeclareOutput(pulse.NewPort(output)); err != nil {
		log.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("double", "计算两倍值",
		pulse.Requires(input), pulse.Provides(output),
		func(rc *pulse.RunCtx) error {
			value, err := pulse.Get(rc, input)
			if err != nil {
				return err
			}
			return pulse.Set(rc, output, value*2)
		})); err != nil {
		log.Fatal(err)
	}

	var in pulse.Inputs
	if err := pulse.Seed(&in, input, 21); err != nil {
		log.Fatal(err)
	}
	e, err := g.NewExecution(context.Background(), in)
	if err != nil {
		log.Fatal(err)
	}
	if err := e.Start(); err != nil {
		log.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		log.Fatal(err)
	}
	value, state, err := pulse.ReadOutput(e, output)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(value, state)
}
```

预期输出：

```text
42 ready
```

这里，`Key[T]` 标识一项有类型的数据；`Node` 声明读写权限与处理函数；`Graph` 保存流程定义；`Execution` 保存这一次运行的状态。`DeclareInput` / `DeclareOutput` 定义宿主可用的接口，`Seed` 提供本轮输入，`ReadOutput` 在 `Wait` 收尾后读取声明出口。

只需要等待完成、不需要执行句柄时，可以调用 `g.Run(ctx, in)`。需要查询状态、读取输出或分步控制生命周期时，使用上例的 `NewExecution`、`Start` 和 `Wait`。完整步骤与解释见[快速开始](site/guide/quickstart.md)。

连续数据使用 `Key[pulse.Stream[T]]` 与 `ProduceStream` / `ConsumeStream` / `TeeStream`。受管等待会暂时让出业务额度，显式 Ack 才释放在途容量；[完整流示例](site/guide/streams.md)用一个额度广播到两个消费端，各得到总和 6。需要宿主实时接收时，在 `NewExecution` 后、`Start` 前调用 `SubscribeStream` 并持续确认；`Wait` 也等已登记读取组完成或 Stop。Sub 的实时流出口提前接通，普通值仍等子执行成功后桥回。旧 `Key[<-chan T]` 与 Produce/Consume/Tee 保留原生兼容语义，无消息确认、自动倒带或持久流保证。

## 阅读文档

| 你要完成的事 | 从这里开始 |
| --- | --- |
| 判断是否适合项目，安装并完成第一次运行 | [介绍](site/guide/index.md) · [安装](site/guide/installation.md) · [快速开始](site/guide/quickstart.md) |
| 理解定义、执行、数据与正常跳过 | [核心概念](site/guide/concepts.md) |
| 提供输入、读取结果、编写分支和汇聚 | [输入与输出](site/guide/inputs-outputs.md) · [分支与汇聚](site/guide/branching.md) |
| 配置重试、超时和并发，组合子图或流 | [重试与取消](site/guide/retry-cancellation.md) · [子图](site/guide/subgraphs.md) · [流式处理](site/guide/streams.md) |
| 组合审批、输入决定的批量处理或 Graph 工具循环 | [业务组合](site/guide/composition.md) |
| 用 YAML 定义流程，接入事件和日志 | [YAML 装配](site/guide/assembly.md) · [图观测](site/guide/observability.md) |
| 查阅生命周期、错误、恢复桥与版本差异 | [参考索引](site/reference/index.md) |
| 判断 1–10000 节点的成本，复跑同任务跨引擎对照 | [规模与性能](site/reference/performance.md) · [评测源码与方法](benchmarks/README.md) |

在线阅读：[中文文档](https://luo-root.github.io/pulse/) · [English documentation](https://luo-root.github.io/pulse/en/)。

## 包与能力边界

| 包 | 职责 | 依赖 |
| --- | --- | --- |
| `github.com/Luo-root/pulse` | 定义、执行、数据流、子图、检查点桥与状态视图 | Go 标准库 |
| [`pulse/observe`](observe/README.md) | 将引擎事件转换为结构化记录，接入日志与自定义出口 | `pulse` |
| [`pulse/yaml`](yaml/README.md) | 从 YAML 装配流程，或在不运行代码时分析结构 | `pulse`、`yaml.v3` |

正常跳过表示没有这项结果，整次执行仍可成功。节点的真实错误会取消本次执行，并保留错误原因。`Retry` 隔离失败尝试对 Pulse 输出的写入；数据库、HTTP 请求、文件等外部副作用由业务负责。

切面可用 `rc.Site()` 取得本次执行的只读位置。接入 `observe.NewRecordObserver` 后，现有 `Retry` / `Timeout` 自动产生尝试、真实重试、退避和本层到期记录，五种 Observer 生命周期回调次数不变。自定义切面用 `pulse.NamedAspect("approval", aspect)` 显式登记进出；未配置观察者时不输出日志。身份、阶段与恢复边界见[观测指南](site/guide/observability.md)。

Pulse 负责单进程执行，Durable 提供检查点存储与恢复。宿主可通过切面、业务等待记录与 Cancel/Resume 组合审批等待，也可在节点中循环调用 Graph 工具；具体接线与保证边界见[业务组合](site/guide/composition.md)。内置唤醒服务、任务派发、跨机器领取/租约与故障接管仍是后续方向。设计依据与演进计划保留在 [设计文档](docs/design/pulse.md)、[设计理念](docs/design/philosophy.md) 和 [两仓职责说明](docs/design/workflow-foundation.md) 中。

## 开发与贡献

在仓库根目录执行：

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
```

仓库中的 [Go Examples](examples_test.go) 可通过 `go test -run Example -v .` 运行。中英文章节位于 `site/`，包文档由各包 README 同步生成。参与开发前请阅读 [贡献指南](CONTRIBUTING.md)；安全问题请按 [安全政策](SECURITY.md) 报告。

Pulse 使用 [MIT 许可证](LICENSE)。
