# memory/compaction

P2-B 压缩层：token meter、§9.1 八步压缩事务、§9.2 tool result deterministic pruning。
核心不变式——**压缩是事务不是删除**：raw log 只增不减，`checkpoint.Replaced` 记录被替代窗口的完整 source refs，失败留审计。设计事实源 §9；实现票 #73。

## 接入

```go
rep, err := compaction.Compact(ctx, sess, compaction.Options{
    Engine:    &compaction.LLMSummarizer{Model: model, ModelName: "gpt-test"},
    Meter:     compaction.CharMeter{},   // nil 用默认 CharMeter
    ModelName: "gpt-test",
    // Window: &[2]int{0, 9},  // 选区；nil = 全量
})
// rep.Replaced      = 被替代窗口的 source event seqs
// rep.CheckpointSeq = compaction.checkpoint 事件的 Seq（新节点的溯源锚点）
```

压缩后 `sess.Surface()` 的对应窗口被替换为单条 `RoleUser` 稳定前缀摘要（事件类型是 `compaction.checkpoint`，**不得伪装 `message.user`**）；session header 的 `FormatVersion` 抬到 2，旧 reader 拒开。

## Meter 与压力

- `CharMeter`：rune 计数 / `CharsPerToken`（默认 4），零 tokenizer 依赖（CGO-free、plan9/js 不锁死）；精确计数由宿主提供自定义 `Meter`。
- `Pressure(meter, surface, threshold)`：压力判定。触发后的**请求级 retry 编排归装配层**，本包只提供检测与手动入口。

## §9.1 事务八步

「八步」是设计文 §9.1 的命名：其中压力检测（步 1，`Pressure`）、选区选择与预检（步 2，`Window` + `ValidateReplace`）与压缩后的 `Flush`（步 8，调用方职责）不产生事件；**落盘的事件/动作按下述 5 项执行**：

`Compact` 的落盘顺序（任一步失败即返回，原始事件不删）：

1. `compaction.started` —— 事务锁（ID + 选区 SourceRefs）
2. Summarize（Engine 调用，可取消/可失败）
3. `compaction.summarized` —— 记录摘要模型、usage、来源
4. `compaction.checkpoint` —— `SurfaceReplace` 替代选区，`Replaced` 完整
5. `compaction.ended` —— 收口

失败语义：Summarize 失败 → started 已落、无 checkpoint、ended 不写——**未闭合 compaction 在日志里保持可见**（恢复不补 ended、不假装完成）。选区预检（`session.ValidateReplace`）失败时零落盘。

## Engine 两实现

| 实现 | 行为 |
|---|---|
| `LLMSummarizer` | 用 `llm.ChatModel` 出摘要；`Model == nil` 报错不静默；usage 传播进 `summarized` 审计 |
| `DeterministicSummarizer` | 无模型 fallback：按序拼接选区文本，结果可复现（测试与降级场景） |

## ValidateReplace：「不新增孤儿」口径

Replace 的 pairing 校验是**「不新增破坏」而非「窗口内自成整组」**——§9.2 pruning 替代单个 result 节点时 call 在窗口外，但替代节点保留同 ToolCallID，配对仍成立（合法）。四条规则：

1. 保留的 call 其 result 落入被删窗口且 replacement 不保留该 ID → 拒（call 悬空）
2. 保留的 result 其 call 落入被删窗口且 replacement 不提供 → 拒（result 孤儿）
3. replacement 的 result 在前后文找不到 call → 拒（新孤儿 result）
4. replacement 的 call 无 result 着落 → 拒（新孤儿 call）

编排预检与 session 的 fold 重放复核同一口径（fail closed）。

## §9.2 Tool Result Pruning

```go
n, checkpoints, err := compaction.PruneResults(ctx, sess, compaction.PruneOptions{})
// 默认 Max 4000 / Head 2400 / Tail 800 rune；head + marker + tail，rune 安全不劈 UTF-8
```

超预算的 tool result 节点逐个 checkpoint Replace（窗口 = 单节点）；结构化字段（ToolCallID/IsError）保留；**原文完整保存在 raw log**（UI 可展开）。确定性操作，幂等。

## 测试

```bash
go test -race -count=1 ./memory/compaction/...
```
