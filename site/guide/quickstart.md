# 快速开始

Pulse 是一个 Go AI Agent 框架，v2 内核以预览版（v0.1.0）发布。本页带你跑通最短链路：**插件内核 + 模型适配层 + ReAct 工具回合**。

## 环境要求

- **Go 1.25+**（工具链会在缺失时自动下载）
- 一个 OpenAI 兼容模型的 API Key（走环境变量，不要写进代码）

## 安装

```bash
go get github.com/Luo-root/pulse
```

## 最短链路：模型 + ReAct 工具回合

下面的例子演示 v2 的完整生产装配：kernel 宿主 → llm.Registry（observed 包装）→ 命名模型实例 → MemToolSet 注册工具 → Agent 挂请求 scope。

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/llm/openai"
	"github.com/Luo-root/pulse/loop"
)

func main() {
	host := kernel.New()
	defer host.Dispose()

	reg := llm.NewRegistry(host)
	if err := openai.Register(host, reg); err != nil {
		panic(err)
	}
	if err := reg.Declare("main", llm.Config{
		Provider: openai.ProviderCompletions,
		Model:    "gpt-4o-mini",
		APIKey:   os.Getenv("OPENAI_API_KEY"),
	}); err != nil {
		panic(err)
	}
	model, err := reg.Open("main")
	if err != nil {
		panic(err)
	}

	tools := loop.NewMemToolSet()
	_ = tools.Register(llm.ToolDef{
		Name:        "echo",
		Description: "echoes the arguments back",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	}, func(ctx context.Context, args json.RawMessage) (string, error) {
		return string(args), nil
	})

	agent, err := loop.NewAgent(model,
		loop.WithToolSet(tools),
		loop.WithSystemPrompt("You are a concise assistant."),
		loop.WithEventScope(host),
	)
	if err != nil {
		panic(err)
	}

	res, err := agent.Run(context.Background(), nil, llm.UserText("call the echo tool with text=hello"))
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Final.Text())
}
```

保存为 `main.go`，设置 `OPENAI_API_KEY` 后运行：

```bash
go run ./main.go
```

## 下一步

- **核心概念**：kernel 的 Effect / ServiceKey / 事件与装载模型 → [核心概念](/guide/concepts)
- **编排**：flow 槽位三态节点图与 YAML 声明式装图 → [flow 编排](/guide/flow)
- **记忆**：会话、压缩、长期存储与上下文装配 → [记忆层](/guide/memory)
- **逐包文档**：27 个包的双语完整文档 → [包文档](/packages/)
- **渐进示例**：8 课从 kernel 地基走到生产集成 → [示例](/examples)
