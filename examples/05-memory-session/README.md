# 05-memory-session

会话真相课：把 02–04 课「进程内的 history 切片」换成 **event-sourced 会话日志**。核心立场（memory 层铁律）：append-only 日志是唯一事实源，`Surface()` 只是投影；**model-visible means logged**——给模型看的必须真实落日志。

本课全部离线（Scripted 摘要模型），无 API Key。

## 本课依赖

[01-chat](../01-chat/)：`llm` 词汇表。无需 loop/toolset——本课聚焦会话层本身。

## 四段演示（对照 main.go）

### ① 内存 session：事件进，投影出

```go
memStore := session.NewMemoryStore()
sess, _ := memStore.Create(ctx, session.SessionHeader{})
sess.Append(ctx, session.EventDraft{
    Type:    session.EventMessageUser,
    Data:    mustJSON(session.MessagePayload{Parts: []llm.Part{llm.Text("find the config")}}),
    Surface: &session.SurfaceIntent{Op: session.SurfaceAppend},
})
```

Append 四条事件（user → assistant(tool call) → tool.result → assistant），`Surface()` fold 出四条消息。**事件是事实，消息是投影**：fold 规则由事件分级 codec registry 决定（`sess.Registry()`）。

### ② JSONL 持久化：Flush → Close → Open

`session.NewJSONLStore(root)` 把日志落成 `{root}/{sessionID}/header.json + events.jsonl + blobs/ + lock`。`Flush`（fsync）保证崩溃只丢 Flush 点之后的；Close 释放文件锁；`store.Open(ctx, id)` 重开——Surface 与关闭前逐字一致，`store.List` 能看到它。

文件锁语义（单写者）与 `.env` 明文边界见 [session README](../../memory/session/README_zh.md)。

### ③ 崩溃恢复：Open 即冷恢复

构造崩溃现场：assistant 发起 tool call（`EventToolCalled`）后、result 落盘前进程死亡 → Close → 重开。Open 冷恢复合成一条 IsError 的闭合 result——**关键：合成事件真实写回日志**（`Events` 里能看到 `interrupted` result），不是只在投影里装装样子。这就是 model-visible means logged，也是「恢复产物必须通过 tool-pairing 校验」的实现（assistant 的 call 与后续 result 成对，OpenAI/Anthropic 都要求）。

### ④ compaction：Surface 换摘要，raw log 不动

```go
meter := compaction.CharMeter{} // rune/4 估算；精确计数由宿主提供自定义 Meter
compaction.Pressure(meter, surface, 1000) // 压力检测
compaction.Compact(ctx, sess, compaction.Options{
    Engine:    &compaction.LLMSummarizer{Model: ..., ModelName: "scripted"},
    Meter:     meter,
    ModelName: "scripted",
})
```

压缩后 `Surface()` 变成一条 checkpoint 摘要（事件类型 `compaction.checkpoint`，**不得伪装 message.user**），而 `Events(ctx, 0)` 里原始事件一个没少（14 条）——`report.Replaced` 记录被替代窗口的完整 source refs，可溯源。`checkpoint seq=13`、重开后压缩形态持久化（header `FormatVersion` 抬 2，旧 reader 拒开）。

压缩失败（Engine 出错）时日志停在未闭合状态、不假装完成——「压缩是事务不是删除」（§9.1 八步）。

## 会话层小结（与后续课程的接口）

- history 的正确形态 = `session` 的事件日志 + `Surface()` 投影——05 之后各课的「history」都从它来。
- tool result 超长的裁剪（head+marker+tail、原文完整保留）是 `compaction.PruneResults`，本课未演示，见 [compaction README](../../memory/compaction/README_zh.md)。
- Fork / 跨 scope 检索隔离等进阶语义见 [session README](../../memory/session/README_zh.md)。

## 运行

```powershell
go run ./examples/05-memory-session
```

## 下一课

[06-memory-agent](../06-memory-agent/)：长期记忆全链路——store → index 召回 → candidate 提炼 → 审批 → assemble 注入。
