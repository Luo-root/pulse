# Pulse v2 examples

渐进装配示例，对应 [Issue #13](https://github.com/Luo-root/pulse/issues/13)。三层示例逐级叠加，每一层都验证上一层暴露出的装配问题：

| 层 | 新增验证点 | 依赖 |
|---|---|---|
| [01-chat](01-chat/README.md) | `kernel.Context` 装配 `llm.Registry` 与 provider adapter；统一 content-block 输入模型 | kernel、llm、openai/anthropic |
| [02-react](02-react/README.md) | ReAct 回合、工具调用、**四种 HITL 模式（含终端交互审批）**、跨轮 history 归属 | 上一层 + loop |
| [03-flow-agent](03-flow-agent/README.md) | 一次请求的图编排：AND 汇聚、外部 Seed、失败取消、节点级时间统计 | 上一层 + kernel/flow |

## 共享层：examples/internal/

```text
internal/
  demoapp/        启动装配与交互协议
    host.go       dotenv 加载、kernel.New、插件注册、ChatModel 打开
    input.go      多模态输入 -> []llm.Part 的唯一转换点
    repl.go       命令解析 + 交互循环（可单测，不依赖真实 stdin）
  observability/  观测插件原型
```

### demoapp.Host 做了什么

`Open(flags)` 的装配顺序就是一次真实的内核使用流程：

```text
kernel.New()
  -> Use(llm.Plugin())            # Registry 成为服务 pulse.llm，卸载时 Close 全部实例
  -> openai.Register / anthropic.Register   # 工厂登记为可逆 Effect
  -> Use(observability.Plugin())  # 订阅 llm/loop 公开事件，Provide Reporter
  -> reg.Declare("main", cfg)     # 命名实例声明
  -> reg.Open("main")             # 打开并缓存，经过 before_generate 包装
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

全部走 stderr 结构化输出，字段固定：`trace_id`、`layer`（kernel/llm/loop/tool/flow）、`event`、`duration_ms`、`status`，以及模态计数（`text_parts`、`image_parts`、`custom_parts`、`inline_media_bytes`）。

隐私边界：记录**元数据**（次数、字节长度、耗时、状态），不记录 prompt 内容、附件内容、密钥和思维链。

## 当前不做

- 记忆层、会话持久化（history 只活在进程内，退出即丢）
- 真实向量库 / embedding（demo3 用内存关键词检索）
- flow 的 wait/run 时间拆分（现有 seam 只有「切面包裹整段」，见 demo3 文档末尾）
