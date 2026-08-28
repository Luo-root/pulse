# Pulse v2 examples

渐进装配示例，对应 [Issue #13](https://github.com/Luo-root/pulse/issues/13)。三层示例逐级叠加，每一层都验证上一层暴露出的装配问题：

| 层 | 新增验证点 | 依赖 |
|---|---|---|
| [01-chat](01-chat/README.md) | `kernel.Context` 装配 `llm.Registry` 与 provider adapter；统一 content-block 输入模型 | kernel、llm、openai/anthropic、observability |
| [02-react](02-react/README.md) | ReAct 回合、工具调用、**四种 HITL 模式（含终端交互审批）**、每请求 `reqScope`+Bridge、跨轮 history 归属 | 上一层 + loop |
| [03-flow-agent](03-flow-agent/README.md) | 一次请求的图编排：AND 汇聚、外部 Seed、失败取消、节点级时间统计 | 上一层 + kernel/flow |
| [04-flow-dag](04-flow-dag/README.md) | **并行双召回 + Skip 分支 + AND**；E1 Observer 峰值；E2 YAML 同构装图 | 上一层 + flow/yaml |

## 共享层：examples/internal/

```text
internal/
  demoapp/        启动装配与交互协议 + 请求级观测桥
    host.go       dotenv、kernel.New、Bootstrap 最先 Use、ChatModel 打开
    bridge.go     请求级 Bridge：挂 reqScope，吃 EmitLocal / WaterfallLocal
    hitl.go       四种 HITL 模式（监听挂在与 Agent 相同的 reqScope）
    input.go      多模态输入 -> []llm.Part 的唯一转换点
    repl.go       命令解析 + 交互循环（可单测，不依赖真实 stdin）
```

正式观测包在仓库根 `observability/`（只依赖 kernel）。examples **没有** `internal/observability/` 原型，也不再提供 `Reporter` 服务。

### demoapp.Host 做了什么

`Open(flags)` 的装配顺序就是一次真实的内核使用流程：

```text
kernel.New()
  -> Use(observability.Bootstrap(hostID, sink))  # 必须最先；订阅 fiber_state / loader_action
  -> Use(llm.Plugin())                           # Registry 成为服务 pulse.llm
  -> openai.Register / anthropic.Register        # 工厂登记为可逆 Effect
  -> reg.Declare("main", cfg)                    # 命名实例声明
  -> reg.Open("main")                            # 打开并缓存，经过 before_generate 包装
```

每轮请求（02/03）再：

```text
reqScope := host.Ctx.Derive()
bridge   := host.NewBridge(reqScope)             # TraceID = Host.NewTraceID()
HITL / Agent 都挂同一 reqScope（Local 派发下否则听不到）
```

`host.Close()` 之后，上述注册按 LIFO 全部回收：事件监听摘除、Registry 关闭、已开模型失效。这就是「对环境的修改都登记为 Effect」的直接验证。

### 输入模型

demo 层的用户输入一律先转成 Pulse 的稳定词汇表（`Input.Message()`），从不拼供应商线格式：

```text
Text       -> llm.Text
ImageURL   -> llm.ImageURL
ImageFile  -> 读字节 -> llm.ImageData
MediaURL   -> llm.MediaURL
MediaFile  -> 读字节 -> llm.Media（PDF/音频/视频）
Attachments-> 按 MIME 分派到 image 或 custom 块
```

某个 provider 吃不下某类 MIME 时，报错来自 adapter（显式 `ErrBadRequest`），demo 层不拦截也不伪装。

### REPL 协议

| 输入 | 行为 |
|---|---|
| 普通文字 | 立即发送本轮 |
| `/image <URL\|路径>` | 暂存图片附件 |
| `/file <URL\|路径> [MIME]` | 暂存开放模态附件 |
| `/send` | 发送当前草稿（可以只有媒体没有文字） |
| `/clear` / `/history` / `/help` / `/exit` | 清空 / 查看消息数 / 帮助 / 退出 |

`ParseCommand` 和 `Loop` 都是纯函数式的输入输出接口（`io.Reader`/`io.Writer`），所以多轮行为有单元测试覆盖，不需要人肉验证。stdin 由单一 `LineSource` 持有行缓冲：REPL 主循环与 interactive 审批器共享同一实例，顺序消费、互不抢行。

## 常用环境变量

```powershell
$env:PULSE_DEMO_PROVIDER = "openai"        # openai | openai-responses | anthropic
$env:PULSE_DEMO_HITL = "interactive"       # denylist | interactive | allowlist | off（02-react）
$env:PULSE_DEMO_DENY_TOOL  = "delete_file" # denylist 名单；allowlist 模式忽略
$env:PULSE_DEMO_ALLOW_TOOL = "lookup"      # allowlist 白名单；空则仅 lookup。与 DENY 语义相反，禁止复用
```

凭据走 `.env` 或官方变量（`OPENAI_API_KEY` / `ANTHROPIC_API_KEY`）；两者皆缺省时自动降级 ScriptedModel。

## 运行

启动时自动读取仓库根 `.env`（已被 gitignore）。

```powershell
go run ./examples/01-chat
go run ./examples/02-react
go run ./examples/03-flow-agent
go test  ./examples/internal/...
```

## 观测日志

- **装配期**（三层共用）：`observability.Bootstrap` → `MemorySink` / `SlogSink`，字段见 [`observability/README_zh.md`](../observability/README_zh.md)（`host_id`、`source=kernel`、`event`、Fiber 状态）。
- **运行期**（仅 02 / 03）：每请求 `demoapp.Bridge` 把 llm/loop/flow 事实折进同一 Sink（`source=bridge`，必填 `trace_id`）；token / turn summary 等装不进信封的指标走 slog 附加键。
- **01-chat 刻意没有运行期 Bridge**：本层只验证装配 + 词汇表，直接 `Model.Generate`；llm Local 事件会落到 Registry ctx，但没有请求级桥去听——这是分层收窄，不是漏装。

隐私边界：记录**元数据**（次数、字节长度、耗时、状态），不记录 prompt 内容、附件内容、密钥和思维链。

## 当前不做

- 记忆层、会话持久化（history 只活在进程内，退出即丢）
- 真实向量库 / embedding（demo3 用内存关键词检索）
- flow 的 wait/run 时间拆分（现有 seam 只有「切面包裹整段」，见 demo3 文档末尾）
