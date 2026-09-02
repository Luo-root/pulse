[English](README.md) | [中文](README_zh.md)

# Pulse

[![Go Version](https://img.shields.io/badge/Go-1.25.0-blue.svg)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

**Pulse** 是一个正在进行 v2 重构的 Go AI Agent 框架。

v2 以可逆效应和依赖响应式为内核，先落地插件内核、模型适配层与无状态 ReAct 回合执行器。v1 的 Agent、旧模型适配器、DAG、记忆、HITL 与遥测实现已彻底移除，后续只会围绕 v2 内核重写，不保留兼容层。

## 当前可用能力

| 包 | 定位 | 从哪里开始 |
|---|---|---|
| [`kernel`](kernel/README_zh.md) | 插件化内核：Context、可逆 Effect、类型服务、事件、Fiber、Loader | `kernel.New()` / `kernel.Use()` |
| [`llm`](llm/README_zh.md) | provider 中立的消息词汇表、请求/流事件、错误分类、模型注册中心 | `llm.NewRegistry()` / `llm.ChatModel` |
| [`llm/openai`](llm/openai/README_zh.md) | OpenAI Chat Completions + Responses 官方 SDK 适配器 | `openai.Register()` |
| [`llm/anthropic`](llm/anthropic/README_zh.md) | Anthropic Messages 官方 SDK 适配器 | `anthropic.Register()` |
| [`loop`](loop/README_zh.md) | 无状态 ReAct 回合执行器，工具调用与 HITL 决策事件 | `loop.NewAgent()` |
| [`toolset`](toolset/README_zh.md) | 可逆工具注册中心（`pulse.tools`），`AsToolSet()` 适配 loop；builtins / mcp / lsp 子包 | `toolset.Plugin()` / `Registry.Register` |
| [`skills`](skills/README_zh.md) | Agent Skills 装载器（agentskills.io；规程包，非 Tool） | `skills.Open()` / `List`/`Load`/`ReadFile` |
| [`textsplit`](textsplit/README_zh.md) | 文本分块：尺寸预算 + 分隔符优先级 + 字节 offset | `textsplit.Split` |
| [`kernel/flow`](kernel/flow/README_zh.md) | 数据就绪驱动的节点编排（槽位三态、Skip、E1 Observer） | `flow.New(ctx)` |
| [`kernel/flow/yaml`](kernel/flow/yaml/README_zh.md) | E2 YAML 声明式装图（拓扑归属 A：Factory 只给 Run） | `flowyaml.Load` |
| [`memory`](memory/README_zh.md) | P2 记忆与会话（9 子包）：session / compaction / store / assemble / selfedit / index / candidate / reflection | `memory/README_zh.md` 全局地图 |
| [`observability`](observability/README_zh.md) | 正式观测包：Bootstrap + Record + Sink（只依赖 kernel） | `observability.Bootstrap()` |
| [`examples`](examples/README.md) | 渐进示例 00–07：kernel 地基 / 装配链+词汇表 / ReAct / HITL / flow 编排 / 会话记忆 / 长期记忆 / 生产集成 | `go run ./examples/00-hello-kernel` |
| [`eval`](eval/README_zh.md) | 评测套件：工程能力 property test + 分层 benchmark + 跨框架内战对比 | `go test -race ./eval/` |

## 快速上手：模型 + ReAct 工具回合

以下示例演示当前 v2 的最短链路。请用环境变量提供 API Key，避免写入代码。

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
        Description: "原样返回参数",
        Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
    }, func(ctx context.Context, args json.RawMessage) (string, error) {
        return string(args), nil
    })

    agent, err := loop.NewAgent(model,
        loop.WithToolSet(tools),
        loop.WithSystemPrompt("你是一个简洁的助手。"),
        loop.WithEventScope(host),
    )
    if err != nil {
        panic(err)
    }

    res, err := agent.Run(context.Background(), nil, llm.UserText("调用 echo 工具，参数 text 是 hello"))
    if err != nil {
        panic(err)
    }
    fmt.Println(res.Final.Text())
}
```

更多模型、流式、多模态、推理参数、能力矩阵与错误处理见 [`llm/README_zh.md`](llm/README_zh.md)；HITL 事件示例见 [`loop/README_zh.md`](loop/README_zh.md)。

## v2 架构

```text
调用方
  │
  ├── kernel.Context
  │     ├── ServiceKey：类型安全服务
  │     ├── Effect：卸载即还原
  │     ├── Event：Emit/Waterfall/Parallel（全树）+ EmitLocal/WaterfallLocal（本 scope）
  │     └── Plugin / Fiber / Loader：依赖响应式装载
  │
  ├── observability.Bootstrap   # 最先 Use；旁路订阅 fiber_state / loader_action
  │
  ├── llm.Registry
  │     └── ChatModel
  │           ├── openai：Chat Completions / Responses
  │           └── anthropic：Messages（MaxTokens 必填）
  │
  ├── loop.Agent（建议挂 reqScope）
  │     ├── 模型推理（llm.WithEventScope → Local）
  │     ├── ToolSet 工具调用
  │     └── before_tool_call WaterfallLocal：HITL 挂载点
  │
  └── kernel/flow（+ flow/yaml）
        ├── Graph：AND / Skip / Observer
        └── YAML 装图：Registry + SeedPlan（装配层执行 IO）
