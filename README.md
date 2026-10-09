[English](README_en.md) | [中文](README.md)

<div align="center">
  <a href="https://luo-root.github.io/pulse/">
    <img alt="Pulse" src=".github/assets/logo.svg" width="260" />
  </a>
</div>

<div align="center">
  <h3>Go 的一次性图引擎 —— 数据到达即调度，失败显式。</h3>
</div>

<div align="center">
  <a href="https://go.dev/"><img alt="Go 1.25.0" src="https://img.shields.io/badge/Go-1.25.0-blue.svg" /></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-green.svg" /></a>
  <a href="https://github.com/Luo-root/pulse/releases/tag/v0.3.1"><img alt="Release v0.3.1" src="https://img.shields.io/badge/release-v0.3.1-2563eb.svg" /></a>
  <a href="https://luo-root.github.io/pulse/"><img alt="文档" src="https://img.shields.io/badge/docs-%E4%B8%AD%E6%96%87%20%7C%20English-2563eb.svg" /></a>
  <a href="docs/design/pulse.md"><img alt="设计文档" src="https://img.shields.io/badge/设计-pulse.md-2563eb.svg" /></a>
</div>

<br />

**Pulse** 是一个一次性运行的图引擎，外加一层图观测。节点只声明读哪些 Key、写哪些 Key，拓扑由数据的生产与消费隐式形成——没有边对象、没有拓扑排序、没有调度循环。

Pulse 只有三样东西：

| 包 | 是什么 | 依赖 |
|---|---|---|
| `pulse`（根） | 图引擎 | **零**（只用标准库） |
| `pulse/observe` | 图观测：把引擎的 `Observer` 回调折成结构化记录 | `pulse` |
| `pulse/yaml` | 声明式装图：YAML → 图 | `pulse` + `yaml.v3` |

引擎不 import 任何观测包，只暴露一个 `Observer` seam；不需要观测的宿主只 import 根包。

## 安装

```bash
go get github.com/Luo-root/pulse
```

## 快速上手

```go
package main

import (
	"context"
	"fmt"

	"github.com/Luo-root/pulse"
)

var Docs = pulse.NewKey[[]string]("docs")
var Summary = pulse.NewKey[string]("summary")

func main() {
	g, err := pulse.New(context.Background(), "demo")
	if err != nil {
		panic(err)
	}
	_ = pulse.Seed(g, Docs, []string{"a", "b"})

	_ = g.Add(pulse.NewNode("summarize",
		pulse.Requires(Docs),
		pulse.Provides(Summary),
		func(rc *pulse.RunCtx) error {
			docs, err := pulse.Get(rc, Docs)
			if err != nil {
				return err
			}
			return pulse.Set(rc, Summary, fmt.Sprintf("%d docs", len(docs)))
		}))

	if err := g.Run(); err != nil {
		panic(err)
	}
	// Summary 已到达。
}
```

## 一屏看懂设计

- **数据到达即调度。** `Requires` 是 AND 前置：全部输入到达（就绪或跳过）才判门；**到几个收几个**——只要有一条输入真的到了值就执行 `Run`，一条值都没到才跳过自己。
- **槽位三态**：`pending` | `ready(值)` | `skipped`。就绪与跳过**都是到达**——跳过不是失败。分支靠对未选中的 `Provide` 调用 `Skip`（选中的那条照常 `Set`）写出；**两边都要表态**，只 Skip 一边、漏了选中的 `Set`，那条输出会被自动跳过，两条下游都不跑。
- **失败显式。** 节点 error 记录首错并取消整图，**不会**被改写成 `ErrSkipped`。
- **一次运行一个世界。** `Graph` 是模板的一次实例，不是可重跑的容器；复用模板的正确做法是再 `New` 一次——类比 CI/CD 的 workflow 定义被 run 无数遍、每遍一个独立 run 实例。跨运行状态（历史、缓存、会话）归调用方，不归引擎。
- **切面包住「等输入 + 执行」整段**，所以 `Timeout` 能打断还在等数据的节点。
- **装配糖只收名字、不改语义。** `pulse.FanOut` / `pulse.Join` 把「一个输入 → N 个并行实例」与「N 路同类型 → 一束」收进函数签名（`pulse.Keys(...)` + `pulse.Batch[T]`）：每条输入都带**来源名**（`Batch.Items`，缺项也占一行），要整束严格就 `m.WaitAll()`，要单路严格就 `m.Get(k)`（缺这一路回 `*SkipError`、传了没声明的 Key 回 `ErrUndeclared`），两个回调都拿到**本节点自己的 `*RunCtx`**（长任务靠 `rc.Context()` 感知取消）。`pulse.NoValue()` 是**节点级**跳过，与只跳一条输出的 `Skip(rc, key)` 不是一回事；`FanOut` 的 N 个 worker **整批一次校验、一次提交**，装不完就整个失败、图上不留半个 fan-out。糖产出的图与手写 `NewNode` 的图观测记录**逐字段一致**（有等价锚用例钉着）。**编译期锁元素类型与个数，锁不住同类型槽位的顺序**（`Keys(a,b)` 与 `Keys(b,a)` 都编译，实测）——按来源 Key 取不受影响，这条边界写在设计文档里。**接线之外还有三件**：`pulse.Only(rc, k, v)` 一句话写排他分支（写这一条、本节点其余出口全作废——手写漏表态时两条下游会都不跑而整轮仍报成功）；`pulse.Produce` / `pulse.Consume` 把流式生产 / 消费的 channel 创建、关闭责任、取消响应与循环收进一次调用（生产端**活着发完**，消费端用 `select` 而不是 `for range`：后者会在取消之后把缓冲区算完、还报成功）；`pulse.Tee` 把一根流**复制**给 N 个下游（普通值不需要它——一个 `Provides` 加 N 个 `Requires` 本来就是广播；channel 才会被瓜分）。流式三件都在**装配期**校验名额：流的两端必须同时活着。
- **观测只走一个 seam。** 引擎发出图级两条（开始 / 结束，把本轮夹在中间）与每节点至多三条回调；把它们折成记录是 `observe` 的事。

完整设计（编排 + 观测）：[`docs/design/pulse.md`](docs/design/pulse.md)。

## 构建与测试

```bash
go build ./...          # 验证编译
go vet ./...
go test -race -count=1 ./...
```

需要 **Go 1.25.0+**。无 Makefile、无 linter 配置；CI 在每个 PR 与 `main` 推送时跑 build、vet、gofmt 检查与 `-race` 测试。

## 文档

- 指南（中文 / English）：<https://luo-root.github.io/pulse/>
- 设计文档（编排 + 观测）：[`docs/design/pulse.md`](docs/design/pulse.md)
- API 契约写在 godoc 里：每个导出符号都带一条。

## 许可证

MIT —— 见 [LICENSE](LICENSE)。
