# 01-chat

装配链与统一消息词汇表：在 [00-hello-kernel](../00-hello-kernel/) 的地基上引入 `llm.Registry`（命名模型实例、provider 适配器注册）与完整的多模态 Part 词汇表，并用 REPL 让「会话」首次出现。kernel 生命周期在 00 已验证；从本课起关键实现**亲手写一遍**（01 手写装配链、02 手写 Bridge、03 手写 HITL），之后各课复用 demoapp 封装版才是「已掌握」。

## 本课依赖

[00-hello-kernel](../00-hello-kernel/)：kernel `New/Use/Dispose` 与 `llm.ChatModel` 接口。

## 手写装配链：六步展开

00 课把装配封装进了 demoapp.Open；本课在 main.go 里逐行手写同一条链：

```text
① kernel.New()                                       # 宿主作用域：一切插件与可逆效应挂这里
② Use(observability.Bootstrap(hostID, sink))         # 必须最先：事件不回放，后装只有快照横幅
③ Use(llm.Plugin())                                  # Registry 装载为服务 "pulse.llm"
④ openai.Register / anthropic.Register               # adapter 工厂登记为可逆 Effect
⑤ reg.Declare("main", cfg) -> reg.Open("main")       # 声明（只登记配置）-> 打开（构造并缓存）
⑥ demoapp.InstallAnthropicMaxTokensDefault(reg.EventScope())  # 装配层示范默认值
```

每一步都有明确的「为什么」：

1. **观测最先 Use**——Bootstrap 只能订阅「之后」的事件，后装会错过装配期轨迹（fiber_state / loader_action），只剩状态快照横幅。
2. **Plugin 自带私有子作用域**——Registry 的观测事件挂在那里，不污染宿主 scope；`kernel.Get(host, llm.ServiceKey)` 取注册中心。
3. **Register 是可逆 Effect**——Dispose 时摘除工厂，已打开的模型随之失效（00 课「卸载即还原」在模型层的落地）。回收断言不靠肉眼（进程退出后 OS 回收一切，无法验证），由 `demoapp_test.go` 的 `TestHostCloseReclaimsServices` 钉死：Close 后 Registry 服务消失、Sink 记录数不再增长。
4. **Declare 与 Open 分离**——声明只登记配置（换 provider/模型只改这一处，调用侧零改动），Open 才构造并缓存。Scripted 路径同样走 `RegisterProvider` + `Declare` + `Open` 穿过 observed 包装，让 before_generate / after_response 事件链对脚本路径同样成立。
5. **MaxTokens 默认值**——Anthropic 线格式该字段必填（nil → ErrBadRequest），而 loop 组请求不填。装配层挂 before_generate 仅在空值时注入 4096；挂在 `Registry.EventScope()`（Plugin 私有子作用域）——请求级 scope 的装配是 02 课主题。

demoapp 在本课只剩两样非主线便利：`.env` 自动加载（import 即生效）与 REPL 输入解析（`/image` `/file` 等命令不是本课主题）。02/03 起装配复用 `demoapp.Open` 封装版——那是你在这里亲手写过一遍的东西。

本层**不创建**请求级 `Bridge` / `reqScope`：只验证装配与词汇表，直接 `model.Generate`。运行期 `source=bridge` 记录从 02-react 起才出现；这是刻意收窄，不是漏装。

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
2. **多模态从第一层就是一等公民**：REPL 输入经 `Input.Message()` 变成带 Part 的用户消息，与纯文本走完全相同的通路。

Generate 是同步路径；Model 同时暴露 `Stream`，本层未展示（文本增量演示放在 02-react 的 `RunStream`）。

## 运行

```powershell
go run ./examples/01-chat
```

REPL 命令同全局说明；本层不保存 history——每轮独立 Generate，这是刻意收窄的边界（保持最小变量隔离）：01 只回答「消息能不能按统一模型发出去并拿回回复」，02 才负责「多轮」。多模态附件通过 `/image`、`/file` 在会话里现场附加，不再提供环境变量一次性入口。

## 下一课

[02-react](../02-react/)：ReAct 循环——给模型装上工具，引入 loop.Agent、toolset.Registry 与流式输出。

| 变量 | 含义 |
|---|---|
| `PULSE_DEMO_PROVIDER` | `openai` / `openai-responses` / `anthropic` |
| `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` | 真实凭据；缺省走 ScriptedModel |

默认读取仓库根 `.env`。

> 注：这些示例用的兼容网关（如 MiniMax）常见行为——部分字段（reasoning 等）在网关上有自有语义，adapter 只按通用线格式实现，不做网关私有扩展。
