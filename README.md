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
  <a href="https://github.com/Luo-root/pulse/releases/tag/v0.3.0"><img alt="Release v0.3.0" src="https://img.shields.io/badge/release-v0.3.0-2563eb.svg" /></a>
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
- **观测只走一个 seam。** 引擎每节点至多三条回调；把它们折成记录是 `observe` 的事。

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
