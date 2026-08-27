# 01-chat

最薄的一层，但验证的是最底下的两块承重墙：kernel 生命周期 和 统一消息词汇表。后面两层全部踩在这里验证过的地基上。

## kernel 在这里做的事

`demoapp.Open` 的装配链是真实的内核使用流程（三个 demo 共用）：

```text
kernel.New()
  -> Use(llm.Plugin())                    # Registry 装载为服务 "pulse.llm"
  -> openai.Register / anthropic.Register # adapter 工厂登记为可逆 Effect
  -> Use(observability.Plugin(...))       # 订阅 before_generate / after_response
  -> reg.Declare("main", cfg)             # 命名实例声明
  -> reg.Open("main")                     # 打开并缓存
```

进程退出时 `host.Close()` → `Dispose` 按 LIFO unwind：观测监听摘除、Registry Close、已打开的模型失效。

「卸载即还原」的回收断言不靠注释代码肉眼观察（进程退出后 OS 回收一切，无法验证），而是由同进程单元测试钉死：`demoapp_test.go` 的 `TestHostCloseReclaimsServices` 在 `host.Close()` 之后断言 `llm.Registry` 与观测 Reporter 的服务绑定均已从仓库消失。

## 消息词汇表：一个输入模型，两种线格式

demo 层只构造 `llm.Part`，从不碰 OpenAI/Anthropic 的 wire format：

| 用户输入 | 构造 | OpenAI Completions 映射 | Anthropic 映射 |
|---|---|---|---|
| 文本 | `llm.Text` | text 块 | text 块 |
| 图片 URL/字节 | `llm.ImageURL` / `ImageData` | image_url | image 块 |
| PDF 字节 | `llm.Media("application/pdf", ...)` | file 块 | document 块 |
| 音频字节 | `llm.Media("audio/wav", ...)` | input_audio（官方块） | **ErrBadRequest** |
| 视频 URL | `llm.MediaURL("video/mp4", ...)` | video_url（兼容网关扩展） | **ErrBadRequest** |

两个关键设计在此落地：

1. **能力矩阵是显式契约**：Anthropic 不吃音频视频时返回分类错误，而不是静默丢弃或假装发送。demo 不在上层 catch 掉这个错误糊弄过去——真机跑 Anthropic + `/file x.wav` 你会看到失败和原因。
2. **多模态从第一层就是一等公民**：REPL 输入经 `Input.Message()` 变成带 Part 的用户消息，与纯文本走完全相同的通路。日志里的 `text_parts/image_parts/custom_parts/inline_media_bytes` 就是这条通路的计量。

Generate 是同步路径；Model 同时暴露 `Stream`，本层未展示（文本增量演示放在 02-react 的 `RunStream`）。

## 运行

```powershell
go run ./examples/01-chat
```

REPL 命令同全局说明；本层不保存 history——每轮独立 Generate，这是刻意收窄的边界（保持最小变量隔离）：01 只回答「消息能不能按统一模型发出去并拿回回复」，02 才负责「多轮」。多模态附件通过 `/image`、`/file` 在会话里现场附加，不再提供环境变量一次性入口。

| 变量 | 含义 |
|---|---|
| `PULSE_DEMO_PROVIDER` | `openai` / `openai-responses` / `anthropic` |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | 真实凭据；缺省走 ScriptedModel |

默认读取仓库根 `.env`。

> 注：这些示例用的兼容网关（如 MiniMax）常见行为——部分字段（reasoning 等）在网关上有自有语义，adapter 只按通用线格式实现，不做网关私有扩展。
