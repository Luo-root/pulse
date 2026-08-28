# 02-react

验证 `loop.Agent` 的核心语义：工具回合、多轮 history、以及 **before_tool_call 上挂载的真实 HITL 审批**。前一层（01-chat）的 kernel + Registry 装配在本层原样复用，不重复验证。

本层起每轮请求创建独立 `reqScope` + `Bridge` + `Agent`（`WithEventScope(reqScope)`）；tool / turn / HITL / llm 事件走 `EmitLocal` / `WaterfallLocal`，请求之间不串扰。

## 四种 HITL 模式：PULSE_DEMO_HITL

| 值 | 行为 | 验证什么 |
|---|---|---|
| `denylist`（默认） | 放行一切，仅拒绝 `PULSE_DEMO_DENY_TOOL` 名单工具（默认 `delete_file`） | 策略拦截 + 拒绝原因回传给模型 |
| `interactive` | **每个危险调用暂停，终端询问操作者** | 真实人审闭环 |
| `allowlist` | 默认拒绝，仅 `PULSE_DEMO_ALLOW_TOOL` 白名单放行（逗号分隔；空则仅 `lookup`） | default-deny 安全姿态 |
| `off` | 不装监听器 | 无监听 = 全放行的框架基线 |

两个名单变量严格分开：`DENY_TOOL` 是「拒绝谁」，`ALLOW_TOOL` 是「只许谁」——语义相反，不互相复用。

```powershell
# 真实 HITL：亲手批准或拒绝每一次删除
$env:PULSE_DEMO_HITL = "interactive"
go run ./examples/02-react
```

## interactive 模式发生了什么

当你让模型「删除 /tmp/test.txt」时：

```text
⚠ 审批请求 tool=delete_file args={"path":"/tmp/test.txt"}
  用途: 删除文件（模拟）
  批准? [y]es / [n]o / [a]lways > _
```

| 输入 | 效果 |
|---|---|
| `y` | 只放行这一次；下次同名调用再问 |
| `n` | 拒绝。拒绝原因作为 IsError 工具结果回传，**回合不失败**，模型可自行改道并向你解释 |
| `a` | 放行并授予会话级信任：本次进程内该工具不再询问（turn 结束摘要会打印 `session_trust=[...]`） |

三个关键语义（也是生产审批系统的最小集）：

1. **裁决在基础设施层，不在模型自觉**。模型 `<think>` 里认定“用户已口头批准”并发起调用后，仍会被监听器拦下——上一版 denylist 已经演示过这一点。
2. **fail-closed**。审批输入不可读（如 EOF）时按拒绝处理，绝不静默放行。
3. **阻塞安全的前提（已在代码里解决）**。REPL 与审批器共享同一个 `LineSource`（单一行缓冲、同一 goroutine 顺序消费），审批时输入的 `y/n/a` 不会被 REPL 预读缓冲抢走。多 Agent 并发审批仍是服务化通道的范畴，demo 不伪装支持。

HITL 的实现是挂在**请求 scope** 上的 `kernel.OnWaterfall` 监听器（`demoapp.InstallHITL`）+ 会话信任表，不是独立 Plugin。每轮 `reqScope` 与 Agent / Bridge 共用；loop 用 `WaterfallLocal` 派发 `before_tool_call`，所以监听必须装在同一 scope，否则听不到。监听随 `reqScope.Dispose()` 自动摘除。

## 多轮 history 归属

`Agent` 无状态。REPL 回调里：

```go
res, err := agent.RunStream(ctx, onDelta, history, msg)
history = append(history, msg)            // 本轮用户输入
history = append(history, res.Messages...) // 本轮 assistant/tool 全部产出
```

`/history` 显示当前条数。第二轮问「刚才那个删除请求怎么样了」，模型能从 history 复述——这就是多轮生效的直接证据，也是后续记忆层要接管的准确位置（替换这两个 append，而不是改 loop）。

## 工具与边界

装配走 `toolset.Plugin` + `Registry.Register`，再 `AsToolSet()` 交给 `loop.Agent`（HITL 仍挂 `before_tool_call`，语义不变）。

- `lookup`：只读查询（`RiskReadonly`，放行侧代表）
- `delete_file`：模拟的危险操作（`RiskDangerous`）——handler 只返回字符串 `"deleted"`，**不碰真实文件系统**；审批发生在执行之前，无论文件是否存在

已知边界如实记录：`ToolSet.Execute` 返回 string，工具结果暂不支持多模态回传。

## 测试

```powershell
go test ./examples/internal/demoapp/ -run HITL -v
```

覆盖：模式解析、denylist 放行/拒绝、allowlist default-deny、interactive 的 y/n/a 三分支、`a` 后的同会话自动放行、EOF fail-closed。真人键鼠路径属手动验收项：启动后看到横幅 `hitl=interactive` 再触发删除类请求，终端会停在审批提示处等待按键（光标停在 `>` 后不是卡死）。

无凭据时 ScriptedModel 回放固定脚本（lookup → delete_file → 总结）；注意 interactive 模式下脚本第二步会在终端真的停下来等按键。