```

设计蓝图与 v1 → v2 的迁移顺序见 [`docs/design/plugin-kernel-v2.md`](docs/design/plugin-kernel-v2.md)；请求级局部事件派发见 [`docs/design/kernel-local-events.md`](docs/design/kernel-local-events.md)。

## 设计边界

- **彻底 breaking**：删除 v1 模型抽象及依赖它的实现；不保留兼容层。
- **词汇表优先**：`llm` 只收跨 provider 有稳定语义的字段；无对应线格式时 adapter 显式 `ErrBadRequest`，不静默吞参数。
- **插件不是口号**：对环境的修改都注册可逆 Effect；服务依赖变化驱动 Fiber 装载 / 卸载。
- **Agent 无状态**：`loop.Agent` 只执行一个回合；历史、会话存储、重试与 failover 由上层或后续 v2 组件承担。
- **v1 components 已删除**：工具 / MCP / 沙箱 / Skill 将作为 v2 插件重写，不复活旧包。

## 构建与测试

```powershell
# 需要 Go 1.25+
go build ./...
go test ./...

# v2 核心回归（无真实 API）
go test -race -skip TestLive ./kernel/... ./llm/... ./loop/ ./toolset/... ./skills/ ./textsplit/... ./memory/... ./observability/ ./examples/04-flow/ ./examples/07-production/

# 单独测试 provider adapter
go test -race -skip TestLive ./llm/openai/
go test -race -skip TestLive ./llm/anthropic/
```

OpenAI / Anthropic / MiMo 的真实 API 冒烟测试由环境变量门控（`PULSE_OPENAI_*` / `PULSE_ANTHROPIC_*` / `PULSE_MIMO_*`）；未设置凭据会自动跳过。MiniMax 经 `PULSE_OPENAI_BASE_URL` 走 OpenAI 兼容通用路径（见 llm/openai README）。不要提交 `.env`、Token 或私钥。

## 仓库结构

```text
kernel/                    v2 插件内核
  flow/                    数据就绪驱动的节点图 + Observer
  flow/yaml/               E2 YAML 声明式装图
llm/                       v2 模型词汇表、Registry 与 provider adapter
loop/                      v2 无状态 ReAct 回合
toolset/                   可逆工具注册（builtins / mcp / lsp 子包）
skills/                    Agent Skills 装载器（agentskills.io）
textsplit/                 文本分块（index/openai 与长文本模块共用）
memory/                    P2 记忆与会话（session / compaction / store / assemble / selfedit / index / candidate / reflection）
observability/             v2 正式观测包（Bootstrap / Record / Sink）
eval/                      评测套件：property test + 分层 benchmark + 内战对比
docs/design/               架构设计与迁移文档（Accepted）
examples/                  00–07 渐进示例 + internal/demoapp 装配层
```

## 许可证

[MIT License](LICENSE)
