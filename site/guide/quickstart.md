# 快速开始

本教程创建一条“文档列表 → 摘要 → 报告”的工作流，并从执行结果读取报告。完成后，你会知道如何声明数据、添加节点、提供输入和运行流程。

## 开始之前

示例使用 Pulse v0.5.2，需要 Go 1.26.0+。安装方法见[安装与版本](installation.md)。

在一个新目录中初始化模块：

```sh
mkdir pulse-quickstart
cd pulse-quickstart
go mod init example.com/pulse-quickstart
go get github.com/Luo-root/pulse@v0.5.2
```

以下所有命令在该目录执行。

## 1. 创建程序

新建 `main.go`，写入完整代码：

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Luo-root/pulse"
)

func main() {
	documents := pulse.NewKey[[]string]("documents")
	summary := pulse.NewKey[string]("summary")
	report := pulse.NewKey[string]("report")

	g, err := pulse.New("documents", "生成文档报告")
	check(err)
	check(g.DeclareInput(pulse.NewPort(documents)))
	check(g.DeclareOutput(pulse.NewPort(report)))

	check(g.Add(pulse.NewNode("summarize", "整理文档列表",
		pulse.Requires(documents), pulse.Provides(summary),
		func(rc *pulse.RunCtx) error {
			docs, err := pulse.Get(rc, documents)
			if err != nil {
				return err
			}
			text := fmt.Sprintf("%s (%d documents)", strings.Join(docs, ", "), len(docs))
			return pulse.Set(rc, summary, text)
		})))

	check(g.Add(pulse.NewNode("format", "生成最终报告",
		pulse.Requires(summary), pulse.Provides(report),
		func(rc *pulse.RunCtx) error {
			text, err := pulse.Get(rc, summary)
			if err != nil {
				return err
			}
			return pulse.Set(rc, report, "Report: "+text)
		})))

	var in pulse.Inputs
	check(pulse.Seed(&in, documents, []string{"alpha", "beta"}))
	e, err := g.NewExecution(context.Background(), in)
	check(err)
	check(e.Start())
	check(e.Wait())

	value, state, err := pulse.ReadOutput(e, report)
	check(err)
	if state != pulse.SlotStateReady {
		log.Fatalf("expected a report, got %s", state)
	}
	fmt.Println(value)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

`check` 仅用于让这个独立程序在错误时报告原因并退出。节点函数里的错误返回给 Pulse，由执行统一处理；在服务中，应把装配和执行错误返回给调用方。

## 2. 运行并检查结果

```sh
go run .
```

预期输出：

```text
Report: alpha, beta (2 documents)
```

数据依赖是 `documents → summarize → summary → format → report`。`format` 只有在 `summary` 到达后才能处理数据；这由 `Requires` / `Provides` 决定，与 `Add` 的先后顺序无关。

## 3. 理解关键步骤

### 声明数据与接口

`NewKey[[]string]("documents")` 创建带类型的键。键的名字用于识别数据，类型决定 `Get` 和 `Set` 的参数。名称相同且类型相同的键表示同一项数据。

`DeclareInput` 声明工作流接受宿主提供的 `documents`；`DeclareOutput` 声明宿主可以读取 `report`。`summary` 仅在内部流动，因此无需声明端口。

### 添加处理节点

`NewNode` 接收节点 ID、描述、输入声明、输出声明和处理函数。处理函数通过 `RunCtx` 读取声明过的输入、写入声明过的输出。图中的一项数据只有一个来源；重复生产同一键会在装配时返回错误。

### 提供这一次的输入

`Seed` 将值写入 `Inputs`，不会修改图定义。下一次请求应创建自己的 `Inputs`，再创建新的 `Execution`。

如果本轮明确没有某项输入，用 `SkipSeed` 表达正常跳过。未提供一个必需入口会在执行创建或启动时被拒绝，不会让节点无限等待。[输入与输出](inputs-outputs.md)说明可选入口与三态结果。

### 创建并完成执行

`NewExecution` 创建执行句柄，`Start` 校验并开始工作，`Wait` 等全部节点结束并确定终态。成功启动会封口整棵图定义；封口后仍可重复执行这份定义，但不能继续添加节点或修改端口。

`ReadOutput` 在 `Wait` 收尾后读取公共出口。先检查错误，再判断状态：`ready` 是值，`skipped` 是正常缺值；可选透传出口还可能保持 `pending`。本示例要求报告存在，所以显式检查了 `ready`。

## 4. 使用不同输入再次运行

保留图定义，创建新的输入与执行即可。下面是可放在上述 `main` 函数末尾的片段：

```go
var another pulse.Inputs
check(pulse.Seed(&another, documents, []string{"gamma"}))
ex, err := g.NewExecution(context.Background(), another)
check(err)
check(ex.Start())
check(ex.Wait())
nextReport, nextState, err := pulse.ReadOutput(ex, report)
check(err)
fmt.Println(nextReport, nextState)
```

新增输出为：

```text
Report: gamma (1 documents) ready
```

这里的第二次执行有自己的槽位、状态和取消域。不要对同一个 Execution 再次启动，也不需要重置 Graph。

## 常见问题

**运行前就报输入或来源错误。** 核对 `DeclareInput`、`Requires` 和 `Seed` 使用的是同名同类型 Key，且该键没有同时由内部节点产生。

**`ReadOutput` 返回未声明错误。** 宿主只能读取声明过的出口。将需要对外提供的键传给 `DeclareOutput`，并在启动前完成声明。

**想查看日志或进度。** 继续阅读[图观测](observability.md)。`Snapshot` 用于查询状态，业务结果仍从声明出口读取。

## 下一步

- [核心概念](concepts.md)：理解定义、执行和数据三态。
- [分支与汇聚](branching.md)：根据条件输出，并选择汇聚策略。
- [组合子图](subgraphs.md)：把完整流程复用为一个步骤。
