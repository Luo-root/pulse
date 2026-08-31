# 03-hitl

验证 **before_tool_call 上挂载的真实 HITL 审批**：四种策略、会话信任表、fail-closed。前一层（02-react）的 ReAct 循环原样复用——审批不改变循环结构，只是在工具执行前多了一个裁决点。

## 本课依赖

[02-react](../02-react/)：ReAct 循环与 toolset 注册（Risk/Source 元数据在本课成为审批输入）。

## 四种模式：PULSE_DEMO_HITL

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
go run ./examples/03-hitl
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
| `a` | 放行并授予会话级信任：本次进程内该工具不再询问（turn 结束摘要打印 `session_trust=[...]`） |

三个关键语义（也是生产审批系统的最小集）：

1. **裁决在基础设施层，不在模型自觉**。模型「认定用户已口头批准」并发起调用后，仍会被监听器拦下——denylist 模式演示过同一点。
2. **fail-closed**。审批输入不可读（如 EOF）时按拒绝处理，绝不静默放行。
3. **阻塞安全的前提（已在代码里解决）**。REPL 与审批器共享同一个 `LineSource`（单一行缓冲、同一 goroutine 顺序消费），审批时的 `y/n/a` 不会被 REPL 预读缓冲抢走。多 Agent 并发审批是服务化通道的范畴，demo 不伪装支持。

## 实现形态：监听器而非插件

HITL 的实现是挂在**请求 scope** 上的 `kernel.OnWaterfall` 监听器（`demoapp.InstallHITLWithTrust`）+ 会话信任表，不是独立 Plugin：

- 每轮 `reqScope` 与 Agent / Bridge 共用；loop 用 `WaterfallLocal` 派发 `before_tool_call`，所以监听必须装在同一 scope，否则听不到；
- 监听随 `reqScope.Dispose()` 自动摘除；
- `trust` 跨轮传入 `InstallHITLWithTrust`：reqScope 销毁不影响 trust 对象，`a` 授予的会话白名单在后续轮次仍生效。

## 工具与边界

- `lookup`：只读（`RiskReadonly`，放行侧代表）。
- `delete_file`：模拟危险操作（`RiskDangerous`）——handler 只返回 `"deleted"`，**不碰真实文件系统**；审批发生在执行之前。

## 测试

```powershell
go test ./examples/internal/demoapp/ -run HITL -v
```

覆盖：模式解析、denylist 放行/拒绝、allowlist default-deny、interactive 的 y/n/a 三分支、`a` 后同会话自动放行、EOF fail-closed。真人键鼠路径属手动验收：启动后看到横幅 `hitl=interactive` 再触发删除类请求，终端会停在审批提示等待按键（光标停在 `>` 后不是卡死）。

无凭据时 ScriptedModel 回放固定脚本（lookup → delete_file → 总结）；interactive 模式下脚本第二步会真的停下来等按键。

## 下一课

[04-flow](../04-flow/)：编排——把检索管道装进节点图（RAG 线性链 + DAG 分支并行 + YAML 同构）。
